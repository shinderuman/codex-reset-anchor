package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

func TestLoadVersion2StateMigratesToCurrentVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data := []byte(`{
  "version": 2,
  "fiveHour": {
    "limitId": "codex",
    "windowName": "primary",
    "usedPercent": 0,
    "windowDurationMins": 300,
    "resetsAt": 1000,
    "checkedAt": 1,
    "resetDetector": {"wasAboveThreshold": true, "resetBoundary": 900}
  }
}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	monitor, found, err := Load(path)
	if err != nil || !found {
		t.Fatalf("version 2 stateを読み込めなかった: found=%v err=%v", found, err)
	}
	if monitor.Version != Version || monitor.FiveHour == nil || monitor.FiveHour.ResetsAt != 1000 {
		t.Fatalf("migration結果が不正: %+v", monitor)
	}
}

func TestLoadLegacySingleWindowState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := quota.Window{
		LimitID:           "codex",
		WindowName:        "secondary",
		UsedPercent:       50,
		WindowDurationMin: quota.WeeklyWindowMinutes,
		ResetsAt:          2000,
		CheckedAt:         1,
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	monitor, found, err := Load(path)
	if err != nil || !found {
		t.Fatalf("legacy stateを読み込めなかった: found=%v err=%v", found, err)
	}
	if monitor.Weekly == nil || monitor.FiveHour != nil {
		t.Fatalf("legacy weeklyのmigration結果が不正: %+v", monitor)
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	monitor := Monitor{
		FiveHour: &quota.Window{LimitID: "codex", WindowDurationMin: quota.FiveHourWindowMinutes, ResetsAt: 1000},
	}
	if err := Save(path, monitor); err != nil {
		t.Fatalf("state保存に失敗した: %v", err)
	}

	loaded, found, err := Load(path)
	if err != nil || !found {
		t.Fatalf("state読込に失敗した: found=%v err=%v", found, err)
	}
	if loaded.Version != Version || loaded.FiveHour == nil || loaded.FiveHour.ResetsAt != 1000 {
		t.Fatalf("state内容が不正: %+v", loaded)
	}
}
