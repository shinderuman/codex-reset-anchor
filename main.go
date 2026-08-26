package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	clientName                        = "codex-reset-anchor"
	clientTitle                       = "Codex Reset Anchor"
	clientVersion                     = "0.3.0"
	limitResetThresholdPercent        = 1.0
	resetBoundaryEquivalenceTolerance = 2 * time.Minute
	fiveHourWindowMinutes             = int64(300)
	weeklyWindowMinutes               = int64(7 * 24 * 60)
	stateVersion                      = 2
	rpcInitializeTimeout              = 8 * time.Second
	rpcRequestTimeout                 = 15 * time.Second
	rpcShutdownGrace                  = 500 * time.Millisecond
)

type config struct {
	codexPath   string
	statePath   string
	pollEvery   time.Duration
	prompt      string
	anchorModel string
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

type resetDetectorState struct {
	WasAboveThreshold bool  `json:"wasAboveThreshold"`
	ResetBoundary     int64 `json:"resetBoundary"`
}

type quotaState struct {
	LimitID           string              `json:"limitId"`
	WindowName        string              `json:"windowName"`
	UsedPercent       float64             `json:"usedPercent"`
	WindowDurationMin int64               `json:"windowDurationMins"`
	ResetsAt          int64               `json:"resetsAt"`
	CheckedAt         int64               `json:"checkedAt"`
	ResetDetector     *resetDetectorState `json:"resetDetector,omitempty"`
}

type quotaSnapshot struct {
	FiveHour *quotaState
	Weekly   *quotaState
}

type monitorState struct {
	Version  int         `json:"version"`
	FiveHour *quotaState `json:"fiveHour,omitempty"`
	Weekly   *quotaState `json:"weekly,omitempty"`
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

func main() {
	cfg, err := parseConfig()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func parseConfig() (config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return config{}, fmt.Errorf("ホームディレクトリを取得できません: %w", err)
	}

	cfg := config{}
	flag.StringVar(&cfg.codexPath, "codex", "codex", "codexコマンドのパス")
	flag.StringVar(
		&cfg.statePath,
		"state",
		filepath.Join(home, ".local", "var", clientName, "state.json"),
		"状態ファイルのパス",
	)
	flag.DurationVar(&cfg.pollEvery, "interval", 5*time.Minute, "利用枠の確認間隔")
	flag.StringVar(&cfg.prompt, "prompt", "Reply only: OK", "アンカー用プロンプト")
	flag.StringVar(&cfg.anchorModel, "model", "", "アンカー実行に使うモデル。空ならCodex既定値")
	flag.Parse()

	if cfg.pollEvery < time.Minute {
		return config{}, errors.New("確認間隔は1分以上にしてください")
	}

	return cfg, nil
}

func run(ctx context.Context, cfg config) error {
	poll := func() {
		quotas, err := readQuotasOnce(ctx, cfg.codexPath)
		if err != nil {
			log.Printf("利用枠の取得に失敗しました: %v", err)
			return
		}

		if err := processCurrentSnapshot(ctx, cfg, quotas); err != nil {
			log.Printf("利用枠の処理に失敗しました: %v", err)
		}
	}

	poll()

	ticker := time.NewTicker(cfg.pollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			poll()
		}
	}
}

func readQuotasOnce(ctx context.Context, codexPath string) (quotaSnapshot, error) {
	server, err := startAppServer(ctx, codexPath)
	if err != nil {
		return quotaSnapshot{}, err
	}
	defer server.close()

	if err := server.initialize(ctx); err != nil {
		return quotaSnapshot{}, err
	}

	return server.readQuotas(ctx)
}

func startAppServer(ctx context.Context, codexPath string) (*appServer, error) {
	cmd := exec.CommandContext(
		ctx,
		codexPath,
		"-s",
		"read-only",
		"-a",
		"untrusted",
		"app-server",
	)
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

func (s *appServer) initialize(ctx context.Context) error {
	params := map[string]any{
		"clientInfo": map[string]string{
			"name":    clientName,
			"title":   clientTitle,
			"version": clientVersion,
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

func (s *appServer) readQuotas(ctx context.Context) (quotaSnapshot, error) {
	result, err := s.call(ctx, "account/rateLimits/read", nil, rpcRequestTimeout)
	if err != nil {
		return quotaSnapshot{}, err
	}

	var limits rateLimitsResult
	if err := json.Unmarshal(result, &limits); err != nil {
		return quotaSnapshot{}, fmt.Errorf("レート制限レスポンスを解析できません: %w", err)
	}

	quotas := selectQuotaSnapshot(limits)
	if quotas.FiveHour == nil && quotas.Weekly == nil {
		return quotaSnapshot{}, errors.New("5時間または週次相当のレート制限ウィンドウが見つかりません")
	}

	return quotas, nil
}

func (s *appServer) call(
	ctx context.Context,
	method string,
	params any,
	timeout time.Duration,
) (json.RawMessage, error) {
	id := s.newID()
	request := map[string]any{
		"id":     id,
		"method": method,
	}
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
				return nil, fmt.Errorf(
					"Codex RPCがタイムアウトしました: method=%s timeout=%s%s",
					method,
					timeout,
					s.stderrSuffix(),
				)
			}
			return nil, requestCtx.Err()

		case err := <-s.errCh:
			return nil, fmt.Errorf("%w%s", err, s.stderrSuffix())

		case response := <-s.responses:
			if response.ID == nil || *response.ID != id {
				continue
			}
			if response.Error != nil {
				return nil, fmt.Errorf(
					"JSON-RPCエラー: code=%d message=%s",
					response.Error.Code,
					response.Error.Message,
				)
			}
			return response.Result, nil
		}
	}
}

func (s *appServer) notify(method string, params any) error {
	return s.writeJSON(map[string]any{
		"method": method,
		"params": params,
	})
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

func (s *appServer) markClosing() {
	s.stateMu.Lock()
	s.closing = true
	s.stateMu.Unlock()
}

func (s *appServer) close() {
	s.closeOnce.Do(func() {
		s.markClosing()
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

func selectQuotaSnapshot(result rateLimitsResult) quotaSnapshot {
	var selected quotaSnapshot
	checkedAt := time.Now().UnixNano()

	consider := func(bucket rateLimitBucket, name string, window *rateLimitWindow) {
		if window == nil {
			return
		}

		candidate := quotaState{
			LimitID:           bucket.LimitID,
			WindowName:        name,
			UsedPercent:       window.UsedPercent,
			WindowDurationMin: window.WindowDurationMin,
			ResetsAt:          window.ResetsAt,
			CheckedAt:         checkedAt,
		}

		if window.WindowDurationMin == fiveHourWindowMinutes {
			if selected.FiveHour == nil || preferCandidate(*selected.FiveHour, candidate) {
				copy := candidate
				selected.FiveHour = &copy
			}
		}

		if window.WindowDurationMin == weeklyWindowMinutes {
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

func preferCandidate(current, candidate quotaState) bool {
	return current.LimitID != "codex" && candidate.LimitID == "codex"
}

func processCurrentSnapshot(ctx context.Context, cfg config, current quotaSnapshot) error {
	previous, found, err := loadMonitorState(cfg.statePath)
	if err != nil {
		return err
	}
	if !found {
		initial := monitorState{Version: stateVersion}
		if current.FiveHour != nil {
			value := initializeResetDetector(*current.FiveHour)
			initial.FiveHour = &value
		}
		if current.Weekly != nil {
			value := initializeResetDetector(*current.Weekly)
			initial.Weekly = &value
		}
		return saveMonitorState(cfg.statePath, initial)
	}

	next := monitorState{Version: stateVersion}
	recovered := make([]string, 0, 2)

	var fiveHourRecovered bool
	next.FiveHour, fiveHourRecovered = observeOptionalQuota(previous.FiveHour, current.FiveHour)
	if fiveHourRecovered {
		recovered = append(recovered, "5h")
	}

	var weeklyRecovered bool
	next.Weekly, weeklyRecovered = observeOptionalQuota(previous.Weekly, current.Weekly)
	if weeklyRecovered {
		recovered = append(recovered, "weekly")
	}

	if len(recovered) == 0 {
		return saveMonitorState(cfg.statePath, next)
	}

	log.Printf("利用枠の回復を検知しました: %s", strings.Join(recovered, ","))
	if err := runAnchor(ctx, cfg); err != nil {
		return fmt.Errorf("アンカー実行に失敗しました: %w", err)
	}

	now := time.Now().UnixNano()
	if next.FiveHour != nil {
		armAfterAnchor(next.FiveHour, now)
	}
	if next.Weekly != nil {
		armAfterAnchor(next.Weekly, now)
	}

	if err := saveMonitorState(cfg.statePath, next); err != nil {
		return err
	}

	log.Printf("アンカー実行が完了しました")
	return nil
}

func observeOptionalQuota(previous, current *quotaState) (*quotaState, bool) {
	if current == nil {
		if previous == nil {
			return nil, false
		}
		copy := *previous
		return &copy, false
	}
	if previous == nil {
		value := initializeResetDetector(*current)
		return &value, false
	}

	next, recovered := observeQuota(*previous, *current)
	return &next, recovered
}

func armAfterAnchor(state *quotaState, checkedAt int64) {
	state.ResetDetector = &resetDetectorState{
		WasAboveThreshold: true,
		ResetBoundary:     state.ResetsAt,
	}
	state.CheckedAt = checkedAt
}

func observeQuota(previous, current quotaState) (quotaState, bool) {
	if previous.LimitID != current.LimitID ||
		previous.WindowDurationMin != current.WindowDurationMin {
		return initializeResetDetector(current), false
	}

	if current.CheckedAt <= previous.CheckedAt {
		return previous, false
	}

	previousDetector := detectorFromState(previous)
	currentAboveThreshold := current.UsedPercent > limitResetThresholdPercent
	crossedBelowThreshold := previousDetector.WasAboveThreshold && !currentAboveThreshold
	boundaryAdvanced := resetBoundaryAdvanced(previousDetector.ResetBoundary, current.ResetsAt)
	recovered := crossedBelowThreshold && boundaryAdvanced
	suppressedCrossing := crossedBelowThreshold && !boundaryAdvanced

	nextBoundary := current.ResetsAt
	if !boundaryAdvanced && previousDetector.ResetBoundary > 0 {
		nextBoundary = previousDetector.ResetBoundary
	}

	nextAboveThreshold := currentAboveThreshold
	if suppressedCrossing {
		nextAboveThreshold = true
	}

	current.ResetDetector = &resetDetectorState{
		WasAboveThreshold: nextAboveThreshold,
		ResetBoundary:     nextBoundary,
	}

	return current, recovered
}

func initializeResetDetector(state quotaState) quotaState {
	state.ResetDetector = &resetDetectorState{
		WasAboveThreshold: state.UsedPercent > limitResetThresholdPercent,
		ResetBoundary:     state.ResetsAt,
	}
	return state
}

func detectorFromState(state quotaState) resetDetectorState {
	if state.ResetDetector != nil {
		return *state.ResetDetector
	}

	return resetDetectorState{
		WasAboveThreshold: state.UsedPercent > limitResetThresholdPercent,
		ResetBoundary:     state.ResetsAt,
	}
}

func resetBoundaryAdvanced(previous, current int64) bool {
	if previous <= 0 || current <= 0 || current <= previous {
		return false
	}

	return time.Duration(current-previous)*time.Second >=
		resetBoundaryEquivalenceTolerance
}

func runAnchor(ctx context.Context, cfg config) error {
	workDirectory := filepath.Dir(cfg.statePath)
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
	if cfg.anchorModel != "" {
		args = append(args, "--model", cfg.anchorModel)
	}
	args = append(args, cfg.prompt)

	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, cfg.codexPath, args...)
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

func loadMonitorState(path string) (monitorState, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return monitorState{}, false, nil
	}
	if err != nil {
		return monitorState{}, false, fmt.Errorf("状態ファイルを読めません: %w", err)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return monitorState{}, false, fmt.Errorf("状態ファイルを解析できません: %w", err)
	}

	if _, ok := keys["limitId"]; ok {
		var legacy quotaState
		if err := json.Unmarshal(data, &legacy); err != nil {
			return monitorState{}, false, fmt.Errorf("旧状態ファイルを解析できません: %w", err)
		}
		state := monitorState{Version: stateVersion}
		switch {
		case legacy.WindowDurationMin == fiveHourWindowMinutes:
			state.FiveHour = &legacy
		case legacy.WindowDurationMin == weeklyWindowMinutes:
			state.Weekly = &legacy
		default:
			state.Weekly = &legacy
		}
		return state, true, nil
	}

	var state monitorState
	if err := json.Unmarshal(data, &state); err != nil {
		return monitorState{}, false, fmt.Errorf("状態ファイルを解析できません: %w", err)
	}
	if state.Version != stateVersion {
		return monitorState{}, false, fmt.Errorf("未対応の状態ファイルversionです: %d", state.Version)
	}
	return state, true, nil
}

func saveMonitorState(path string, state monitorState) error {
	state.Version = stateVersion
	return saveJSONAtomic(path, state)
}

func saveJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("状態ディレクトリを作成できません: %w", err)
	}

	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("状態JSONを生成できません: %w", err)
	}
	data = append(data, '\n')

	temporaryPath := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(temporaryPath, data, 0o600); err != nil {
		return fmt.Errorf("一時状態ファイルを書けません: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("状態ファイルを更新できません: %w", err)
	}
	return nil
}
