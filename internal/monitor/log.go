package monitor

import (
	"fmt"
	"strings"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

func (m *Monitor) logRecovery(windows []quota.RecoveredWindow, previous, current quota.Snapshot) {
	recovered := make([]string, 0, len(windows))
	alreadyUsed := make([]string, 0, len(windows))
	for _, recoveredWindow := range windows {
		recovered = append(recovered, recoveredWindow.Name)
		if recoveredWindow.Window.UsedPercent > 0 {
			alreadyUsed = append(alreadyUsed, fmt.Sprintf("%s(usedPercent=%.1f%%)", recoveredWindow.Name, recoveredWindow.Window.UsedPercent))
		}
	}
	m.logger.Printf("利用枠の回復を検知しました: %s", strings.Join(recovered, ","))
	m.logger.Printf("前回確認時の残り利用枠: 5h=%s, weekly=%s", remainingPercent(previous.FiveHour), remainingPercent(previous.Weekly))
	m.logger.Printf("リセット検知時の残り利用枠: 5h=%s, weekly=%s", remainingPercent(current.FiveHour), remainingPercent(current.Weekly))
	if len(alreadyUsed) > 0 {
		m.logger.Printf("リセット後の利用を検知しました: %s", strings.Join(alreadyUsed, ","))
	}
}

func remainingPercent(window *quota.Window) string {
	if window == nil {
		return "不明"
	}
	return fmt.Sprintf("%.1f%%", max(0, min(100, 100-window.UsedPercent)))
}
