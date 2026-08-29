package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
	"github.com/shinderuman/codex-reset-anchor/internal/state"
)

type fakeAnchor struct {
	calls int
	err   error
}

func (f *fakeAnchor) RunAnchor(context.Context, string, string, string, time.Duration) error {
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

func TestProcessSnapshotDoesNotRepeatAnchorWhenAnchorMovesResetBoundary(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{statePath: statePath, prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)

	previous := state.Monitor{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Unix(), resetAt.Add(-time.Minute)),
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
	if anchor.calls != 1 {
		t.Fatalf("最初のresetでanchor回数が不正: %d", anchor.calls)
	}

	second := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour+5*time.Minute).Unix(), resetAt.Add(6*time.Minute)),
	}
	if err := processCurrentSnapshot(context.Background(), cfg, second, anchor); err != nil {
		t.Fatalf("次poll処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("anchorによるresetsAt移動を再resetと誤検知した: %d", anchor.calls)
	}
}

func TestSimultaneousResetsRunSingleAnchor(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{statePath: statePath, prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := state.Monitor{
		FiveHour: window(quota.FiveHourWindowMinutes, 100, resetAt.Unix(), resetAt.Add(-time.Minute)),
		Weekly:   window(quota.WeeklyWindowMinutes, 100, resetAt.Unix(), resetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}
	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute)),
		Weekly:   window(quota.WeeklyWindowMinutes, 0, resetAt.Add(7*24*time.Hour).Unix(), resetAt.Add(time.Minute)),
	}

	if err := processCurrentSnapshot(context.Background(), cfg, current, anchor); err != nil {
		t.Fatalf("reset処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("同時resetでanchorが1回ではない: %d", anchor.calls)
	}
}

func TestWeeklyResetSurvivesMissingBoundaryAndActiveUsage(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{statePath: statePath, prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}
	resetAt := time.Date(2026, 8, 30, 6, 30, 0, 0, time.UTC)
	previous := state.Monitor{
		Weekly: window(quota.WeeklyWindowMinutes, 92, resetAt.Unix(), resetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}

	sparse := quota.Snapshot{
		Weekly: window(quota.WeeklyWindowMinutes, 17, 0, resetAt.Add(time.Minute)),
	}
	if err := processCurrentSnapshot(context.Background(), cfg, sparse, anchor); err != nil {
		t.Fatalf("resetsAt欠落pollの処理に失敗した: %v", err)
	}
	if anchor.calls != 0 {
		t.Fatalf("次回境界が不明な状態でanchorした: %d", anchor.calls)
	}
	preserved, found, err := state.Load(statePath)
	if err != nil || !found {
		t.Fatalf("stateを再読込できなかった: found=%v err=%v", found, err)
	}
	if preserved.Weekly == nil || preserved.Weekly.ResetsAt != resetAt.Unix() {
		t.Fatalf("既知のweekly reset境界を保持しなかった: %+v", preserved.Weekly)
	}

	resolved := quota.Snapshot{
		Weekly: window(quota.WeeklyWindowMinutes, 23, resetAt.Add(7*24*time.Hour).Unix(), resetAt.Add(6*time.Minute)),
	}
	if err := processCurrentSnapshot(context.Background(), cfg, resolved, anchor); err != nil {
		t.Fatalf("weekly reset確定pollの処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("利用が進んだweekly resetでanchorしなかった: %d", anchor.calls)
	}
}

func TestFailedAnchorDoesNotAdvanceState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{statePath: statePath, prompt: "Reply only: OK"}
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := state.Monitor{
		FiveHour: window(quota.FiveHourWindowMinutes, 100, resetAt.Unix(), resetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute)),
	}
	anchor := &fakeAnchor{err: errors.New("failed")}
	if err := processCurrentSnapshot(context.Background(), cfg, current, anchor); err == nil {
		t.Fatal("anchor失敗が成功扱いになった")
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("anchor失敗時にstateが進んだ:\nBEFORE:\n%s\nAFTER:\n%s", before, after)
	}
}

func TestParseConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := parseConfig(nil, "/tmp/home")
	if err != nil {
		t.Fatalf("default configを解析できなかった: %v", err)
	}
	if cfg.pollEvery != 5*time.Minute || cfg.prompt != "Reply only: OK" || cfg.codexPath != "codex" {
		t.Fatalf("default configが不正: %+v", cfg)
	}
	if cfg.statePath != "/tmp/home/.local/var/codex-reset-anchor/state.json" {
		t.Fatalf("default state pathが不正: %s", cfg.statePath)
	}
	if _, err := parseConfig([]string{"-interval", "30s"}, "/tmp/home"); err == nil {
		t.Fatal("1分未満のintervalを許可した")
	}
}

func TestParseConfigRejectsInvalidAnchorTimeout(t *testing.T) {
	_, err := parseConfig([]string{"-anchor-timeout", "0s"}, t.TempDir())
	if err == nil {
		t.Fatal("0秒のanchor timeoutを受理した")
	}
}
