package quota

import (
	"reflect"
	"testing"
	"time"
)

func TestObservePreservesKnownDataOnlyForSameWindow(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := testWindow(FiveHourWindowMinutes, 80, resetAt.Unix(), resetAt.Add(-time.Minute))
	current := testWindow(FiveHourWindowMinutes, 17, 0, resetAt.Add(time.Minute))
	current.LimitID = ""
	merged := current
	merged.LimitID = previous.LimitID
	merged.ResetsAt = previous.ResetsAt
	replacement := current
	replacement.LimitID = "other"
	recovered := current
	recovered.ResetsAt = resetAt.Add(5 * time.Hour).Unix()
	recoveredMerged := recovered
	recoveredMerged.LimitID = previous.LimitID

	tests := []struct {
		name      string
		previous  *Window
		current   *Window
		want      *Window
		recovered bool
	}{
		{name: "no observations"},
		{name: "first observation", current: &current, want: &current},
		{name: "missing window", previous: &previous, want: &previous},
		{name: "missing ID and boundary", previous: &previous, current: &current, want: &merged},
		{name: "different ID", previous: &previous, current: &replacement, want: &replacement},
		{name: "reset after usage with missing ID", previous: &previous, current: &recovered, want: &recoveredMerged, recovered: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Observe(Snapshot{FiveHour: tt.previous}, Snapshot{FiveHour: tt.current})
			if !reflect.DeepEqual(got.Next.FiveHour, tt.want) || got.Next.Weekly != nil {
				t.Fatalf("unexpected next baseline: %+v, want %+v", got.Next.FiveHour, tt.want)
			}
			if (len(got.Recovered) > 0) != tt.recovered {
				t.Fatalf("unexpected recovered windows: %+v", got.Recovered)
			}
			if tt.recovered && (got.Recovered[0].Name != "5h" || got.Recovered[0].Window != *tt.want) {
				t.Fatalf("unexpected recovered window details: %+v", got.Recovered)
			}
			if got.Next.FiveHour != nil && (got.Next.FiveHour == tt.previous || got.Next.FiveHour == tt.current) {
				t.Fatal("next baseline shares a mutable window with an input snapshot")
			}
		})
	}
	if current.LimitID != "" || current.ResetsAt != 0 || recovered.LimitID != "" {
		t.Fatal("Observe modified an input window while filling missing data")
	}
}

func TestObserveAnchorsOnlyUnusedRecoveredWindows(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previousFiveHour := testWindow(FiveHourWindowMinutes, 80, resetAt.Unix(), resetAt.Add(-time.Minute))
	previousWeekly := testWindow(WeeklyWindowMinutes, 92, resetAt.Unix(), resetAt.Add(-time.Minute))
	unusedFiveHour := testWindow(FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute))
	usedWeekly := testWindow(WeeklyWindowMinutes, 17, resetAt.Add(7*24*time.Hour).Unix(), resetAt.Add(time.Minute))
	activeFiveHour := unusedFiveHour
	activeFiveHour.ResetsAt = resetAt.Unix()
	previous := Snapshot{FiveHour: &previousFiveHour, Weekly: &previousWeekly}

	tests := []struct {
		name        string
		current     Snapshot
		wantNames   []string
		needsAnchor bool
	}{
		{name: "no reset", current: Snapshot{FiveHour: &activeFiveHour}},
		{name: "unused 5h reset", current: Snapshot{FiveHour: &unusedFiveHour}, wantNames: []string{"5h"}, needsAnchor: true},
		{name: "used weekly reset with unused active 5h", current: Snapshot{FiveHour: &activeFiveHour, Weekly: &usedWeekly}, wantNames: []string{"weekly"}},
		{name: "simultaneous resets", current: Snapshot{FiveHour: &unusedFiveHour, Weekly: &usedWeekly}, wantNames: []string{"5h", "weekly"}, needsAnchor: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Observe(previous, tt.current)
			var names []string
			for _, recovered := range got.Recovered {
				names = append(names, recovered.Name)
			}
			if !reflect.DeepEqual(names, tt.wantNames) || got.NeedsAnchor() != tt.needsAnchor {
				t.Fatalf("recovered=%v needsAnchor=%v, want recovered=%v needsAnchor=%v", names, got.NeedsAnchor(), tt.wantNames, tt.needsAnchor)
			}
		})
	}
}
