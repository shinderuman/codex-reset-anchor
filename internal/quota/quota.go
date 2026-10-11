package quota

const (
	FiveHourWindowMinutes = int64(300)
	WeeklyWindowMinutes   = int64(7 * 24 * 60)
)

type Window struct {
	LimitID           string  `json:"limitId"`
	WindowName        string  `json:"windowName"`
	UsedPercent       float64 `json:"usedPercent"`
	WindowDurationMin int64   `json:"windowDurationMins"`
	ResetsAt          int64   `json:"resetsAt"`
}

type Snapshot struct {
	FiveHour *Window `json:"fiveHour,omitempty"`
	Weekly   *Window `json:"weekly,omitempty"`
}

func SameWindow(previous, current Window) bool {
	if previous.WindowDurationMin != current.WindowDurationMin {
		return false
	}
	return previous.LimitID == current.LimitID
}

func Changed(previous, current *Window) bool {
	if previous == nil || current == nil || !SameWindow(*previous, *current) {
		return false
	}
	return current.ResetsAt/60 != previous.ResetsAt/60
}
