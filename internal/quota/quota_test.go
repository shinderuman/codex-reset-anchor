package quota

import "testing"

func testWindow(duration int64, used float64, resetsAt int64) Window {
	return Window{LimitID: "codex", WindowDurationMin: duration, UsedPercent: used, ResetsAt: resetsAt}
}

func TestChanged(t *testing.T) {
	w := func(duration int64, used float64, reset int64) *Window {
		window := testWindow(duration, used, reset)
		return &window
	}
	differentID := w(WeeklyWindowMinutes, 0, 2000)
	differentID.LimitID = "other"
	missingID := w(WeeklyWindowMinutes, 0, 2000)
	missingID.LimitID = ""
	for _, tt := range []struct {
		name              string
		previous, current *Window
		want              bool
	}{
		{"absent", nil, nil, false},
		{"initial observation", nil, w(WeeklyWindowMinutes, 0, 1000), false},
		{"missing window", w(WeeklyWindowMinutes, 74, 1000), nil, false},
		{"usage change only", w(WeeklyWindowMinutes, 74, 1000), w(WeeklyWindowMinutes, 0, 1000), false},
		{"5h later by one second", w(FiveHourWindowMinutes, 0, 1000), w(FiveHourWindowMinutes, 0, 1001), false},
		{"5h earlier by one second", w(FiveHourWindowMinutes, 0, 1000), w(FiveHourWindowMinutes, 0, 999), false},
		{"weekly later by one second", w(WeeklyWindowMinutes, 0, 1000), w(WeeklyWindowMinutes, 17, 1001), false},
		{"weekly earlier by one second", w(WeeklyWindowMinutes, 0, 1000), w(WeeklyWindowMinutes, 17, 999), false},
		{"5h same minute", w(FiveHourWindowMinutes, 0, 960), w(FiveHourWindowMinutes, 0, 1019), false},
		{"weekly same minute", w(WeeklyWindowMinutes, 0, 1019), w(WeeklyWindowMinutes, 17, 960), false},
		{"5h next minute", w(FiveHourWindowMinutes, 0, 1019), w(FiveHourWindowMinutes, 0, 1020), true},
		{"5h previous minute", w(FiveHourWindowMinutes, 0, 1020), w(FiveHourWindowMinutes, 0, 1019), true},
		{"weekly next minute", w(WeeklyWindowMinutes, 0, 1019), w(WeeklyWindowMinutes, 17, 1020), true},
		{"weekly previous minute", w(WeeklyWindowMinutes, 0, 1020), w(WeeklyWindowMinutes, 17, 1019), true},
		{"observed weekly reset", w(WeeklyWindowMinutes, 74, 1791948708), w(WeeklyWindowMinutes, 0, 1792301252), true},
		{"different duration", w(FiveHourWindowMinutes, 74, 1000), w(WeeklyWindowMinutes, 0, 2000), false},
		{"different ID", w(WeeklyWindowMinutes, 74, 1000), differentID, false},
		{"missing ID is not inferred", w(WeeklyWindowMinutes, 74, 1000), missingID, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Changed(tt.previous, tt.current); got != tt.want {
				t.Fatalf("Changed=%v, want %v", got, tt.want)
			}
		})
	}
}
