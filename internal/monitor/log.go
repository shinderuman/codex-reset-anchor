package monitor

import (
	"fmt"
	"strings"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

func (m *Monitor) logReset(result *resetResult, err error) {
	recovered := make([]string, 0, len(result.windows))
	for _, recoveredWindow := range result.windows {
		recovered = append(recovered, recoveredWindow.Name)
	}
	message := fmt.Sprintf("reset=%s prev=%s/%s anchor=%s", strings.Join(recovered, ","), remainingPercent(result.previous.FiveHour), remainingPercent(result.previous.Weekly), result.anchor)
	if err != nil {
		message += fmt.Sprintf(" err=%q", err.Error())
	}
	m.logger.Print(message)
}

func remainingPercent(window *quota.Window) string {
	if window == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", max(0, min(100, 100-window.UsedPercent)))
}
