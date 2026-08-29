package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

const (
	clientName           = "codex-reset-anchor"
	clientTitle          = "Codex Reset Anchor"
	rpcInitializeTimeout = 8 * time.Second
	rpcRequestTimeout    = 15 * time.Second
	rpcShutdownGrace     = 500 * time.Millisecond
)

type Client struct {
	path    string
	version string
}

type rpcMessage struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rateLimitWindow struct {
	UsedPercent       float64 `json:"usedPercent"`
	WindowDurationMin int64   `json:"windowDurationMins"`
	ResetsAt          int64   `json:"resetsAt"`
}

type rateLimitBucket struct {
	LimitID   string           `json:"limitId"`
	LimitName *string          `json:"limitName"`
	Primary   *rateLimitWindow `json:"primary"`
	Secondary *rateLimitWindow `json:"secondary"`
}

type rateLimitsResult struct {
	RateLimits          *rateLimitBucket           `json:"rateLimits"`
	RateLimitsByLimitID map[string]rateLimitBucket `json:"rateLimitsByLimitId"`
}

type appServer struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	responses chan rpcMessage
	errCh     chan error
	done      chan struct{}

	writeMu sync.Mutex
	idMu    sync.Mutex
	nextID  int64

	stateMu   sync.Mutex
	closing   bool
	closeOnce sync.Once

	stderrMu sync.Mutex
	stderr   bytes.Buffer
}

func New(path, version string) *Client {
	return &Client{path: path, version: version}
}

func (c *Client) ReadQuotas(ctx context.Context) (quota.Snapshot, error) {
	server, err := startAppServer(ctx, c.path)
	if err != nil {
		return quota.Snapshot{}, err
	}
	defer server.close()

	if err := server.initialize(ctx, c.version); err != nil {
		return quota.Snapshot{}, err
	}

	return server.readQuotas(ctx)
}

func (c *Client) RunAnchor(ctx context.Context, workDirectory, prompt, model string) error {
	if err := os.MkdirAll(workDirectory, 0o755); err != nil {
		return fmt.Errorf("アンカー実行用ディレクトリを作成できません: %w", err)
	}

	args := []string{
		"--ask-for-approval",
		"never",
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"--sandbox",
		"read-only",
		"--color",
		"never",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.path, args...)
	cmd.Dir = workDirectory
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return fmt.Errorf("codex execが失敗しました: %w", err)
		}
		return fmt.Errorf("codex execが失敗しました: %w\n%s", err, message)
	}

	return nil
}

func startAppServer(ctx context.Context, path string) (*appServer, error) {
	cmd := exec.CommandContext(ctx, path, "-s", "read-only", "-a", "untrusted", "app-server")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("app-serverの標準入力を開けません: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("app-serverの標準出力を開けません: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("app-serverの標準エラーを開けません: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex app-serverを起動できません: %w", err)
	}

	server := &appServer{
		cmd:       cmd,
		stdin:     stdin,
		responses: make(chan rpcMessage, 16),
		errCh:     make(chan error, 1),
		done:      make(chan struct{}),
		nextID:    1,
	}
	go server.readLoop(stdout)
	go server.readStderr(stderr)
	return server, nil
}

func (s *appServer) initialize(ctx context.Context, version string) error {
	params := map[string]any{
		"clientInfo": map[string]string{
			"name":    clientName,
			"title":   clientTitle,
			"version": version,
		},
	}
	if _, err := s.call(ctx, "initialize", params, rpcInitializeTimeout); err != nil {
		return fmt.Errorf("app-serverの初期化に失敗しました: %w", err)
	}
	if err := s.notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized通知を送信できません: %w", err)
	}
	return nil
}

func (s *appServer) readQuotas(ctx context.Context) (quota.Snapshot, error) {
	result, err := s.call(ctx, "account/rateLimits/read", nil, rpcRequestTimeout)
	if err != nil {
		return quota.Snapshot{}, err
	}

	var limits rateLimitsResult
	if err := json.Unmarshal(result, &limits); err != nil {
		return quota.Snapshot{}, fmt.Errorf("レート制限レスポンスを解析できません: %w", err)
	}

	snapshot := selectQuotaSnapshot(limits, time.Now())
	if snapshot.FiveHour == nil && snapshot.Weekly == nil {
		return quota.Snapshot{}, errors.New("5時間または週次相当のレート制限ウィンドウが見つかりません")
	}
	return snapshot, nil
}

func selectQuotaSnapshot(result rateLimitsResult, checkedAt time.Time) quota.Snapshot {
	var selected quota.Snapshot

	consider := func(bucket rateLimitBucket, name string, window *rateLimitWindow) {
		if window == nil {
			return
		}
		candidate := quota.Window{
			LimitID:           bucket.LimitID,
			WindowName:        name,
			UsedPercent:       window.UsedPercent,
			WindowDurationMin: window.WindowDurationMin,
			ResetsAt:          window.ResetsAt,
			CheckedAt:         checkedAt.UnixNano(),
		}

		switch window.WindowDurationMin {
		case quota.FiveHourWindowMinutes:
			if selected.FiveHour == nil || preferCandidate(*selected.FiveHour, candidate) {
				copy := candidate
				selected.FiveHour = &copy
			}
		case quota.WeeklyWindowMinutes:
			if selected.Weekly == nil || preferCandidate(*selected.Weekly, candidate) {
				copy := candidate
				selected.Weekly = &copy
			}
		}
	}

	for limitID, bucket := range result.RateLimitsByLimitID {
		if bucket.LimitID == "" {
			bucket.LimitID = limitID
		}
		consider(bucket, "primary", bucket.Primary)
		consider(bucket, "secondary", bucket.Secondary)
	}
	if result.RateLimits != nil {
		consider(*result.RateLimits, "primary", result.RateLimits.Primary)
		consider(*result.RateLimits, "secondary", result.RateLimits.Secondary)
	}
	return selected
}

func preferCandidate(current, candidate quota.Window) bool {
	return current.LimitID != "codex" && candidate.LimitID == "codex"
}

func (s *appServer) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := s.newID()
	request := map[string]any{"id": id, "method": method}
	if params != nil {
		request["params"] = params
	}
	if err := s.writeJSON(request); err != nil {
		return nil, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		select {
		case <-requestCtx.Done():
			s.close()
			if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("Codex RPCがタイムアウトしました: method=%s timeout=%s%s", method, timeout, s.stderrSuffix())
			}
			return nil, requestCtx.Err()
		case err := <-s.errCh:
			return nil, fmt.Errorf("%w%s", err, s.stderrSuffix())
		case response := <-s.responses:
			if response.ID == nil || *response.ID != id {
				continue
			}
			if response.Error != nil {
				return nil, fmt.Errorf("JSON-RPCエラー: code=%d message=%s", response.Error.Code, response.Error.Message)
			}
			return response.Result, nil
		}
	}
}

func (s *appServer) notify(method string, params any) error {
	return s.writeJSON(map[string]any{"method": method, "params": params})
}

func (s *appServer) writeJSON(value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("JSONを生成できません: %w", err)
	}
	data = append(data, '\n')
	if _, err := s.stdin.Write(data); err != nil {
		return fmt.Errorf("app-serverへ送信できません: %w", err)
	}
	return nil
}

func (s *appServer) readLoop(stdout io.Reader) {
	defer close(s.done)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if message.ID != nil {
			s.responses <- message
		}
	}
	if err := scanner.Err(); err != nil {
		s.reportError(fmt.Errorf("app-serverの出力読取に失敗しました: %w", err))
		return
	}
	if err := s.cmd.Wait(); err != nil && !s.isClosing() {
		s.reportError(fmt.Errorf("app-serverが終了しました: %w", err))
		return
	}
	if !s.isClosing() {
		s.reportError(errors.New("app-serverが終了しました"))
	}
}

func (s *appServer) readStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 16*1024), 256*1024)
	for scanner.Scan() {
		s.stderrMu.Lock()
		if s.stderr.Len() < 64*1024 {
			s.stderr.WriteString(scanner.Text())
			s.stderr.WriteByte('\n')
		}
		s.stderrMu.Unlock()
	}
}

func (s *appServer) newID() int64 {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	id := s.nextID
	s.nextID++
	return id
}

func (s *appServer) reportError(err error) {
	select {
	case s.errCh <- err:
	default:
	}
}

func (s *appServer) isClosing() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closing
}

func (s *appServer) close() {
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		s.closing = true
		s.stateMu.Unlock()
		_ = s.stdin.Close()
		if s.cmd.Process == nil {
			return
		}

		signalProcessGroup(s.cmd, syscall.SIGINT)
		select {
		case <-s.done:
			return
		case <-time.After(rpcShutdownGrace):
		}
		signalProcessGroup(s.cmd, syscall.SIGKILL)
		select {
		case <-s.done:
		case <-time.After(rpcShutdownGrace):
		}
	})
}

func signalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, signal); err != nil {
		_ = cmd.Process.Signal(signal)
	}
}

func (s *appServer) stderrSuffix() string {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	text := strings.TrimSpace(s.stderr.String())
	if text == "" {
		return ""
	}
	return "\ncodex stderr:\n" + text
}
