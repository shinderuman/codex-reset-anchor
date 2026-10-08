package monitor

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
	"github.com/shinderuman/codex-reset-anchor/internal/state"
)

type resetResult struct {
	windows  []quota.RecoveredWindow
	previous quota.Snapshot
	anchor   string
}

func (m *Monitor) processSnapshot(ctx context.Context, current quota.Snapshot) (*resetResult, error) {
	previous, found, err := state.Load(m.config.StatePath)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, state.Save(m.config.StatePath, state.FromSnapshot(current))
	}

	previousSnapshot := quota.Snapshot{FiveHour: previous.FiveHour, Weekly: previous.Weekly}
	observation := quota.Observe(previousSnapshot, current)
	next := state.FromSnapshot(observation.Next)
	if len(observation.Recovered) == 0 {
		return nil, state.Save(m.config.StatePath, next)
	}

	result := &resetResult{windows: observation.Recovered, previous: previousSnapshot, anchor: "skip"}
	if !observation.NeedsAnchor() {
		return result, state.Save(m.config.StatePath, next)
	}

	result.anchor = "err"
	if err := m.anchor.RunAnchor(ctx, filepath.Dir(m.config.StatePath), m.config.Prompt, m.config.AnchorModel, m.config.AnchorTimeout); err != nil {
		return result, fmt.Errorf("アンカー実行に失敗しました: %w", err)
	}
	result.anchor = "ok"
	return result, state.Save(m.config.StatePath, next)
}
