package app

import (
	"testing"
	"time"
)

func TestParseConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := parseConfig(nil, "/tmp/home")
	if err != nil {
		t.Fatalf("default configを解析できなかった: %v", err)
	}
	if cfg.monitorConfig.PollEvery != 5*time.Minute || cfg.monitorConfig.Prompt != "Reply only: OK" || cfg.codexPath != "codex" {
		t.Fatalf("default configが不正: %+v", cfg)
	}
	if cfg.monitorConfig.StatePath != "/tmp/home/.local/var/codex-reset-anchor/state.json" {
		t.Fatalf("default state pathが不正: %s", cfg.monitorConfig.StatePath)
	}
	if _, err := parseConfig([]string{"-interval", "30s"}, "/tmp/home"); err == nil {
		t.Fatal("1分未満のintervalを許可した")
	}
}

func TestParseConfigRejectsInvalidAnchorTimeout(t *testing.T) {
	_, err := parseConfig([]string{"-anchor-timeout", "0s"}, t.TempDir())
	if err == nil {
		t.Fatal("0秒のanchor timeoutを受理した")
	}
}
