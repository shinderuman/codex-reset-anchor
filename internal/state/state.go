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

func Load(path string) (quota.Snapshot, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return quota.Snapshot{}, false, nil
	}
	if err != nil {
		return quota.Snapshot{}, false, fmt.Errorf("状態ファイルを読めません: %w", err)
	}

	var snapshot quota.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return quota.Snapshot{}, false, fmt.Errorf("状態ファイルを解析できません: %w", err)
	}
	return snapshot, true, nil
}

func Save(path string, snapshot quota.Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("状態ディレクトリを作成できません: %w", err)
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
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
