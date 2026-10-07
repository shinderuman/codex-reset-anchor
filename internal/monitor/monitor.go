package monitor

import (
	"context"
	"log"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

type Config struct {
	StatePath     string
	PollEvery     time.Duration
	Prompt        string
	AnchorModel   string
	AnchorTimeout time.Duration
}

type QuotaReader interface {
	ReadQuotas(context.Context) (quota.Snapshot, error)
}

type AnchorRunner interface {
	RunAnchor(context.Context, string, string, string, time.Duration) error
}

type Monitor struct {
	config Config
	reader QuotaReader
	anchor AnchorRunner
	logger *log.Logger
}

func New(cfg Config, reader QuotaReader, anchor AnchorRunner, logger *log.Logger) *Monitor {
	return &Monitor{config: cfg, reader: reader, anchor: anchor, logger: logger}
}

func (m *Monitor) Run(ctx context.Context) error {
	m.poll(ctx)
	ticker := time.NewTicker(m.config.PollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.poll(ctx)
		}
	}
}

func (m *Monitor) poll(ctx context.Context) {
	current, err := m.reader.ReadQuotas(ctx)
	if err != nil {
		m.logger.Printf("利用枠の取得に失敗しました: %v", err)
		return
	}
	if err := m.processSnapshot(ctx, current); err != nil {
		m.logger.Printf("利用枠の処理に失敗しました: %v", err)
	}
}
