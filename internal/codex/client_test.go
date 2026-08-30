package codex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shinderuman/codex-reset-anchor/internal/quota"
)

func TestSelectQuotaSnapshotCurrentShape(t *testing.T) {
	primary := &rateLimitWindow{UsedPercent: 50, WindowDurationMin: quota.FiveHourWindowMinutes, ResetsAt: 1000}
	secondary := &rateLimitWindow{UsedPercent: 71, WindowDurationMin: quota.WeeklyWindowMinutes, ResetsAt: 2000}
	bucket := rateLimitBucket{LimitID: "codex", Primary: primary, Secondary: secondary}

	snapshot := selectQuotaSnapshot(rateLimitsResult{
		RateLimits:          &bucket,
		RateLimitsByLimitID: map[string]rateLimitBucket{"codex": bucket},
	}, time.Unix(10, 0))

	if snapshot.FiveHour == nil || snapshot.Weekly == nil {
		t.Fatalf("5h/weeklyを選択できなかった: %+v", snapshot)
	}
	if snapshot.FiveHour.UsedPercent != 50 || snapshot.Weekly.UsedPercent != 71 {
		t.Fatalf("選択結果が不正: %+v", snapshot)
	}
}

func TestSelectQuotaSnapshotDoesNotDependOnPlacement(t *testing.T) {
	bucket := rateLimitBucket{
		LimitID:   "codex",
		Primary:   &rateLimitWindow{WindowDurationMin: quota.WeeklyWindowMinutes},
		Secondary: &rateLimitWindow{WindowDurationMin: quota.FiveHourWindowMinutes},
	}

	snapshot := selectQuotaSnapshot(rateLimitsResult{RateLimits: &bucket}, time.Now())
	if snapshot.FiveHour == nil || snapshot.FiveHour.WindowName != "secondary" {
		t.Fatalf("5h選択が不正: %+v", snapshot.FiveHour)
	}
	if snapshot.Weekly == nil || snapshot.Weekly.WindowName != "primary" {
		t.Fatalf("weekly選択が不正: %+v", snapshot.Weekly)
	}
}

func TestRunAnchorUsesExpectedArgumentsAndWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	argumentsPath := filepath.Join(dir, "arguments.txt")
	workingDirectoryPath := filepath.Join(dir, "working-directory.txt")
	scriptPath := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$CODEX_TEST_ARGUMENTS"
pwd > "$CODEX_TEST_WORKING_DIRECTORY"
printf 'discarded stdout\n'
printf 'discarded stderr\n' >&2
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_ARGUMENTS", argumentsPath)
	t.Setenv("CODEX_TEST_WORKING_DIRECTORY", workingDirectoryPath)

	client := New(scriptPath, "test")
	workDirectory := filepath.Join(dir, "state")
	if err := client.RunAnchor(context.Background(), workDirectory, "Reply only: OK", "test-model", 2*time.Second); err != nil {
		t.Fatalf("anchorに失敗した: %v", err)
	}

	argumentsData, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	actual := strings.Split(strings.TrimSuffix(string(argumentsData), "\n"), "\n")
	expected := []string{
		"--ask-for-approval", "never", "exec", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--sandbox", "read-only", "--color", "never", "--model", "test-model",
		"-c", `model_reasoning_effort="low"`,
		"-c", `model_verbosity="low"`,
		"-c", `instructions="Answer briefly. Do not use tools."`,
		"-c", "include_permissions_instructions=false",
		"-c", "include_apps_instructions=false",
		"-c", "include_collaboration_mode_instructions=false",
		"-c", "include_environment_context=false",
		"-c", "skills.include_instructions=false",
		"-c", "skills.bundled.enabled=false",
		"-c", `web_search="disabled"`,
		"-c", "project_doc_max_bytes=0",
		"Reply only: OK",
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("引数が不正:\n got=%q\nwant=%q", actual, expected)
	}

	workingDirectoryData, err := os.ReadFile(workingDirectoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(workingDirectoryData)) != workDirectory {
		t.Fatalf("working directoryが不正: %q", workingDirectoryData)
	}
}

func TestRunAnchorDefaultsToLuna(t *testing.T) {
	dir := t.TempDir()
	argumentsPath := filepath.Join(dir, "arguments.txt")
	scriptPath := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$CODEX_TEST_ARGUMENTS"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_ARGUMENTS", argumentsPath)

	if err := New(scriptPath, "test").RunAnchor(context.Background(), dir, "Reply only: OK", "", 2*time.Second); err != nil {
		t.Fatalf("anchorに失敗した: %v", err)
	}
	argumentsData, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(strings.TrimSuffix(string(argumentsData), "\n"), "\n")
	for i := 0; i+1 < len(arguments); i++ {
		if arguments[i] == "--model" {
			if arguments[i+1] != defaultAnchorModel {
				t.Fatalf("default modelが不正: %q", arguments[i+1])
			}
			return
		}
	}
	t.Fatalf("--modelがない: %q", arguments)
}

func TestRunAnchorReturnsStderrOnFailure(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf 'anchor failed\\n' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := New(scriptPath, "test").RunAnchor(context.Background(), dir, "Reply only: OK", "", 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "anchor failed") || !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("失敗内容が不正: %v", err)
	}
}

func TestRunAnchorTimesOut(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "codex")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	err := New(scriptPath, "test").RunAnchor(context.Background(), dir, "Reply only: OK", "", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "タイムアウト") {
		t.Fatalf("timeoutエラーにならなかった: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout後もcodex execが長時間終了しなかった: %s", elapsed)
	}
}
