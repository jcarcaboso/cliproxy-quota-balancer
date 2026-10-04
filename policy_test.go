//go:build cgo

package main

import (
	"encoding/json"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var testNow = time.Unix(1_800_000_000, 0)

func codexCandidate(id, used string, remaining time.Duration) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{
		ID:       id,
		Provider: "codex",
		Quota: pluginapi.SchedulerQuotaSnapshot{
			ObservedAt: testNow,
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   used,
				"X-Codex-Primary-Window-Minutes": "300",
				"X-Codex-Primary-Reset-At":       strconv.FormatInt(testNow.Add(remaining).Unix(), 10),
			},
		},
	}
}

func TestPickQuotaAuth(t *testing.T) {
	tests := []struct {
		name       string
		candidates []pluginapi.SchedulerAuthCandidate
		wantID     string
		reject     bool
	}{
		{
			name:       "earliest reset wins regardless of candidate order",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("later", "20", 4*time.Hour), codexCandidate("soon", "50", 2*time.Hour)},
			wantID:     "soon",
		},
		{
			name:       "reserve at exactly ten percent",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("reserved", "90", 2*time.Hour), codexCandidate("later", "20", 4*time.Hour)},
			wantID:     "later",
		},
		{
			name:       "less than ten percent is also reserved",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("reserved", "99", 2*time.Hour), codexCandidate("later", "20", 4*time.Hour)},
			wantID:     "later",
		},
		{
			name:       "above ten percent remains eligible",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("soon", "89.99", 2*time.Hour), codexCandidate("later", "20", 4*time.Hour)},
			wantID:     "soon",
		},
		{
			name:       "reserve released in final hour",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("urgent", "99", 30*time.Minute), codexCandidate("later", "20", 4*time.Hour)},
			wantID:     "urgent",
		},
		{
			name:       "reserve released at exactly one hour",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("urgent", "90", time.Hour), codexCandidate("later", "20", 4*time.Hour)},
			wantID:     "urgent",
		},
		{
			name:       "reserve held one second before release",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("reserved", "90", time.Hour+time.Second)},
			reject:     true,
		},
		{
			name:       "exhausted quota is not usable even near reset",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("empty", "100", 30*time.Minute), codexCandidate("later", "20", 4*time.Hour)},
			wantID:     "later",
		},
		{
			name:       "all accounts reserved",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("a", "90", 2*time.Hour), codexCandidate("b", "95", 3*time.Hour)},
			reject:     true,
		},
		{
			name:       "single reserved account does not bypass policy",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("only", "90", 2*time.Hour)},
			reject:     true,
		},
		{
			name:       "unknown quota preferred over spending reserve",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("reserved", "90", 2*time.Hour), {ID: "unknown"}},
			wantID:     "unknown",
		},
		{
			name:       "known eligible window preferred over unknown",
			candidates: []pluginapi.SchedulerAuthCandidate{{ID: "unknown"}, codexCandidate("known", "50", 2*time.Hour)},
			wantID:     "known",
		},
		{
			name:       "expired exhausted observation is unknown",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("expired", "100", 0)},
			wantID:     "expired",
		},
		{
			name:       "ties resolved by stable account id",
			candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("b", "50", 2*time.Hour), codexCandidate("a", "50", 2*time.Hour)},
			wantID:     "a",
		},
		{name: "no candidates", reject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cursor atomic.Uint64
			got := pickQuotaAuth(tt.candidates, testNow, &cursor, defaultConfig())
			if !got.Handled || got.AuthID != tt.wantID || got.Reject != tt.reject {
				t.Fatalf("pick = %#v, want auth %q, reject %v", got, tt.wantID, tt.reject)
			}
		})
	}
}

func TestQuotaFallbackRoundRobin(t *testing.T) {
	var cursor atomic.Uint64
	for _, want := range []string{"a", "b", "a", "b"} {
		got := pickQuotaAuth([]pluginapi.SchedulerAuthCandidate{{ID: "b"}, {ID: "a"}, codexCandidate("reserved", "90", 2*time.Hour)}, testNow, &cursor, defaultConfig())
		if got.AuthID != want {
			t.Fatalf("fallback selected %q, want %q", got.AuthID, want)
		}
	}
}

func TestCandidateWindowCodexSecondaryAndRelativeReset(t *testing.T) {
	candidate := codexCandidate("account", "20", 2*time.Hour)
	candidate.Quota.ObservedAt = testNow.Add(-30 * time.Minute)
	candidate.Quota.Signals = map[string]string{
		"x-codex-secondary-used-percent":        "95",
		"x-codex-secondary-window-minutes":      "300",
		"x-codex-secondary-reset-after-seconds": "5400",
		"X-Codex-Primary-Used-Percent":          "20",
		"X-Codex-Primary-Window-Minutes":        "10080",
		"X-Codex-Primary-Reset-At":              strconv.FormatInt(testNow.Add(24*time.Hour).Unix(), 10),
	}
	windows, exhausted := candidateWindows(candidate, testNow)
	if len(windows) != 2 || exhausted || windows[1].used != 95 || !windows[1].reset.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("windows = %#v, exhausted %v", windows, exhausted)
	}
	candidate.Quota.Signals["X-Codex-Primary-Used-Percent"] = "100"
	_, exhausted = candidateWindows(candidate, testNow)
	if !exhausted {
		t.Fatal("exhausted weekly quota must block an otherwise usable five-hour window")
	}
}

func TestCandidateWindowClaude(t *testing.T) {
	candidate := pluginapi.SchedulerAuthCandidate{
		ID:       "claude",
		Provider: "claude",
		Quota: pluginapi.SchedulerQuotaSnapshot{
			ObservedAt: testNow,
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.95",
				"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(testNow.Add(30*time.Minute).Unix(), 10),
			},
		},
	}
	windows, exhausted := candidateWindows(candidate, testNow)
	if len(windows) != 1 || exhausted || windows[0].used != 95 {
		t.Fatalf("Claude windows = %#v, exhausted %v", windows, exhausted)
	}
	var cursor atomic.Uint64
	if got := pickQuotaAuth([]pluginapi.SchedulerAuthCandidate{candidate}, testNow, &cursor, defaultConfig()); got.AuthID != "claude" {
		t.Fatalf("final-hour Claude pick = %#v", got)
	}
	candidate.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(testNow.Add(24*time.Hour).Unix(), 10)
	candidate.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Status"] = "rejected"
	_, exhausted = candidateWindows(candidate, testNow)
	if !exhausted {
		t.Fatal("Claude weekly rejection must block the account")
	}
}

func TestCandidateWindowInvalidSignals(t *testing.T) {
	for _, raw := range []string{"", "invalid", "NaN", "+Inf", "-1", "101"} {
		t.Run(raw, func(t *testing.T) {
			candidate := codexCandidate("account", raw, 2*time.Hour)
			if windows, _ := candidateWindows(candidate, testNow); len(windows) != 0 {
				t.Fatalf("invalid used percentage %q was accepted", raw)
			}
		})
	}
	for _, tt := range []struct {
		name   string
		change func(*pluginapi.SchedulerAuthCandidate)
	}{
		{"missing observation time", func(c *pluginapi.SchedulerAuthCandidate) { c.Quota.ObservedAt = time.Time{} }},
		{"future observation", func(c *pluginapi.SchedulerAuthCandidate) { c.Quota.ObservedAt = testNow.Add(time.Second) }},
		{"unknown provider", func(c *pluginapi.SchedulerAuthCandidate) { c.Provider = "other" }},
		{"unknown window duration", func(c *pluginapi.SchedulerAuthCandidate) { c.Quota.Signals["X-Codex-Primary-Window-Minutes"] = "60" }},
		{"invalid reset", func(c *pluginapi.SchedulerAuthCandidate) { c.Quota.Signals["X-Codex-Primary-Reset-At"] = "bad" }},
		{"relative overflow", func(c *pluginapi.SchedulerAuthCandidate) {
			delete(c.Quota.Signals, "X-Codex-Primary-Reset-At")
			c.Quota.Signals["X-Codex-Primary-Reset-After-Seconds"] = "9223372036854775807"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			candidate := codexCandidate("account", "50", 2*time.Hour)
			tt.change(&candidate)
			if windows, _ := candidateWindows(candidate, testNow); len(windows) != 0 {
				t.Fatal("invalid observation was accepted")
			}
		})
	}
}

func TestPluginLifecycleAndStrategies(t *testing.T) {
	t.Cleanup(func() { currentConfig.Store(defaultConfig()) })
	for _, strategy := range []string{"", earliestReset, pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst} {
		raw, errMarshal := json.Marshal(struct {
			ConfigYAML []byte `json:"config_yaml"`
		}{ConfigYAML: []byte("strategy: " + strconv.Quote(strategy))})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		registered, errRegister := handleMethod(pluginabi.MethodPluginRegister, raw)
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		var envelope pluginabi.Envelope
		if errUnmarshal := json.Unmarshal(registered, &envelope); errUnmarshal != nil || !envelope.OK {
			t.Fatalf("registration = %s, error %v", registered, errUnmarshal)
		}
		var registration struct {
			Metadata     pluginapi.Metadata `json:"metadata"`
			Capabilities map[string]bool    `json:"capabilities"`
		}
		if errUnmarshal := json.Unmarshal(envelope.Result, &registration); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		if registration.Metadata.Name == "" || registration.Metadata.Version == "" ||
			registration.Metadata.Author == "" || registration.Metadata.GitHubRepository == "" ||
			!registration.Capabilities["scheduler"] {
			t.Fatalf("registration is missing host-required fields: %#v", registration)
		}
		raw, errPick := handleMethod(pluginabi.MethodSchedulerPick, []byte(`{"Candidates":[]}`))
		if errPick != nil {
			t.Fatal(errPick)
		}
		if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		var response pluginapi.SchedulerPickResponse
		if errUnmarshal := json.Unmarshal(envelope.Result, &response); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		if strategy == "" || strategy == earliestReset {
			if !response.Handled || !response.Reject {
				t.Fatalf("empty candidate pick = %#v", response)
			}
		} else if !response.Handled || response.DelegateBuiltin != strategy {
			t.Fatalf("strategy %q pick = %#v", strategy, response)
		}
	}
	invalid := []byte(`{"config_yaml":"c3RyYXRlZ3k6IGludmFsaWQ="}`)
	if _, errReconfigure := handleMethod(pluginabi.MethodPluginReconfigure, invalid); errReconfigure == nil {
		t.Fatal("invalid reconfiguration was accepted")
	}
	if cfg := currentConfig.Load().(pluginConfig); cfg.Strategy != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatal("invalid reconfiguration replaced the working configuration")
	}
	if _, errPick := handleMethod(pluginabi.MethodSchedulerPick, []byte("invalid JSON")); errPick == nil {
		t.Fatal("invalid scheduler request was accepted")
	}
}
