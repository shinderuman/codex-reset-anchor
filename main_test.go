package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func quota(limitID, windowName string, duration int64, used float64, resetsAt, checkedAt int64) quotaState {
	return quotaState{
		LimitID:           limitID,
		WindowName:        windowName,
		WindowDurationMin: duration,
		UsedPercent:       used,
		ResetsAt:          resetsAt,
		CheckedAt:         checkedAt,
	}
}

func initializedQuota(limitID, windowName string, duration int64, used float64, resetsAt, checkedAt int64) quotaState {
	return initializeResetDetector(quota(limitID, windowName, duration, used, resetsAt, checkedAt))
}

func writeAnchorStub(t *testing.T, exitCode int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	callsPath := filepath.Join(dir, "calls")
	scriptPath := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf 'x\\n' >> \"$CODEX_TEST_CALLS\"\n"
	if exitCode != 0 {
		script += "printf 'anchor failed\\n' >&2\nexit " + string(rune('0'+exitCode)) + "\n"
	}
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("stub codexを書けなかった: %v", err)
	}
	t.Setenv("CODEX_TEST_CALLS", callsPath)
	return scriptPath, callsPath
}

func countAnchorCalls(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("anchor call記録を読めなかった: %v", err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0
	}
	return len(strings.Split(text, "\n"))
}

func TestSelectQuotaSnapshotCurrentCodexShape(t *testing.T) {
	primary := &rateLimitWindow{UsedPercent: 50, WindowDurationMin: 300, ResetsAt: 1_787_775_687}
	secondary := &rateLimitWindow{UsedPercent: 71, WindowDurationMin: 10080, ResetsAt: 1_788_271_937}
	bucket := rateLimitBucket{LimitID: "codex", Primary: primary, Secondary: secondary}

	result := rateLimitsResult{
		RateLimits: &bucket,
		RateLimitsByLimitID: map[string]rateLimitBucket{
			"codex": bucket,
		},
	}

	snapshot := selectQuotaSnapshot(result)
	if snapshot.FiveHour == nil || snapshot.Weekly == nil {
		t.Fatalf("5h/weeklyの両方を選択できなかった: %+v", snapshot)
	}
	if snapshot.FiveHour.WindowDurationMin != fiveHourWindowMinutes || snapshot.FiveHour.UsedPercent != 50 {
		t.Fatalf("5h選択が不正: %+v", *snapshot.FiveHour)
	}
	if snapshot.Weekly.WindowDurationMin != weeklyWindowMinutes || snapshot.Weekly.UsedPercent != 71 {
		t.Fatalf("weekly選択が不正: %+v", *snapshot.Weekly)
	}
}

func TestSelectQuotaSnapshotDoesNotDependOnPrimarySecondaryPlacement(t *testing.T) {
	weekly := &rateLimitWindow{UsedPercent: 20, WindowDurationMin: weeklyWindowMinutes, ResetsAt: 2000}
	fiveHour := &rateLimitWindow{UsedPercent: 30, WindowDurationMin: fiveHourWindowMinutes, ResetsAt: 1000}
	bucket := rateLimitBucket{LimitID: "codex", Primary: weekly, Secondary: fiveHour}

	snapshot := selectQuotaSnapshot(rateLimitsResult{RateLimits: &bucket})
	if snapshot.FiveHour == nil || snapshot.FiveHour.WindowName != "secondary" {
		t.Fatalf("secondaryの5hを選択できなかった: %+v", snapshot.FiveHour)
	}
	if snapshot.Weekly == nil || snapshot.Weekly.WindowName != "primary" {
		t.Fatalf("primaryのweeklyを選択できなかった: %+v", snapshot.Weekly)
	}
}

func TestSelectQuotaSnapshotIgnoresLongerNonWeeklyWindow(t *testing.T) {
	monthly := &rateLimitWindow{UsedPercent: 90, WindowDurationMin: 43200, ResetsAt: 9000}
	weekly := &rateLimitWindow{UsedPercent: 40, WindowDurationMin: weeklyWindowMinutes, ResetsAt: 8000}
	result := rateLimitsResult{
		RateLimitsByLimitID: map[string]rateLimitBucket{
			"other": {LimitID: "other", Primary: monthly},
			"codex": {LimitID: "codex", Secondary: weekly},
		},
	}

	snapshot := selectQuotaSnapshot(result)
	if snapshot.Weekly == nil || snapshot.Weekly.WindowDurationMin != weeklyWindowMinutes || snapshot.Weekly.LimitID != "codex" {
		t.Fatalf("10080分のCodex weeklyを選択できなかった: %+v", snapshot.Weekly)
	}
}

func TestEquivalentBoundaryDoesNotRecover(t *testing.T) {
	previous := initializedQuota("codex", "primary", weeklyWindowMinutes, 100, 1_000_000, 1)
	current := quota("codex", "primary", weeklyWindowMinutes, 0, 1_000_001, 2)

	next, recovered := observeQuota(previous, current)
	if recovered {
		t.Fatal("1秒の境界差で回復扱いになった")
	}
	if next.ResetDetector == nil || !next.ResetDetector.WasAboveThreshold {
		t.Fatal("抑止した低使用率サンプルで基準が失われた")
	}
	if next.ResetDetector.ResetBoundary != previous.ResetsAt {
		t.Fatalf("境界を保持していない: got=%d want=%d", next.ResetDetector.ResetBoundary, previous.ResetsAt)
	}
}

func TestAdvancedBoundaryRecoversOnce(t *testing.T) {
	previous := initializedQuota("codex", "primary", weeklyWindowMinutes, 100, 1_000_000, 1)
	current := quota("codex", "primary", weeklyWindowMinutes, 0, 1_000_000+7*24*60*60, 2)

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

func TestObserveQuotaSurvivesPrimarySecondaryPlacementChange(t *testing.T) {
	previous := initializedQuota("codex", "primary", fiveHourWindowMinutes, 100, 10_000, 1)
	current := quota("codex", "secondary", fiveHourWindowMinutes, 0, 10_000+5*60*60, 2)

	_, recovered := observeQuota(previous, current)
	if !recovered {
		t.Fatal("primary/secondary移動で5h reset検知が失われた")
	}
}

func TestMissingBoundaryPreservesBaseline(t *testing.T) {
	previous := initializedQuota("codex", "primary", weeklyWindowMinutes, 100, 1_000_000, 1)
	current := quota("codex", "primary", weeklyWindowMinutes, 0, 0, 2)

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

func TestInitialSnapshotDoesNotAnchor(t *testing.T) {
	scriptPath, callsPath := writeAnchorStub(t, 0)
	statePath := filepath.Join(t.TempDir(), "state.json")
	five := quota("codex", "primary", fiveHourWindowMinutes, 50, 1000, 1)
	weekly := quota("codex", "secondary", weeklyWindowMinutes, 71, 2000, 1)

	if err := processCurrentSnapshot(context.Background(), config{codexPath: scriptPath, statePath: statePath, prompt: "Reply only: OK"}, quotaSnapshot{FiveHour: &five, Weekly: &weekly}); err != nil {
		t.Fatalf("初期snapshot保存に失敗した: %v", err)
	}
	if got := countAnchorCalls(t, callsPath); got != 0 {
		t.Fatalf("初回観測でanchorした: %d", got)
	}
	state, found, err := loadMonitorState(statePath)
	if err != nil || !found {
		t.Fatalf("初期stateを読み戻せなかった: found=%v err=%v", found, err)
	}
	if state.FiveHour == nil || state.Weekly == nil {
		t.Fatalf("初期stateに両windowが保存されていない: %+v", state)
	}
}

func TestFiveHourResetAnchorsOnceAndRearmsAllWindows(t *testing.T) {
	scriptPath, callsPath := writeAnchorStub(t, 0)
	statePath := filepath.Join(t.TempDir(), "state.json")
	previousFive := initializedQuota("codex", "primary", fiveHourWindowMinutes, 100, 10_000, 1)
	previousWeekly := initializedQuota("codex", "secondary", weeklyWindowMinutes, 0, 20_000, 1)
	if err := saveMonitorState(statePath, monitorState{FiveHour: &previousFive, Weekly: &previousWeekly}); err != nil {
		t.Fatalf("初期state保存に失敗した: %v", err)
	}

	currentFive := quota("codex", "primary", fiveHourWindowMinutes, 0, 10_000+5*60*60, 2)
	currentWeekly := quota("codex", "secondary", weeklyWindowMinutes, 0, 20_000, 2)
	cfg := config{codexPath: scriptPath, statePath: statePath, prompt: "Reply only: OK"}
	if err := processCurrentSnapshot(context.Background(), cfg, quotaSnapshot{FiveHour: &currentFive, Weekly: &currentWeekly}); err != nil {
		t.Fatalf("5h reset処理に失敗した: %v", err)
	}
	if got := countAnchorCalls(t, callsPath); got != 1 {
		t.Fatalf("5h resetのanchor回数が不正: %d", got)
	}

	saved, _, err := loadMonitorState(statePath)
	if err != nil {
		t.Fatalf("state読込に失敗した: %v", err)
	}
	for name, state := range map[string]*quotaState{"5h": saved.FiveHour, "weekly": saved.Weekly} {
		if state == nil || state.ResetDetector == nil || !state.ResetDetector.WasAboveThreshold {
			t.Fatalf("%s detectorがanchor後にrearmされていない: %+v", name, state)
		}
		if state.ResetDetector.ResetBoundary != state.ResetsAt {
			t.Fatalf("%s detector boundaryが現在値と一致しない: %+v", name, state)
		}
	}
}

func TestWeeklyResetAnchorsOnceAndRearmsAllWindows(t *testing.T) {
	scriptPath, callsPath := writeAnchorStub(t, 0)
	statePath := filepath.Join(t.TempDir(), "state.json")
	previousFive := initializedQuota("codex", "primary", fiveHourWindowMinutes, 0, 10_000, 1)
	previousWeekly := initializedQuota("codex", "secondary", weeklyWindowMinutes, 100, 20_000, 1)
	if err := saveMonitorState(statePath, monitorState{FiveHour: &previousFive, Weekly: &previousWeekly}); err != nil {
		t.Fatalf("初期state保存に失敗した: %v", err)
	}

	currentFive := quota("codex", "primary", fiveHourWindowMinutes, 0, 10_000, 2)
	currentWeekly := quota("codex", "secondary", weeklyWindowMinutes, 0, 20_000+7*24*60*60, 2)
	cfg := config{codexPath: scriptPath, statePath: statePath, prompt: "Reply only: OK"}
	if err := processCurrentSnapshot(context.Background(), cfg, quotaSnapshot{FiveHour: &currentFive, Weekly: &currentWeekly}); err != nil {
		t.Fatalf("weekly reset処理に失敗した: %v", err)
	}
	if got := countAnchorCalls(t, callsPath); got != 1 {
		t.Fatalf("weekly resetのanchor回数が不正: %d", got)
	}

	saved, _, err := loadMonitorState(statePath)
	if err != nil {
		t.Fatalf("state読込に失敗した: %v", err)
	}
	if saved.FiveHour == nil || saved.FiveHour.ResetDetector == nil || !saved.FiveHour.ResetDetector.WasAboveThreshold {
		t.Fatalf("5h sibling detectorがrearmされていない: %+v", saved.FiveHour)
	}
	if saved.Weekly == nil || saved.Weekly.ResetDetector == nil || !saved.Weekly.ResetDetector.WasAboveThreshold {
		t.Fatalf("weekly detectorがrearmされていない: %+v", saved.Weekly)
	}
}

func TestSimultaneousResetsRunSingleAnchor(t *testing.T) {
	scriptPath, callsPath := writeAnchorStub(t, 0)
	statePath := filepath.Join(t.TempDir(), "state.json")
	previousFive := initializedQuota("codex", "primary", fiveHourWindowMinutes, 100, 10_000, 1)
	previousWeekly := initializedQuota("codex", "secondary", weeklyWindowMinutes, 100, 20_000, 1)
	if err := saveMonitorState(statePath, monitorState{FiveHour: &previousFive, Weekly: &previousWeekly}); err != nil {
		t.Fatalf("初期state保存に失敗した: %v", err)
	}

	currentFive := quota("codex", "primary", fiveHourWindowMinutes, 0, 10_000+5*60*60, 2)
	currentWeekly := quota("codex", "secondary", weeklyWindowMinutes, 0, 20_000+7*24*60*60, 2)
	cfg := config{codexPath: scriptPath, statePath: statePath, prompt: "Reply only: OK"}
	if err := processCurrentSnapshot(context.Background(), cfg, quotaSnapshot{FiveHour: &currentFive, Weekly: &currentWeekly}); err != nil {
		t.Fatalf("同時reset処理に失敗した: %v", err)
	}
	if got := countAnchorCalls(t, callsPath); got != 1 {
		t.Fatalf("同時resetでanchorが1回ではない: %d", got)
	}
}

func TestFailedAnchorDoesNotAdvanceMonitorState(t *testing.T) {
	temporaryDirectory := t.TempDir()
	scriptPath := filepath.Join(temporaryDirectory, "codex")
	script := "#!/bin/sh\nprintf 'anchor failed\\n' >&2\nexit 2\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("stub codexを書けなかった: %v", err)
	}

	statePath := filepath.Join(temporaryDirectory, "state.json")
	previousFive := initializedQuota("codex", "primary", fiveHourWindowMinutes, 100, 10_000, 1)
	previousWeekly := initializedQuota("codex", "secondary", weeklyWindowMinutes, 71, 20_000, 1)
	previous := monitorState{Version: stateVersion, FiveHour: &previousFive, Weekly: &previousWeekly}
	if err := saveMonitorState(statePath, previous); err != nil {
		t.Fatalf("初期state保存に失敗した: %v", err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("初期state読込に失敗した: %v", err)
	}

	currentFive := quota("codex", "primary", fiveHourWindowMinutes, 0, 10_000+5*60*60, 2)
	currentWeekly := quota("codex", "secondary", weeklyWindowMinutes, 71, 20_000, 2)
	cfg := config{codexPath: scriptPath, statePath: statePath, prompt: "Reply only: OK"}
	if err := processCurrentSnapshot(context.Background(), cfg, quotaSnapshot{FiveHour: &currentFive, Weekly: &currentWeekly}); err == nil {
		t.Fatal("anchor失敗が成功扱いになった")
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("失敗後state読込に失敗した: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("anchor失敗時にstateが進んだ:\nBEFORE:\n%s\nAFTER:\n%s", before, after)
	}
}

func TestLegacySingleWindowStateMigratesToWeekly(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	legacy := initializedQuota("codex", "secondary", weeklyWindowMinutes, 71, 20_000, 1)
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("legacy JSON生成に失敗した: %v", err)
	}
	if err := os.WriteFile(statePath, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("legacy state保存に失敗した: %v", err)
	}

	state, found, err := loadMonitorState(statePath)
	if err != nil || !found {
		t.Fatalf("legacy state migrationに失敗した: found=%v err=%v", found, err)
	}
	if state.Version != stateVersion || state.Weekly == nil || state.FiveHour != nil {
		t.Fatalf("legacy weeklyのmigration結果が不正: %+v", state)
	}
	if !reflect.DeepEqual(*state.Weekly, legacy) {
		t.Fatalf("legacy weekly内容が変化した:\n got=%+v\nwant=%+v", *state.Weekly, legacy)
	}
}

func TestLegacyWeeklyMigrationAddsFiveHourWithoutSpuriousAnchor(t *testing.T) {
	scriptPath, callsPath := writeAnchorStub(t, 0)
	statePath := filepath.Join(t.TempDir(), "state.json")
	legacy := initializedQuota("codex", "secondary", weeklyWindowMinutes, 71, 20_000, 1)
	data, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(statePath, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("legacy state保存に失敗した: %v", err)
	}

	five := quota("codex", "primary", fiveHourWindowMinutes, 50, 10_000, 2)
	weekly := quota("codex", "secondary", weeklyWindowMinutes, 71, 20_000, 2)
	cfg := config{codexPath: scriptPath, statePath: statePath, prompt: "Reply only: OK"}
	if err := processCurrentSnapshot(context.Background(), cfg, quotaSnapshot{FiveHour: &five, Weekly: &weekly}); err != nil {
		t.Fatalf("migration後のsnapshot処理に失敗した: %v", err)
	}
	if got := countAnchorCalls(t, callsPath); got != 0 {
		t.Fatalf("5h detector追加だけでanchorした: %d", got)
	}
	state, _, err := loadMonitorState(statePath)
	if err != nil || state.FiveHour == nil || state.Weekly == nil {
		t.Fatalf("migration後に両windowが保存されていない: state=%+v err=%v", state, err)
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
	_, err = server.call(context.Background(), "account/rateLimits/read", nil, 100*time.Millisecond)
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
	cfg := config{codexPath: scriptPath, statePath: statePath, prompt: "Reply only: OK", anchorModel: "test-model"}

	stdout, stderr, err := captureOutput(func() error { return runAnchor(context.Background(), cfg) })
	if err != nil {
		t.Fatalf("アンカー実行に失敗した: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("anchor出力が漏れた: stdout=%q stderr=%q", stdout, stderr)
	}

	argumentsData, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatalf("引数記録を読めなかった: %v", err)
	}
	actualArguments := strings.Split(strings.TrimSuffix(string(argumentsData), "\n"), "\n")
	expectedArguments := []string{
		"--ask-for-approval", "never", "exec", "--ephemeral", "--skip-git-repo-check",
		"--sandbox", "read-only", "--color", "never", "--model", "test-model", "Reply only: OK",
	}
	if !reflect.DeepEqual(actualArguments, expectedArguments) {
		t.Fatalf("引数が不正:\n got=%q\nwant=%q", actualArguments, expectedArguments)
	}

	workingDirectoryData, err := os.ReadFile(workingDirectoryPath)
	if err != nil {
		t.Fatalf("作業ディレクトリ記録を読めなかった: %v", err)
	}
	if got, want := strings.TrimSpace(string(workingDirectoryData)), filepath.Dir(statePath); got != want {
		t.Fatalf("作業ディレクトリが不正: got=%q want=%q", got, want)
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

	cfg := config{codexPath: scriptPath, statePath: filepath.Join(temporaryDirectory, "state", "state.json"), prompt: "Reply only: OK"}
	stdout, stderr, err := captureOutput(func() error { return runAnchor(context.Background(), cfg) })
	if err == nil {
		t.Fatal("失敗するstubが成功扱いになった")
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("失敗時に直接出力が漏れた: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "exit status 2") || !strings.Contains(err.Error(), "invalid anchor arguments") {
		t.Fatalf("必要なfailure情報がない: %v", err)
	}
	if strings.Contains(err.Error(), "discarded stdout") {
		t.Fatalf("不要なstdoutがエラーへ混入した: %v", err)
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
