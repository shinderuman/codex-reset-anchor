package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

const Version = 3

type Monitor struct {
	Version  int           `json:"version"`
	FiveHour *quota.Window `json:"fiveHour,omitempty"`
	Weekly   *quota.Window `json:"weekly,omitempty"`
}

func FromSnapshot(snapshot quota.Snapshot) Monitor {
	return Monitor{
		Version:  Version,
		FiveHour: cloneWindow(snapshot.FiveHour),
		Weekly:   cloneWindow(snapshot.Weekly),
	}
}

func Load(path string) (Monitor, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Monitor{}, false, nil
	}
	if err != nil {
		return Monitor{}, false, fmt.Errorf("状態ファイルを読めません: %w", err)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return Monitor{}, false, fmt.Errorf("状態ファイルを解析できません: %w", err)
	}

	if _, ok := keys["limitId"]; ok {
		var legacy quota.Window
		if err := json.Unmarshal(data, &legacy); err != nil {
			return Monitor{}, false, fmt.Errorf("旧状態ファイルを解析できません: %w", err)
		}
		monitor := Monitor{Version: Version}
		switch legacy.WindowDurationMin {
		case quota.FiveHourWindowMinutes:
			monitor.FiveHour = &legacy
		default:
			monitor.Weekly = &legacy
		}
		return monitor, true, nil
	}

	var monitor Monitor
	if err := json.Unmarshal(data, &monitor); err != nil {
		return Monitor{}, false, fmt.Errorf("状態ファイルを解析できません: %w", err)
	}
	if monitor.Version != 2 && monitor.Version != Version {
		return Monitor{}, false, fmt.Errorf("未対応の状態ファイルversionです: %d", monitor.Version)
	}
	monitor.Version = Version
	return monitor, true, nil
}

func Save(path string, monitor Monitor) error {
	monitor.Version = Version
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("状態ディレクトリを作成できません: %w", err)
	}

	data, err := json.MarshalIndent(monitor, "", "  ")
	if err != nil {
		return fmt.Errorf("状態JSONを生成できません: %w", err)
	}
	data = append(data, '\n')

	temporaryPath := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(temporaryPath, data, 0o600); err != nil {
		return fmt.Errorf("一時状態ファイルを書けません: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("状態ファイルを更新できません: %w", err)
	}
	return nil
}

func cloneWindow(window *quota.Window) *quota.Window {
	if window == nil {
		return nil
	}
	copy := *window
	return &copy
}
