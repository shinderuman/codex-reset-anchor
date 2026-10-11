package monitor

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
	"github.com/shinderuman/codex-reset-anchor/internal/state"
)

func (m *Monitor) processSnapshot(ctx context.Context, current quota.Snapshot) (string, error) {
	previous, found, err := state.Load(m.config.StatePath)
	if err != nil {
		return "", err
	}
	if !found {
		return "", state.Save(m.config.StatePath, current)
	}

	fiveHourChanged := quota.Changed(previous.FiveHour, current.FiveHour)
	weeklyChanged := quota.Changed(previous.Weekly, current.Weekly)
	var changed []string
	if fiveHourChanged {
		changed = append(changed, "5h")
	}
	if weeklyChanged {
		changed = append(changed, "weekly")
	}
	if len(changed) == 0 {
		return "", state.Save(m.config.StatePath, current)
	}

	status := "skip"
	if (fiveHourChanged && current.FiveHour.UsedPercent == 0) || (weeklyChanged && current.Weekly.UsedPercent == 0) {
		if err := m.anchor.RunAnchor(ctx, filepath.Dir(m.config.StatePath), m.config.Prompt, m.config.AnchorModel, m.config.AnchorTimeout); err != nil {
			return resetLog(changed, previous, "err"), fmt.Errorf("アンカー実行に失敗しました: %w", err)
		}
		status = "ok"
	}
	return resetLog(changed, previous, status), state.Save(m.config.StatePath, current)
}
