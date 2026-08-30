package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/codex"
	"github.com/shinderuman/codex-reset-anchor/internal/quota"
	"github.com/shinderuman/codex-reset-anchor/internal/state"
)

const Version = "0.4.0"

type config struct {
	codexPath     string
	statePath     string
	pollEvery     time.Duration
	prompt        string
	anchorModel   string
	anchorTimeout time.Duration
}

type quotaReader interface {
	ReadQuotas(context.Context) (quota.Snapshot, error)
}

type anchorRunner interface {
	RunAnchor(context.Context, string, string, string, time.Duration) error
}

func Run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("ホームディレクトリを取得できません: %w", err)
	}
	cfg, err := parseConfig(os.Args[1:], home)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := codex.New(cfg.codexPath, Version)
	return run(ctx, cfg, client, client)
}

func parseConfig(args []string, home string) (config, error) {
	cfg := config{}
	flags := flag.NewFlagSet("codex-reset-anchor", flag.ContinueOnError)
	flags.StringVar(&cfg.codexPath, "codex", "codex", "codexコマンドのパス")
	flags.StringVar(&cfg.statePath, "state", filepath.Join(home, ".local", "var", "codex-reset-anchor", "state.json"), "状態ファイルのパス")
	flags.DurationVar(&cfg.pollEvery, "interval", 5*time.Minute, "利用枠の確認間隔")
	flags.StringVar(&cfg.prompt, "prompt", "Reply only: OK", "アンカー用プロンプト")
	flags.StringVar(&cfg.anchorModel, "model", "", "アンカー実行に使うモデル。空ならgpt-5.6-luna")
	flags.DurationVar(&cfg.anchorTimeout, "anchor-timeout", 2*time.Minute, "アンカー実行のタイムアウト")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.pollEvery < time.Minute {
		return config{}, errors.New("確認間隔は1分以上にしてください")
	}
	if cfg.anchorTimeout <= 0 {
		return config{}, errors.New("アンカー実行のタイムアウトは0より大きくしてください")
	}
	return cfg, nil
}

func run(ctx context.Context, cfg config, reader quotaReader, anchor anchorRunner) error {
	poll := func() {
		current, err := reader.ReadQuotas(ctx)
		if err != nil {
			log.Printf("利用枠の取得に失敗しました: %v", err)
			return
		}
		if err := processCurrentSnapshot(ctx, cfg, current, anchor); err != nil {
			log.Printf("利用枠の処理に失敗しました: %v", err)
		}
	}

	poll()
	ticker := time.NewTicker(cfg.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			poll()
		}
	}
}

func processCurrentSnapshot(ctx context.Context, cfg config, current quota.Snapshot, anchor anchorRunner) error {
	previous, found, err := state.Load(cfg.statePath)
	if err != nil {
		return err
	}
	if !found {
		return state.Save(cfg.statePath, state.FromSnapshot(current))
	}

	next := state.Monitor{Version: state.Version}
	recovered := make([]string, 0, 2)
	unused := make([]string, 0, 2)
	alreadyUsed := make([]string, 0, 2)

	fiveHour, recoveredFiveHour := observeOptionalQuota(previous.FiveHour, current.FiveHour)
	next.FiveHour = fiveHour
	if recoveredFiveHour {
		recovered = append(recovered, "5h")
		classifyRecoveredWindow("5h", fiveHour, &unused, &alreadyUsed)
	}
	weekly, recoveredWeekly := observeOptionalQuota(previous.Weekly, current.Weekly)
	next.Weekly = weekly
	if recoveredWeekly {
		recovered = append(recovered, "weekly")
		classifyRecoveredWindow("weekly", weekly, &unused, &alreadyUsed)
	}

	if len(recovered) == 0 {
		return state.Save(cfg.statePath, next)
	}

	log.Printf("利用枠の回復を検知しました: %s", strings.Join(recovered, ","))
	if len(alreadyUsed) > 0 {
		log.Printf("リセット後の利用を検知しました: %s", strings.Join(alreadyUsed, ","))
	}
	if len(unused) == 0 {
		if err := state.Save(cfg.statePath, next); err != nil {
			return err
		}
		log.Printf("アンカーをスキップしました: 回復した利用枠はすでに使用されています")
		return nil
	}

	if err := anchor.RunAnchor(ctx, filepath.Dir(cfg.statePath), cfg.prompt, cfg.anchorModel, cfg.anchorTimeout); err != nil {
		return fmt.Errorf("アンカー実行に失敗しました: %w", err)
	}
	if err := state.Save(cfg.statePath, next); err != nil {
		return err
	}
	log.Printf("アンカー実行が完了しました")
	return nil
}

func classifyRecoveredWindow(name string, window *quota.Window, unused, alreadyUsed *[]string) {
	if window != nil && window.UsedPercent > 0 {
		*alreadyUsed = append(*alreadyUsed, fmt.Sprintf("%s(usedPercent=%.1f%%)", name, window.UsedPercent))
		return
	}
	*unused = append(*unused, name)
}

func observeOptionalQuota(previous, current *quota.Window) (*quota.Window, bool) {
	if current == nil {
		return cloneWindow(previous), false
	}
	if previous == nil {
		return cloneWindow(current), false
	}

	observed := *current
	if quota.SameWindow(*previous, observed) {
		if observed.LimitID == "" {
			observed.LimitID = previous.LimitID
		}
		if observed.ResetsAt <= 0 {
			observed.ResetsAt = previous.ResetsAt
		}
	}
	return &observed, quota.Recovered(*previous, observed)
}

func cloneWindow(window *quota.Window) *quota.Window {
	if window == nil {
		return nil
	}
	copy := *window
	return &copy
}
