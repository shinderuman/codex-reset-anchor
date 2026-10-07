package app

import (
	"errors"
	"flag"
	"path/filepath"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/monitor"
)

type config struct {
	codexPath     string
	monitorConfig monitor.Config
}

func parseConfig(args []string, home string) (config, error) {
	cfg := config{}
	flags := flag.NewFlagSet("codex-reset-anchor", flag.ContinueOnError)
	flags.StringVar(&cfg.codexPath, "codex", "codex", "codexコマンドのパス")
	flags.StringVar(&cfg.monitorConfig.StatePath, "state", filepath.Join(home, ".local", "var", "codex-reset-anchor", "state.json"), "状態ファイルのパス")
	flags.DurationVar(&cfg.monitorConfig.PollEvery, "interval", 5*time.Minute, "利用枠の確認間隔")
	flags.StringVar(&cfg.monitorConfig.Prompt, "prompt", "Reply only: OK", "アンカー用プロンプト")
	flags.StringVar(&cfg.monitorConfig.AnchorModel, "model", "", "アンカー実行に使うモデル。空ならgpt-5.6-luna")
	flags.DurationVar(&cfg.monitorConfig.AnchorTimeout, "anchor-timeout", 2*time.Minute, "アンカー実行のタイムアウト")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.monitorConfig.PollEvery < time.Minute {
		return config{}, errors.New("確認間隔は1分以上にしてください")
	}
	if cfg.monitorConfig.AnchorTimeout <= 0 {
		return config{}, errors.New("アンカー実行のタイムアウトは0より大きくしてください")
	}
	return cfg, nil
}
