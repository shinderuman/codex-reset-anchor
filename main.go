package main

import (
	"bufio"
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
	"sync"
	"syscall"
	"time"
)

const (
	clientName              = "codex-reset-anchor"
	clientTitle             = "Codex Reset Anchor"
	clientVersion           = "0.1.0"
	minimumUsageDropPercent = 5.0
	minimumWeeklyWindow     = 24 * time.Hour
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

type rateLimitsUpdatedParams struct {
	RateLimits rateLimitBucket `json:"rateLimits"`
}

type quotaState struct {
	LimitID           string  `json:"limitId"`
	WindowName        string  `json:"windowName"`
	UsedPercent       float64 `json:"usedPercent"`
	WindowDurationMin int64   `json:"windowDurationMins"`
	ResetsAt          int64   `json:"resetsAt"`
	CheckedAt         int64   `json:"checkedAt"`
}

type appServer struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	responses chan rpcMessage
	notices   chan rpcMessage
	errCh     chan error

	writeMu sync.Mutex
	idMu    sync.Mutex
	nextID  int64
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
	flag.StringVar(&cfg.prompt, "prompt", "Reply with only OK.", "アンカー用プロンプト")
	flag.StringVar(&cfg.anchorModel, "model", "", "アンカー実行に使うモデル。空ならCodex既定値")
	flag.Parse()

	if cfg.pollEvery < time.Minute {
		return config{}, errors.New("確認間隔は1分以上にしてください")
	}

	return cfg, nil
}

func run(ctx context.Context, cfg config) error {
	server, err := startAppServer(ctx, cfg.codexPath)
	if err != nil {
		return err
	}
	defer server.close()

	if err := server.initialize(ctx); err != nil {
		return err
	}

	current, err := server.readWeeklyQuota(ctx)
	if err != nil {
		return err
	}

	previous, found, err := loadState(cfg.statePath)
	if err != nil {
		return err
	}
	if !found {
		log.Printf("初期状態を保存します: %s", formatState(current))
		if err := saveState(cfg.statePath, current); err != nil {
			return err
		}
	} else if err := processQuotaChange(ctx, cfg, previous, current); err != nil {
		return err
	}

	ticker := time.NewTicker(cfg.pollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-server.errCh:
			return err
		case notice := <-server.notices:
			if notice.Method != "account/rateLimits/updated" {
				continue
			}

			quota, ok := weeklyQuotaFromNotification(notice)
			if !ok {
				continue
			}
			if err := processCurrentState(ctx, cfg, quota); err != nil {
				log.Printf("通知の処理に失敗しました: %v", err)
			}
		case <-ticker.C:
			quota, err := server.readWeeklyQuota(ctx)
			if err != nil {
				log.Printf("利用枠の取得に失敗しました: %v", err)
				continue
			}
			if err := processCurrentState(ctx, cfg, quota); err != nil {
				log.Printf("利用枠の処理に失敗しました: %v", err)
			}
		}
	}
}

func startAppServer(ctx context.Context, codexPath string) (*appServer, error) {
	cmd := exec.CommandContext(ctx, codexPath, "app-server")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("app-serverの標準入力を開けません: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("app-serverの標準出力を開けません: %w", err)
	}

	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex app-serverを起動できません: %w", err)
	}

	server := &appServer{
		cmd:       cmd,
		stdin:     stdin,
		responses: make(chan rpcMessage, 16),
		notices:   make(chan rpcMessage, 32),
		errCh:     make(chan error, 1),
		nextID:    1,
	}

	go server.readLoop(stdout)
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

	if _, err := s.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("app-serverの初期化に失敗しました: %w", err)
	}
	if err := s.notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized通知を送信できません: %w", err)
	}

	return nil
}

func (s *appServer) readWeeklyQuota(ctx context.Context) (quotaState, error) {
	result, err := s.call(ctx, "account/rateLimits/read", nil)
	if err != nil {
		return quotaState{}, err
	}

	var limits rateLimitsResult
	if err := json.Unmarshal(result, &limits); err != nil {
		return quotaState{}, fmt.Errorf("レート制限レスポンスを解析できません: %w", err)
	}

	quota, ok := selectWeeklyQuota(limits)
	if !ok {
		return quotaState{}, errors.New("週次相当のレート制限ウィンドウが見つかりません")
	}

	return quota, nil
}

func (s *appServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
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

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-s.errCh:
			return nil, err
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
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			log.Printf("app-serverから不正なJSONを受信しました: %v", err)
			continue
		}

		if message.ID != nil {
			s.responses <- message
			continue
		}
		s.notices <- message
	}

	if err := scanner.Err(); err != nil {
		s.reportError(fmt.Errorf("app-serverの出力読取に失敗しました: %w", err))
		return
	}

	if err := s.cmd.Wait(); err != nil {
		s.reportError(fmt.Errorf("app-serverが終了しました: %w", err))
		return
	}
	s.reportError(errors.New("app-serverが終了しました"))
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

func (s *appServer) close() {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
	}
}

func selectWeeklyQuota(result rateLimitsResult) (quotaState, bool) {
	var selected quotaState
	found := false

	consider := func(bucket rateLimitBucket, name string, window *rateLimitWindow) {
		if window == nil {
			return
		}
		if time.Duration(window.WindowDurationMin)*time.Minute < minimumWeeklyWindow {
			return
		}
		if found && window.WindowDurationMin <= selected.WindowDurationMin {
			return
		}

		selected = quotaState{
			LimitID:           bucket.LimitID,
			WindowName:        name,
			UsedPercent:       window.UsedPercent,
			WindowDurationMin: window.WindowDurationMin,
			ResetsAt:          window.ResetsAt,
			CheckedAt:         time.Now().Unix(),
		}
		found = true
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

	return selected, found
}

func weeklyQuotaFromNotification(message rpcMessage) (quotaState, bool) {
	var params rateLimitsUpdatedParams
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return quotaState{}, false
	}

	result := rateLimitsResult{RateLimits: &params.RateLimits}
	return selectWeeklyQuota(result)
}

func processCurrentState(ctx context.Context, cfg config, current quotaState) error {
	previous, found, err := loadState(cfg.statePath)
	if err != nil {
		return err
	}
	if !found {
		return saveState(cfg.statePath, current)
	}

	return processQuotaChange(ctx, cfg, previous, current)
}

func processQuotaChange(ctx context.Context, cfg config, previous, current quotaState) error {
	if !quotaRecovered(previous, current) {
		return saveState(cfg.statePath, current)
	}

	log.Printf("利用枠の回復を検知しました: %s -> %s", formatState(previous), formatState(current))
	if err := runAnchor(ctx, cfg); err != nil {
		return fmt.Errorf("アンカー実行に失敗しました: %w", err)
	}

	current.CheckedAt = time.Now().Unix()
	if err := saveState(cfg.statePath, current); err != nil {
		return err
	}

	log.Printf("アンカー実行が完了しました")
	return nil
}

func quotaRecovered(previous, current quotaState) bool {
	if previous.LimitID != current.LimitID || previous.WindowName != current.WindowName {
		return false
	}

	usageDropped := previous.UsedPercent-current.UsedPercent >= minimumUsageDropPercent
	resetMovedForward := current.ResetsAt > previous.ResetsAt && current.UsedPercent <= previous.UsedPercent
	return usageDropped || resetMovedForward
}

func runAnchor(ctx context.Context, cfg config) error {
	args := []string{
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"--sandbox", "read-only",
		"--ask-for-approval", "never",
	}
	if cfg.anchorModel != "" {
		args = append(args, "--model", cfg.anchorModel)
	}
	args = append(args, cfg.prompt)

	cmd := exec.CommandContext(ctx, cfg.codexPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex execが失敗しました: %w", err)
	}
	return nil
}

func loadState(path string) (quotaState, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return quotaState{}, false, nil
	}
	if err != nil {
		return quotaState{}, false, fmt.Errorf("状態ファイルを読めません: %w", err)
	}

	var state quotaState
	if err := json.Unmarshal(data, &state); err != nil {
		return quotaState{}, false, fmt.Errorf("状態ファイルを解析できません: %w", err)
	}
	return state, true, nil
}

func saveState(path string, state quotaState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("状態ディレクトリを作成できません: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
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

func formatState(state quotaState) string {
	resetTime := time.Unix(state.ResetsAt, 0).Local().Format("2006-01-02 15:04:05")
	return fmt.Sprintf(
		"limit=%s window=%s duration=%dm used=%.1f%% reset=%s",
		state.LimitID,
		state.WindowName,
		state.WindowDurationMin,
		state.UsedPercent,
		resetTime,
	)
}
