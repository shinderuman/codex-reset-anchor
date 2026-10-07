package monitor

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
	"github.com/shinderuman/codex-reset-anchor/internal/state"
)

type readerFunc func(context.Context) (quota.Snapshot, error)

func (f readerFunc) ReadQuotas(ctx context.Context) (quota.Snapshot, error) {
	return f(ctx)
}

func TestRunPollsImmediatelyAndStopsOnCancellation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	current := quota.Snapshot{FiveHour: window(quota.FiveHourWindowMinutes, 25, 1000, time.Unix(900, 0))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	reader := readerFunc(func(context.Context) (quota.Snapshot, error) {
		calls++
		cancel()
		return current, nil
	})
	anchor := &fakeAnchor{}
	var logs bytes.Buffer
	m := New(Config{StatePath: statePath, PollEvery: time.Hour}, reader, anchor, log.New(&logs, "", 0))
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || anchor.calls != 0 || logs.Len() != 0 {
		t.Fatalf("unexpected initial poll: reads=%d anchors=%d logs=%s", calls, anchor.calls, logs.String())
	}
	saved, found, err := state.Load(statePath)
	if err != nil || !found || saved.FiveHour == nil || *saved.FiveHour != *current.FiveHour {
		t.Fatalf("initial poll did not save its baseline: saved=%+v found=%v err=%v", saved, found, err)
	}
}

func TestPollLogsReadAndProcessingErrors(t *testing.T) {
	for _, tt := range []struct {
		name         string
		readError    error
		corruptState bool
		wantLog      string
	}{
		{name: "read error", readError: errors.New("unavailable"), wantLog: "利用枠の取得に失敗しました: unavailable"},
		{name: "processing error", corruptState: true, wantLog: "利用枠の処理に失敗しました: 状態ファイルを解析できません"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.json")
			if tt.corruptState {
				if err := os.WriteFile(statePath, []byte("invalid JSON"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reader := readerFunc(func(context.Context) (quota.Snapshot, error) {
				return quota.Snapshot{}, tt.readError
			})
			anchor := &fakeAnchor{}
			var logs bytes.Buffer
			m := New(Config{StatePath: statePath}, reader, anchor, log.New(&logs, "", 0))
			m.poll(context.Background())
			if !strings.Contains(logs.String(), tt.wantLog) || anchor.calls != 0 {
				t.Fatalf("unexpected failed poll: anchors=%d logs=%s", anchor.calls, logs.String())
			}
		})
	}
}
