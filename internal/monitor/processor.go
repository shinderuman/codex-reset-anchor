package monitor

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
	"github.com/shinderuman/codex-reset-anchor/internal/state"
)

func (m *Monitor) processSnapshot(ctx context.Context, current quota.Snapshot) error {
	previous, found, err := state.Load(m.config.StatePath)
	if err != nil {
		return err
	}
	if !found {
		return state.Save(m.config.StatePath, state.FromSnapshot(current))
	}

	previousSnapshot := quota.Snapshot{FiveHour: previous.FiveHour, Weekly: previous.Weekly}
	observation := quota.Observe(previousSnapshot, current)
	next := state.FromSnapshot(observation.Next)
	if len(observation.Recovered) == 0 {
		return state.Save(m.config.StatePath, next)
	}

	m.logRecovery(observation.Recovered, previousSnapshot, current)
	if !observation.NeedsAnchor() {
		if err := state.Save(m.config.StatePath, next); err != nil {
			return err
		}
		m.logger.Printf("アンカーをスキップしました: 回復した利用枠はすでに使用されています")
		return nil
	}

	if err := m.anchor.RunAnchor(ctx, filepath.Dir(m.config.StatePath), m.config.Prompt, m.config.AnchorModel, m.config.AnchorTimeout); err != nil {
		return fmt.Errorf("アンカー実行に失敗しました: %w", err)
	}
	if err := state.Save(m.config.StatePath, next); err != nil {
		return err
	}
	m.logger.Printf("アンカー実行が完了しました")
	return nil
}
