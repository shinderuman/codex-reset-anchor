package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/shinderuman/codex-reset-anchor/internal/codex"
	"github.com/shinderuman/codex-reset-anchor/internal/monitor"
)

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
	client := codex.New(cfg.codexPath)
	return monitor.New(cfg.monitorConfig, client, client, log.Default()).Run(ctx)
}
