package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
	"github.com/shinderuman/codex-reset-anchor/internal/state"
)

type fakeAnchor struct {
	calls int
	err   error
}

func (f *fakeAnchor) RunAnchor(context.Context, string, string, string) error {
	f.calls++
	return f.err
}

func window(duration int64, used float64, resetsAt int64, checkedAt time.Time) *quota.Window {
	return &quota.Window{
		LimitID:           "codex",
		WindowName:        "primary",
		UsedPercent:       used,
		WindowDurationMin: duration,
		ResetsAt:          resetsAt,
		CheckedAt:         checkedAt.UnixNano(),
	}
}

func TestProcessCurrentSnapshotAnchorsOnceAfterReset(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{statePath: statePath, prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}

	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := state.Monitor{
		Version:  state.Version,
		FiveHour: window(quota.FiveHourWindowMinutes, 80, resetAt.Unix(), resetAt.Add(-time.Minute)),
		Weekly:   window(quota.WeeklyWindowMinutes, 70, resetAt.Add(6*24*time.Hour).Unix(), resetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}

	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute)),
		Weekly:   window(quota.WeeklyWindowMinutes, 70, resetAt.Add(6*24*time.Hour).Unix(), resetAt.Add(time.Minute)),
	}
	if err := processCurrentSnapshot(context.Background(), cfg, current, anchor); err != nil {
		t.Fatalf("reset処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("anchor回数が不正: got=%d want=1", anchor.calls)
	}
}

func TestProcessCurrentSnapshotDoesNotReanchorOnBoundaryDrift(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{statePath: statePath, prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}

	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := state.Monitor{
		Version:  state.Version,
		FiveHour: window(quota.FiveHourWindowMinutes, 80, resetAt.Unix(), resetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}

	first := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute)),
	}
	if err := processCurrentSnapshot(context.Background(), cfg, first, anchor); err != nil {
		t.Fatalf("最初のreset処理に失敗した: %v", err)
	}

	fiveMinutesLater := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour+5*time.Minute).Unix(), resetAt.Add(6*time.Minute)),
	}
	if err := processCurrentSnapshot(context.Background(), cfg, fiveMinutesLater, anchor); err != nil {
		t.Fatalf("次poll処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("resetsAtのdriftで再anchorした: got=%d want=1", anchor.calls)
	}
}

func TestProcessCurrentSnapshotDoesNotRearmSiblingWindow(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{statePath: statePath, prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}

	fiveResetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	weeklyResetAt := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	previous := state.Monitor{
		Version:  state.Version,
		FiveHour: window(quota.FiveHourWindowMinutes, 80, fiveResetAt.Unix(), fiveResetAt.Add(-time.Minute)),
		Weekly:   window(quota.WeeklyWindowMinutes, 0, weeklyResetAt.Unix(), fiveResetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}

	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, fiveResetAt.Add(5*time.Hour).Unix(), fiveResetAt.Add(time.Minute)),
		Weekly:   window(quota.WeeklyWindowMinutes, 0, weeklyResetAt.Add(5*time.Minute).Unix(), fiveResetAt.Add(time.Minute)),
	}
	if err := processCurrentSnapshot(context.Background(), cfg, current, anchor); err != nil {
		t.Fatalf("snapshot処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("anchor回数が不正: got=%d want=1", anchor.calls)
	}

	saved, _, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Weekly == nil || saved.Weekly.ResetsAt != current.Weekly.ResetsAt {
		t.Fatalf("weekly stateが観測値として保存されていない: %+v", saved.Weekly)
	}
}

func TestFailedAnchorDoesNotAdvanceState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{statePath: statePath, prompt: "Reply only: OK"}
	anchor := &fakeAnchor{err: errors.New("anchor failed")}
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := state.Monitor{
		Version:  state.Version,
		FiveHour: window(quota.FiveHourWindowMinutes, 80, resetAt.Unix(), resetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}

	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute)),
	}
	if err := processCurrentSnapshot(context.Background(), cfg, current, anchor); err == nil {
		t.Fatal("anchor失敗が成功扱いになった")
	}

	saved, _, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.FiveHour == nil || saved.FiveHour.ResetsAt != previous.FiveHour.ResetsAt {
		t.Fatalf("anchor失敗時にstateが進んだ: %+v", saved.FiveHour)
	}
}

func TestParseConfigRejectsShortInterval(t *testing.T) {
	_, err := parseConfig([]string{"-interval", "30s"}, t.TempDir())
	if err == nil {
		t.Fatal("1分未満のintervalを受理した")
	}
}
