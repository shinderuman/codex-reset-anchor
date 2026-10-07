package monitor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
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
	if err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), first); err != nil {
		t.Fatalf("最初のreset処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("最初のresetでanchor回数が不正: %d", anchor.calls)
	}

	second := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour+5*time.Minute).Unix(), resetAt.Add(6*time.Minute)),
	}
	if err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), second); err != nil {
		t.Fatalf("次poll処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("anchorによるresetsAt移動を再resetと誤検知した: %d", anchor.calls)
	}
}

func TestSimultaneousResetsRunSingleAnchor(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
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

	if err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), current); err != nil {
		t.Fatalf("reset処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("同時resetでanchorが1回ではない: %d", anchor.calls)
	}
}

func TestResetLogsRemainingQuotas(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previousFiveHour := window(quota.FiveHourWindowMinutes, 80, resetAt.Unix(), resetAt.Add(-time.Minute))
	previousWeekly := window(quota.WeeklyWindowMinutes, 65, resetAt.Unix(), resetAt.Add(-time.Minute))
	resetFiveHour := window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute))
	resetWeekly := window(quota.WeeklyWindowMinutes, 17, resetAt.Add(7*24*time.Hour).Unix(), resetAt.Add(time.Minute))
	activeFiveHour := window(quota.FiveHourWindowMinutes, 82.5, resetAt.Unix(), resetAt.Add(time.Minute))
	activeWeekly := window(quota.WeeklyWindowMinutes, 67, resetAt.Unix(), resetAt.Add(time.Minute))

	tests := []struct {
		name     string
		previous state.Monitor
		current  quota.Snapshot
		before   string
		after    string
	}{
		{
			name:     "5h reset",
			previous: state.Monitor{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: resetFiveHour, Weekly: activeWeekly},
			before:   "5h=20.0%, weekly=35.0%",
			after:    "5h=100.0%, weekly=33.0%",
		},
		{
			name:     "weekly reset with anchor skipped",
			previous: state.Monitor{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: activeFiveHour, Weekly: resetWeekly},
			before:   "5h=20.0%, weekly=35.0%",
			after:    "5h=17.5%, weekly=83.0%",
		},
		{
			name:     "simultaneous resets",
			previous: state.Monitor{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: resetFiveHour, Weekly: resetWeekly},
			before:   "5h=20.0%, weekly=35.0%",
			after:    "5h=100.0%, weekly=83.0%",
		},
		{
			name:     "missing current weekly does not report stale value",
			previous: state.Monitor{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: resetFiveHour},
			before:   "5h=20.0%, weekly=35.0%",
			after:    "5h=100.0%, weekly=不明",
		},
		{
			name:     "missing previous 5h",
			previous: state.Monitor{Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: activeFiveHour, Weekly: resetWeekly},
			before:   "5h=不明, weekly=35.0%",
			after:    "5h=17.5%, weekly=83.0%",
		},
		{
			name:     "missing weekly",
			previous: state.Monitor{FiveHour: previousFiveHour},
			current:  quota.Snapshot{FiveHour: resetFiveHour},
			before:   "5h=20.0%, weekly=不明",
			after:    "5h=100.0%, weekly=不明",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.json")
			if err := state.Save(statePath, tt.previous); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)

			if err := New(Config{StatePath: statePath}, nil, &fakeAnchor{}, logger).processSnapshot(context.Background(), tt.current); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"前回確認時の残り利用枠: " + tt.before,
				"リセット検知時の残り利用枠: " + tt.after,
			} {
				if strings.Count(logs.String(), want) != 1 {
					t.Fatalf("残り利用枠ログが1回出力されなかった: want=%q logs=%s", want, logs.String())
				}
			}
		})
	}
}

func TestResetWithActiveUsageSkipsAnchorAndLogs(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}
	resetAt := time.Date(2026, 8, 30, 6, 30, 0, 0, time.UTC)
	previous := state.Monitor{
		Weekly: window(quota.WeeklyWindowMinutes, 92, resetAt.Unix(), resetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}
	current := quota.Snapshot{
		Weekly: window(quota.WeeklyWindowMinutes, 17, resetAt.Add(7*24*time.Hour).Unix(), resetAt.Add(time.Minute)),
	}

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)

	if err := New(cfg, nil, anchor, logger).processSnapshot(context.Background(), current); err != nil {
		t.Fatalf("weekly reset処理に失敗した: %v", err)
	}
	if anchor.calls != 0 {
		t.Fatalf("既に使用済みのweekly resetでanchorした: %d", anchor.calls)
	}
	if !strings.Contains(logs.String(), "利用枠の回復を検知しました: weekly") {
		t.Fatalf("weekly reset検知ログがない: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "リセット後の利用を検知しました: weekly(usedPercent=17.0%)") {
		t.Fatalf("利用済みログがない: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "アンカーをスキップしました") {
		t.Fatalf("anchor skipログがない: %s", logs.String())
	}

	saved, found, err := state.Load(statePath)
	if err != nil || !found {
		t.Fatalf("stateを再読込できなかった: found=%v err=%v", found, err)
	}
	if saved.Weekly == nil || saved.Weekly.ResetsAt != current.Weekly.ResetsAt {
		t.Fatalf("skip後に新しいweekly境界を保存しなかった: %+v", saved.Weekly)
	}
}

func TestSimultaneousResetsAnchorWhenAnyRecoveredWindowUnused(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}
	resetAt := time.Date(2026, 8, 30, 6, 30, 0, 0, time.UTC)
	previous := state.Monitor{
		FiveHour: window(quota.FiveHourWindowMinutes, 80, resetAt.Unix(), resetAt.Add(-time.Minute)),
		Weekly:   window(quota.WeeklyWindowMinutes, 92, resetAt.Unix(), resetAt.Add(-time.Minute)),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}
	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute)),
		Weekly:   window(quota.WeeklyWindowMinutes, 17, resetAt.Add(7*24*time.Hour).Unix(), resetAt.Add(time.Minute)),
	}

	if err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), current); err != nil {
		t.Fatalf("同時reset処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("未使用の5h枠があるのにanchorしなかった: %d", anchor.calls)
	}
}

func TestWeeklyResetSurvivesMissingBoundaryAndActiveUsage(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
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
	if err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), sparse); err != nil {
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
	if err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), resolved); err != nil {
		t.Fatalf("weekly reset確定pollの処理に失敗した: %v", err)
	}
	if anchor.calls != 0 {
		t.Fatalf("既に利用済みのweekly resetでanchorした: %d", anchor.calls)
	}
	saved, found, err := state.Load(statePath)
	if err != nil || !found {
		t.Fatalf("stateを再読込できなかった: found=%v err=%v", found, err)
	}
	if saved.Weekly == nil || saved.Weekly.ResetsAt != resolved.Weekly.ResetsAt {
		t.Fatalf("skip後に確定したweekly境界を保存しなかった: %+v", saved.Weekly)
	}
}

func TestFailedAnchorDoesNotAdvanceState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
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
	if err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), current); err == nil {
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

func newTestMonitor(cfg Config, anchor AnchorRunner) *Monitor {
	return New(cfg, nil, anchor, log.New(io.Discard, "", 0))
}
