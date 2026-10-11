package state

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	snapshot := quota.Snapshot{
		FiveHour: &quota.Window{LimitID: "codex", WindowDurationMin: quota.FiveHourWindowMinutes, UsedPercent: 20, ResetsAt: 1000},
		Weekly:   &quota.Window{LimitID: "codex", WindowDurationMin: quota.WeeklyWindowMinutes, UsedPercent: 74, ResetsAt: 2000},
	}
	if err := Save(path, snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := Load(path)
	if err != nil || !found {
		t.Fatalf("state読込に失敗した: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(loaded, snapshot) {
		t.Fatalf("state内容が不正: got=%+v want=%+v", loaded, snapshot)
	}
}
