package main

import "testing"

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
		t.Fatalf("境界を保持していない: got=%d want=%d", next.ResetDetector.ResetBoundary, previous.ResetsAt)
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
