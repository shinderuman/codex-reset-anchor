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

func window(duration int64, used float64, resetsAt int64) *quota.Window {
	return &quota.Window{
		LimitID:           "codex",
		WindowName:        "primary",
		UsedPercent:       used,
		WindowDurationMin: duration,
		ResetsAt:          resetsAt,
	}
}

func TestSecondChangesWithinSameMinuteDoNotLogOrAnchor(t *testing.T) {
	for _, tt := range []struct {
		name   string
		unused bool
	}{
		{name: "active usage"},
		{name: "unused windows", unused: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.json")
			var current quota.Snapshot
			reader := readerFunc(func(context.Context) (quota.Snapshot, error) { return current, nil })
			anchor := &fakeAnchor{}
			var logs bytes.Buffer
			m := New(Config{StatePath: statePath}, reader, anchor, log.New(&logs, "", 0))
			for i, offset := range []int64{0, 1, 0, 27, 0} {
				fiveHourUsed, weeklyUsed := float64(16+i), float64(2+i)
				if tt.unused {
					fiveHourUsed, weeklyUsed = 0, 0
				}
				current = quota.Snapshot{
					FiveHour: window(quota.FiveHourWindowMinutes, fiveHourUsed, 1791714452+offset),
					Weekly:   window(quota.WeeklyWindowMinutes, weeklyUsed, 1792301252+offset),
				}
				m.poll(context.Background())
			}
			if anchor.calls != 0 || logs.Len() != 0 {
				t.Fatalf("同じ分内の秒の変化でresetを処理した: calls=%d logs=%q", anchor.calls, logs.String())
			}
			saved, found, err := state.Load(statePath)
			if err != nil || !found || !reflect.DeepEqual(saved, current) {
				t.Fatalf("最新の取得値を保存しなかった: saved=%+v found=%v err=%v", saved, found, err)
			}
		})
	}
}

func TestProcessSnapshotAnchorsWhenResetBoundaryChanges(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)

	previous := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Unix()),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}

	first := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix()),
	}
	if _, err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), first); err != nil {
		t.Fatalf("最初のreset処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("最初のresetでanchor回数が不正: %d", anchor.calls)
	}

	second := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour+time.Minute).Unix()),
	}
	if _, err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), second); err != nil {
		t.Fatalf("次poll処理に失敗した: %v", err)
	}
	if anchor.calls != 2 {
		t.Fatalf("分のresetsAt更新でanchorを再実行しなかった: %d", anchor.calls)
	}

	third := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, first.FiveHour.ResetsAt),
	}
	if _, err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	if anchor.calls != 3 {
		t.Fatalf("期限の分が早くなる変更でanchorを再実行しなかった: %d", anchor.calls)
	}
	if _, err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	if anchor.calls != 3 {
		t.Fatalf("期限が変わっていないのにanchorを再実行した: %d", anchor.calls)
	}
}

func TestSimultaneousResetsRunSingleAnchor(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 100, resetAt.Unix()),
		Weekly:   window(quota.WeeklyWindowMinutes, 100, resetAt.Unix()),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}
	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix()),
		Weekly:   window(quota.WeeklyWindowMinutes, 0, resetAt.Add(7*24*time.Hour).Unix()),
	}

	if _, err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), current); err != nil {
		t.Fatalf("reset処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("同時resetでanchorが1回ではない: %d", anchor.calls)
	}
}

func TestResetLogsSingleLine(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previousFiveHour := window(quota.FiveHourWindowMinutes, 80, resetAt.Unix())
	previousWeekly := window(quota.WeeklyWindowMinutes, 65, resetAt.Unix())
	resetFiveHour := window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix())
	resetWeekly := window(quota.WeeklyWindowMinutes, 17, resetAt.Add(7*24*time.Hour).Unix())
	activeFiveHour := window(quota.FiveHourWindowMinutes, 83, resetAt.Unix())
	activeWeekly := window(quota.WeeklyWindowMinutes, 67, resetAt.Unix())
	unusedActiveFiveHour := window(quota.FiveHourWindowMinutes, 0, resetAt.Unix())
	usedResetFiveHour := window(quota.FiveHourWindowMinutes, 3, resetAt.Add(5*time.Hour).Unix())

	tests := []struct {
		name        string
		previous    quota.Snapshot
		current     quota.Snapshot
		anchorError error
		wantLog     string
	}{
		{
			name:     "5h reset",
			previous: quota.Snapshot{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: resetFiveHour, Weekly: activeWeekly},
			wantLog:  "reset=5h prev=20%/35% anchor=ok\n",
		},
		{
			name:     "weekly reset with anchor skipped",
			previous: quota.Snapshot{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: activeFiveHour, Weekly: resetWeekly},
			wantLog:  "reset=weekly prev=20%/35% anchor=skip\n",
		},
		{
			name:     "simultaneous resets",
			previous: quota.Snapshot{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: resetFiveHour, Weekly: resetWeekly},
			wantLog:  "reset=5h,weekly prev=20%/35% anchor=ok\n",
		},
		{
			name:     "used weekly reset with unused unchanged 5h",
			previous: quota.Snapshot{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: unusedActiveFiveHour, Weekly: resetWeekly},
			wantLog:  "reset=weekly prev=20%/35% anchor=skip\n",
		},
		{
			name:     "simultaneous used resets",
			previous: quota.Snapshot{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: usedResetFiveHour, Weekly: resetWeekly},
			wantLog:  "reset=5h,weekly prev=20%/35% anchor=skip\n",
		},
		{
			name:     "missing current weekly still reports previous value",
			previous: quota.Snapshot{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: resetFiveHour},
			wantLog:  "reset=5h prev=20%/35% anchor=ok\n",
		},
		{
			name:     "missing previous 5h",
			previous: quota.Snapshot{Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: activeFiveHour, Weekly: resetWeekly},
			wantLog:  "reset=weekly prev=-/35% anchor=skip\n",
		},
		{
			name:     "missing weekly",
			previous: quota.Snapshot{FiveHour: previousFiveHour},
			current:  quota.Snapshot{FiveHour: resetFiveHour},
			wantLog:  "reset=5h prev=20%/- anchor=ok\n",
		},
		{
			name:        "anchor failure stays on one line",
			previous:    quota.Snapshot{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:     quota.Snapshot{FiveHour: resetFiveHour, Weekly: activeWeekly},
			anchorError: errors.New("failed\nsecond line"),
			wantLog:     `reset=5h prev=20%/35% anchor=err err="アンカー実行に失敗しました: failed\nsecond line"` + "\n",
		},
		{
			name:     "no reset emits no log",
			previous: quota.Snapshot{FiveHour: previousFiveHour, Weekly: previousWeekly},
			current:  quota.Snapshot{FiveHour: activeFiveHour, Weekly: activeWeekly},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.json")
			if err := state.Save(statePath, tt.previous); err != nil {
				t.Fatal(err)
			}
			reader := readerFunc(func(context.Context) (quota.Snapshot, error) {
				return tt.current, nil
			})
			anchor := &fakeAnchor{err: tt.anchorError}
			var logs bytes.Buffer
			m := New(Config{StatePath: statePath}, reader, anchor, log.New(&logs, "", 0))
			m.poll(context.Background())
			if got := logs.String(); got != tt.wantLog {
				t.Fatalf("unexpected reset log:\n got: %q\nwant: %q", got, tt.wantLog)
			}
		})
	}
}

func TestResetWithActiveUsageSkipsAnchorAndLogs(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
	anchor := &fakeAnchor{}
	resetAt := time.Date(2026, 8, 30, 6, 30, 0, 0, time.UTC)
	previous := quota.Snapshot{
		Weekly: window(quota.WeeklyWindowMinutes, 92, resetAt.Unix()),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}
	current := quota.Snapshot{
		Weekly: window(quota.WeeklyWindowMinutes, 17, resetAt.Add(7*24*time.Hour).Unix()),
	}

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)

	reader := readerFunc(func(context.Context) (quota.Snapshot, error) { return current, nil })
	New(cfg, reader, anchor, logger).poll(context.Background())
	if anchor.calls != 0 {
		t.Fatalf("既に使用済みのweekly resetでanchorした: %d", anchor.calls)
	}
	if got, want := logs.String(), "reset=weekly prev=-/8% anchor=skip\n"; got != want {
		t.Fatalf("unexpected skip log: got=%q want=%q", got, want)
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
	previous := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 80, resetAt.Unix()),
		Weekly:   window(quota.WeeklyWindowMinutes, 92, resetAt.Unix()),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}
	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix()),
		Weekly:   window(quota.WeeklyWindowMinutes, 17, resetAt.Add(7*24*time.Hour).Unix()),
	}

	if _, err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), current); err != nil {
		t.Fatalf("同時reset処理に失敗した: %v", err)
	}
	if anchor.calls != 1 {
		t.Fatalf("未使用の5h枠があるのにanchorしなかった: %d", anchor.calls)
	}
}

func TestEarlyWeeklyResetIsProcessedOnce(t *testing.T) {
	fiveHourResetAt := time.Date(2026, 10, 11, 10, 30, 36, 0, time.UTC)
	oldResetAt := time.Date(2026, 10, 14, 3, 31, 48, 0, time.UTC)
	newResetAt := time.Date(2026, 10, 18, 5, 27, 32, 0, time.UTC)
	for _, tt := range []struct {
		name      string
		used      float64
		wantCalls int
		wantLog   string
	}{
		{name: "unused", used: 0, wantCalls: 1, wantLog: "reset=weekly prev=80%/26% anchor=ok\n"},
		{name: "already used", used: 17, wantLog: "reset=weekly prev=80%/26% anchor=skip\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.json")
			previous := quota.Snapshot{
				FiveHour: window(quota.FiveHourWindowMinutes, 20, fiveHourResetAt.Unix()),
				Weekly:   window(quota.WeeklyWindowMinutes, 74, oldResetAt.Unix()),
			}
			if err := state.Save(statePath, previous); err != nil {
				t.Fatal(err)
			}
			current := quota.Snapshot{
				FiveHour: window(quota.FiveHourWindowMinutes, 21, previous.FiveHour.ResetsAt),
				Weekly:   window(quota.WeeklyWindowMinutes, tt.used, newResetAt.Unix()),
			}
			reader := readerFunc(func(context.Context) (quota.Snapshot, error) { return current, nil })
			anchor := &fakeAnchor{}
			var logs bytes.Buffer
			m := New(Config{StatePath: statePath}, reader, anchor, log.New(&logs, "", 0))
			m.poll(context.Background())
			if anchor.calls != tt.wantCalls || logs.String() != tt.wantLog {
				t.Fatalf("旧期限前のweekly処理が不正: calls=%d logs=%q", anchor.calls, logs.String())
			}
			saved, found, err := state.Load(statePath)
			if err != nil || !found || saved.Weekly == nil || saved.Weekly.ResetsAt != newResetAt.Unix() {
				t.Fatalf("更新されたweekly境界を保存できなかった: saved=%+v found=%v err=%v", saved, found, err)
			}

			m.poll(context.Background())
			current.Weekly.UsedPercent = 25
			m.poll(context.Background())
			if anchor.calls != tt.wantCalls || logs.String() != tt.wantLog {
				t.Fatalf("同じweeklyリセットを再処理した: calls=%d logs=%q", anchor.calls, logs.String())
			}
		})
	}
}

func TestFailedAnchorDoesNotAdvanceState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StatePath: statePath, Prompt: "Reply only: OK"}
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 100, resetAt.Unix()),
	}
	if err := state.Save(statePath, previous); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	current := quota.Snapshot{
		FiveHour: window(quota.FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix()),
	}
	anchor := &fakeAnchor{err: errors.New("failed")}
	if _, err := newTestMonitor(cfg, anchor).processSnapshot(context.Background(), current); err == nil {
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
