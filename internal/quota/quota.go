package quota

import "time"

const (
	FiveHourWindowMinutes = int64(300)
	WeeklyWindowMinutes   = int64(7 * 24 * 60)

	boundaryTolerance = 2 * time.Minute
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

func SameWindow(previous, current Window) bool {
	if previous.WindowDurationMin != current.WindowDurationMin {
		return false
	}
	return previous.LimitID == current.LimitID || previous.LimitID == "" || current.LimitID == ""
}

func Recovered(previous, current Window) bool {
	if !SameWindow(previous, current) {
		return false
	}
	if current.CheckedAt <= previous.CheckedAt {
		return false
	}
	if previous.ResetsAt <= 0 {
		return false
	}
	if current.CheckedAt < time.Unix(previous.ResetsAt, 0).UnixNano() {
		return false
	}

	return resetBoundaryAdvanced(previous.ResetsAt, current.ResetsAt)
}

func resetBoundaryAdvanced(previous, current int64) bool {
	if previous <= 0 || current <= 0 || current <= previous {
		return false
	}

	return time.Duration(current-previous)*time.Second >= boundaryTolerance
}
