package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testState(used float64, resetsAt, checkedAt int64) quotaState {
	state := quotaState{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: 10080,
		UsedPercent:       used,
		ResetsAt:          resetsAt,
		CheckedAt:         checkedAt,
	}
	return initializeResetDetector(state)
}

func TestEquivalentBoundaryDoesNotRecover(t *testing.T) {
	previous := testState(100, 1_000_000, 1)
	current := quotaState{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: 10080,
		UsedPercent:       0,
		ResetsAt:          1_000_001,
		CheckedAt:         2,
	}

	next, recovered := observeQuota(previous, current)
	if recovered {
		t.Fatal("1秒の境界差で回復扱いになった")
	}
	if next.ResetDetector == nil || !next.ResetDetector.WasAboveThreshold {
		t.Fatal("抑止した低使用率サンプルで基準が失われた")
	}
	if next.ResetDetector.ResetBoundary != previous.ResetsAt {
		t.Fatalf(
			"境界を保持していない: got=%d want=%d",
			next.ResetDetector.ResetBoundary,
			previous.ResetsAt,
		)
	}
}

func TestAdvancedBoundaryRecoversOnce(t *testing.T) {
	previous := testState(100, 1_000_000, 1)
	current := quotaState{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: 10080,
		UsedPercent:       0,
		ResetsAt:          1_000_000 + 7*24*60*60,
		CheckedAt:         2,
	}

	next, recovered := observeQuota(previous, current)
	if !recovered {
		t.Fatal("週次境界が進んだ高使用率→低使用率を回復検知できなかった")
	}
	if next.ResetDetector == nil || next.ResetDetector.WasAboveThreshold {
		t.Fatal("検知後に基準が解除されていない")
	}

	later := current
	later.CheckedAt = 3
	_, recoveredAgain := observeQuota(next, later)
	if recoveredAgain {
		t.Fatal("同じリセットを重複検知した")
	}
}

func TestMissingBoundaryPreservesBaseline(t *testing.T) {
	previous := testState(100, 1_000_000, 1)
	current := quotaState{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: 10080,
		UsedPercent:       0,
		ResetsAt:          0,
		CheckedAt:         2,
	}

	next, recovered := observeQuota(previous, current)
	if recovered {
		t.Fatal("境界欠落を回復扱いにした")
	}
	if next.ResetDetector == nil || !next.ResetDetector.WasAboveThreshold {
		t.Fatal("境界欠落で基準が失われた")
	}
	if next.ResetDetector.ResetBoundary != previous.ResetsAt {
		t.Fatal("既知の境界を保持していない")
	}
}

func TestLegacyStateMigratesDetector(t *testing.T) {
	previous := quotaState{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: 10080,
		UsedPercent:       100,
		ResetsAt:          1_000_000,
		CheckedAt:         1,
	}
	current := quotaState{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: 10080,
		UsedPercent:       0,
		ResetsAt:          1_000_000 + 7*24*60*60,
		CheckedAt:         2,
	}

	_, recovered := observeQuota(previous, current)
	if !recovered {
		t.Fatal("旧状態ファイルから検知状態を移行できなかった")
	}
}

func TestRPCRequestTimeoutTerminatesServer(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
while IFS= read -r line; do
    sleep 30
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("stub codexを書けなかった: %v", err)
	}

	server, err := startAppServer(context.Background(), scriptPath)
	if err != nil {
		t.Fatalf("stub app-serverを起動できなかった: %v", err)
	}

	startedAt := time.Now()
	_, err = server.call(
		context.Background(),
		"account/rateLimits/read",
		nil,
		100*time.Millisecond,
	)
	if err == nil {
		t.Fatal("タイムアウトせず成功した")
	}
	if time.Since(startedAt) > 2*time.Second {
		t.Fatalf("タイムアウト後の終了が遅すぎる: %s", time.Since(startedAt))
	}

	select {
	case <-server.done:
	case <-time.After(2 * time.Second):
		t.Fatal("タイムアウト後もapp-serverが終了していない")
	}
}

func TestRunAnchorDiscardsOutputAndUsesExpectedArguments(t *testing.T) {
	temporaryDirectory := t.TempDir()
	argumentsPath := filepath.Join(temporaryDirectory, "arguments.txt")
	workingDirectoryPath := filepath.Join(temporaryDirectory, "working-directory.txt")
	scriptPath := filepath.Join(temporaryDirectory, "codex")

	script := `#!/bin/sh
printf '%s\n' "$@" > "$CODEX_TEST_ARGUMENTS"
pwd > "$CODEX_TEST_WORKING_DIRECTORY"
printf 'discarded stdout\n'
printf 'discarded stderr\n' >&2
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("stub codexを書けなかった: %v", err)
	}

	t.Setenv("CODEX_TEST_ARGUMENTS", argumentsPath)
	t.Setenv("CODEX_TEST_WORKING_DIRECTORY", workingDirectoryPath)

	statePath := filepath.Join(temporaryDirectory, "state", "state.json")
	cfg := config{
		codexPath:   scriptPath,
		statePath:   statePath,
		prompt:      "Reply only: OK",
		anchorModel: "test-model",
	}

	stdout, stderr, err := captureOutput(func() error {
		return runAnchor(context.Background(), cfg)
	})
	if err != nil {
		t.Fatalf("アンカー実行に失敗した: %v", err)
	}
	if stdout != "" {
		t.Fatalf("標準出力が漏れた: %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("標準エラーが漏れた: %q", stderr)
	}

	argumentsData, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatalf("引数記録を読めなかった: %v", err)
	}

	actualArguments := strings.Split(strings.TrimSuffix(string(argumentsData), "\n"), "\n")
	expectedArguments := []string{
		"--ask-for-approval",
		"never",
		"exec",
		"--ephemeral",
		"--skip-git-repo-check",
		"--sandbox",
		"read-only",
		"--color",
		"never",
		"--model",
		"test-model",
		"Reply only: OK",
	}
	if !reflect.DeepEqual(actualArguments, expectedArguments) {
		t.Fatalf("引数が不正:\n got=%q\nwant=%q", actualArguments, expectedArguments)
	}

	workingDirectoryData, err := os.ReadFile(workingDirectoryPath)
	if err != nil {
		t.Fatalf("作業ディレクトリ記録を読めなかった: %v", err)
	}

	actualWorkingDirectory := strings.TrimSpace(string(workingDirectoryData))
	expectedWorkingDirectory := filepath.Dir(statePath)
	if actualWorkingDirectory != expectedWorkingDirectory {
		t.Fatalf(
			"作業ディレクトリが不正: got=%q want=%q",
			actualWorkingDirectory,
			expectedWorkingDirectory,
		)
	}
}

func TestRunAnchorReturnsStderrOnlyOnFailure(t *testing.T) {
	temporaryDirectory := t.TempDir()
	scriptPath := filepath.Join(temporaryDirectory, "codex")
	script := `#!/bin/sh
printf 'discarded stdout\n'
printf 'invalid anchor arguments\n' >&2
exit 2
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("stub codexを書けなかった: %v", err)
	}

	cfg := config{
		codexPath: scriptPath,
		statePath: filepath.Join(temporaryDirectory, "state", "state.json"),
		prompt:    "Reply only: OK",
	}

	stdout, stderr, err := captureOutput(func() error {
		return runAnchor(context.Background(), cfg)
	})
	if err == nil {
		t.Fatal("失敗するstubが成功扱いになった")
	}
	if stdout != "" {
		t.Fatalf("失敗時に標準出力が漏れた: %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("失敗時に標準エラーが直接漏れた: %q", stderr)
	}
	if !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("終了ステータスがエラーに含まれない: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid anchor arguments") {
		t.Fatalf("Codexの標準エラーが返されない: %v", err)
	}
	if strings.Contains(err.Error(), "discarded stdout") {
		t.Fatalf("不要な標準出力がエラーへ混入した: %v", err)
	}
}

func TestSuccessfulAnchorArmsDetectorForNextReset(t *testing.T) {
	temporaryDirectory := t.TempDir()
	scriptPath := filepath.Join(temporaryDirectory, "codex")
	script := `#!/bin/sh
printf 'discarded stdout\n'
printf 'discarded stderr\n' >&2
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("stub codexを書けなかった: %v", err)
	}

	statePath := filepath.Join(temporaryDirectory, "state", "state.json")
	previous := testState(100, 1_000_000, 1)
	current := quotaState{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: 10080,
		UsedPercent:       0,
		ResetsAt:          1_000_000 + 7*24*60*60,
		CheckedAt:         2,
	}

	cfg := config{
		codexPath: scriptPath,
		statePath: statePath,
		prompt:    "Reply only: OK",
	}
	if err := processQuotaChange(context.Background(), cfg, previous, current); err != nil {
		t.Fatalf("アンカー実行に失敗した: %v", err)
	}

	saved, found, err := loadState(statePath)
	if err != nil {
		t.Fatalf("状態を読み戻せなかった: %v", err)
	}
	if !found {
		t.Fatal("状態ファイルが保存されなかった")
	}
	if saved.ResetDetector == nil || !saved.ResetDetector.WasAboveThreshold {
		t.Fatal("最小アンカー実行後に次回リセット検知用の基準が有効化されていない")
	}
	if saved.ResetDetector.ResetBoundary != current.ResetsAt {
		t.Fatalf(
			"アンカー後の境界が不正: got=%d want=%d",
			saved.ResetDetector.ResetBoundary,
			current.ResetsAt,
		)
	}

	nextReset := current
	nextReset.ResetsAt += 7 * 24 * 60 * 60
	nextReset.CheckedAt = saved.CheckedAt + 1
	_, recovered := observeQuota(saved, nextReset)
	if !recovered {
		t.Fatal("使用率表示が0%のままでも次回リセットを検知できなかった")
	}
}

func TestFailedAnchorDoesNotAdvanceSavedState(t *testing.T) {
	temporaryDirectory := t.TempDir()
	scriptPath := filepath.Join(temporaryDirectory, "codex")
	script := `#!/bin/sh
printf 'anchor failed\n' >&2
exit 2
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("stub codexを書けなかった: %v", err)
	}

	statePath := filepath.Join(temporaryDirectory, "state", "state.json")
	previous := testState(100, 1_000_000, 1)
	current := quotaState{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: 10080,
		UsedPercent:       0,
		ResetsAt:          1_000_000 + 7*24*60*60,
		CheckedAt:         2,
	}
	if err := saveState(statePath, previous); err != nil {
		t.Fatalf("初期状態を保存できなかった: %v", err)
	}

	cfg := config{
		codexPath: scriptPath,
		statePath: statePath,
		prompt:    "Reply only: OK",
	}
	if err := processQuotaChange(context.Background(), cfg, previous, current); err == nil {
		t.Fatal("アンカー失敗が成功扱いになった")
	}

	saved, found, err := loadState(statePath)
	if err != nil {
		t.Fatalf("状態を読み戻せなかった: %v", err)
	}
	if !found {
		t.Fatal("状態ファイルが消えた")
	}
	if !reflect.DeepEqual(saved, previous) {
		t.Fatalf("アンカー失敗時に状態が進んだ:\n got=%+v\nwant=%+v", saved, previous)
	}
}

func captureOutput(run func() error) (string, string, error) {
	originalStdout := os.Stdout
	originalStderr := os.Stderr

	stdoutReader, stdoutWriter, _ := os.Pipe()
	stderrReader, stderrWriter, _ := os.Pipe()
	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter

	runErr := run()

	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	os.Stdout = originalStdout
	os.Stderr = originalStderr

	stdoutData, _ := io.ReadAll(stdoutReader)
	stderrData, _ := io.ReadAll(stderrReader)
	_ = stdoutReader.Close()
	_ = stderrReader.Close()

	return string(stdoutData), string(stderrData), runErr
}
