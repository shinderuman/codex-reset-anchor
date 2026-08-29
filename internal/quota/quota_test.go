package quota

import (
	"testing"
	"time"
)

func testWindow(duration int64, used float64, resetsAt int64, checkedAt time.Time) Window {
	return Window{
		LimitID:           "codex",
		WindowName:        "primary",
		WindowDurationMin: duration,
		UsedPercent:       used,
		ResetsAt:          resetsAt,
		CheckedAt:         checkedAt.UnixNano(),
	}
}

func TestRecoveredWhenKnownResetBoundaryElapsed(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := testWindow(FiveHourWindowMinutes, 0, resetAt.Unix(), resetAt.Add(-time.Minute))
	current := testWindow(FiveHourWindowMinutes, 0, resetAt.Add(5*time.Hour).Unix(), resetAt.Add(time.Minute))

	if !Recovered(previous, current) {
		t.Fatal("既知のリセット時刻を通過した低使用率windowを回復として検知できなかった")
	}
}

func TestBoundaryMoveBeforeResetDoesNotRecover(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 18, 0, 0, 0, time.UTC)
	previous := testWindow(FiveHourWindowMinutes, 0, resetAt.Unix(), resetAt.Add(-4*time.Hour))
	current := testWindow(FiveHourWindowMinutes, 0, resetAt.Add(5*time.Minute).Unix(), resetAt.Add(-4*time.Hour+5*time.Minute))

	if Recovered(previous, current) {
		t.Fatal("リセット前のresetsAt移動を回復として誤検知した")
	}
}

func TestUsedQuotaAfterResetDoesNotNeedAnchor(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := testWindow(WeeklyWindowMinutes, 80, resetAt.Unix(), resetAt.Add(-time.Minute))
	current := testWindow(WeeklyWindowMinutes, 3, resetAt.Add(7*24*time.Hour).Unix(), resetAt.Add(time.Minute))

	if Recovered(previous, current) {
		t.Fatal("リセット後に既に利用されているwindowをanchor対象にした")
	}
}

func TestSmallBoundaryJitterDoesNotRecover(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := testWindow(WeeklyWindowMinutes, 100, resetAt.Unix(), resetAt.Add(-time.Minute))
	current := testWindow(WeeklyWindowMinutes, 0, resetAt.Add(time.Minute).Unix(), resetAt.Add(time.Minute))

	if Recovered(previous, current) {
		t.Fatal("小さな境界差を回復として誤検知した")
	}
}

func TestWindowIdentityChangeDoesNotRecover(t *testing.T) {
	resetAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	previous := testWindow(FiveHourWindowMinutes, 100, resetAt.Unix(), resetAt.Add(-time.Minute))
	current := testWindow(WeeklyWindowMinutes, 0, resetAt.Add(7*24*time.Hour).Unix(), resetAt.Add(time.Minute))

	if Recovered(previous, current) {
		t.Fatal("異なるwindowを同一resetとして扱った")
	}
}
