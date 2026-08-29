package quota

import "time"

const (
	FiveHourWindowMinutes = int64(300)
	WeeklyWindowMinutes   = int64(7 * 24 * 60)

	resetThresholdPercent = 1.0
	boundaryTolerance     = 2 * time.Minute
)

type Window struct {
	LimitID           string  `json:"limitId"`
	WindowName        string  `json:"windowName"`
	UsedPercent       float64 `json:"usedPercent"`
	WindowDurationMin int64   `json:"windowDurationMins"`
	ResetsAt          int64   `json:"resetsAt"`
	CheckedAt         int64   `json:"checkedAt"`
}

type Snapshot struct {
	FiveHour *Window
	Weekly   *Window
}

func Recovered(previous, current Window) bool {
	if previous.LimitID != current.LimitID || previous.WindowDurationMin != current.WindowDurationMin {
		return false
	}
	if current.CheckedAt <= previous.CheckedAt {
		return false
	}
	if current.UsedPercent > resetThresholdPercent {
		return false
	}
	if !resetBoundaryAdvanced(previous.ResetsAt, current.ResetsAt) {
		return false
	}

	return previous.ResetsAt > 0 && current.CheckedAt >= time.Unix(previous.ResetsAt, 0).UnixNano()
}

func resetBoundaryAdvanced(previous, current int64) bool {
	if previous <= 0 || current <= 0 || current <= previous {
		return false
	}

	return time.Duration(current-previous)*time.Second >= boundaryTolerance
}
