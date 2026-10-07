package quota

// Observation describes the next baseline and the windows that reset in a poll.
type Observation struct {
	Next      Snapshot
	Recovered []RecoveredWindow
}

type RecoveredWindow struct {
	Name   string
	Window Window
}

func Observe(previous, current Snapshot) Observation {
	var observation Observation
	fiveHour, recoveredFiveHour := observeWindow(previous.FiveHour, current.FiveHour)
	weekly, recoveredWeekly := observeWindow(previous.Weekly, current.Weekly)
	observation.Next = Snapshot{FiveHour: fiveHour, Weekly: weekly}
	if recoveredFiveHour {
		observation.Recovered = append(observation.Recovered, RecoveredWindow{Name: "5h", Window: *fiveHour})
	}
	if recoveredWeekly {
		observation.Recovered = append(observation.Recovered, RecoveredWindow{Name: "weekly", Window: *weekly})
	}
	return observation
}

func (o Observation) NeedsAnchor() bool {
	for _, recovered := range o.Recovered {
		if recovered.Window.UsedPercent > 0 {
			continue
		}
		return true
	}
	return false
}

func observeWindow(previous, current *Window) (*Window, bool) {
	if current == nil {
		return cloneWindow(previous), false
	}
	if previous == nil {
		return cloneWindow(current), false
	}

	observed := *current
	if SameWindow(*previous, observed) {
		if observed.LimitID == "" {
			observed.LimitID = previous.LimitID
		}
		if observed.ResetsAt <= 0 {
			observed.ResetsAt = previous.ResetsAt
		}
	}
	return &observed, Recovered(*previous, observed)
}

func cloneWindow(window *Window) *Window {
	if window == nil {
		return nil
	}
	copy := *window
	return &copy
}
