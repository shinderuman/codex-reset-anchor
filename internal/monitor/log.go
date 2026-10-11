package monitor

import (
	"fmt"
	"strings"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

func resetLog(changed []string, previous quota.Snapshot, status string) string {
	return fmt.Sprintf("reset=%s prev=%s/%s anchor=%s", strings.Join(changed, ","), remainingPercent(previous.FiveHour), remainingPercent(previous.Weekly), status)
}

func (m *Monitor) logResult(message string, err error) {
	if message == "" {
		if err != nil {
			m.logger.Printf("利用枠の処理に失敗しました: %v", err)
		}
		return
	}
	if err != nil {
		message += fmt.Sprintf(" err=%q", err.Error())
	}
	m.logger.Print(message)
}

func remainingPercent(window *quota.Window) string {
	if window == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100-window.UsedPercent)
}
