//go:build cgo

package main

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type quotaWindow struct {
	used  float64
	reset time.Time
}

func pickQuotaAuth(candidates []pluginapi.SchedulerAuthCandidate, now time.Time, cursor *atomic.Uint64, cfg pluginConfig) pluginapi.SchedulerPickResponse {
	var selected string
	var earliest time.Time
	var fallback string
	var fallbackReset time.Time
	var unknown []string
	for _, candidate := range candidates {
		windows, exhausted := candidateWindows(candidate, now)
		if exhausted {
			continue
		}
		if len(windows) == 0 {
			unknown = append(unknown, candidate.ID)
			continue
		}
		var reset time.Time
		reserved := false
		for _, window := range windows {
			if window.used >= 100-cfg.ReservePercent && window.reset.Sub(now) > cfg.releaseWithin {
				reserved = true
			}
			if reset.IsZero() || window.reset.Before(reset) {
				reset = window.reset
			}
		}
		if reserved {
			if fallback == "" || reset.Before(fallbackReset) || (reset.Equal(fallbackReset) && candidate.ID < fallback) {
				fallback, fallbackReset = candidate.ID, reset
			}
			continue
		}
		if selected == "" || reset.Before(earliest) || (reset.Equal(earliest) && candidate.ID < selected) {
			selected, earliest = candidate.ID, reset
		}
	}
	if selected != "" {
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		index := (cursor.Add(1) - 1) % uint64(len(unknown))
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: unknown[index]}
	}
	if fallback != "" {
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: fallback}
	}
	return pluginapi.SchedulerPickResponse{
		Handled:      true,
		Reject:       true,
		RejectCode:   "auth_unavailable",
		RejectReason: "no candidate accounts with usable quota",
	}
}

func candidateWindows(candidate pluginapi.SchedulerAuthCandidate, now time.Time) ([]quotaWindow, bool) {
	quota := candidate.Quota
	if quota.ObservedAt.IsZero() || quota.ObservedAt.After(now) {
		return nil, false
	}
	signals := make(http.Header, len(quota.Signals))
	for key, value := range quota.Signals {
		signals.Set(key, value)
	}
	var windows []quotaWindow
	var exhausted bool
	switch strings.ToLower(strings.TrimSpace(candidate.Provider)) {
	case "codex":
		for _, name := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + name + "-"
			reset := windowReset(signals, prefix, quota.ObservedAt)
			if !reset.After(now) {
				continue
			}
			used, valid := quotaNumber(signals.Get(prefix+"Used-Percent"), 100)
			if !valid {
				continue
			}
			exhausted = exhausted || used >= 100 ||
				strings.EqualFold(signals.Get("X-Codex-Allowed"), "false") ||
				strings.EqualFold(signals.Get("X-Codex-Limit-Reached"), "true")
			minutes, errParse := strconv.Atoi(strings.TrimSpace(signals.Get(prefix + "Window-Minutes")))
			if errParse == nil && (minutes == 300 || minutes == 10080) {
				windows = append(windows, quotaWindow{used: used, reset: reset})
			}
		}
	case "claude":
		for _, name := range []string{"5h", "7d"} {
			prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
			reset := unixReset(signals.Get(prefix + "Reset"))
			if !reset.After(now) {
				continue
			}
			used, valid := quotaNumber(signals.Get(prefix+"Utilization"), 1)
			exhausted = exhausted || strings.EqualFold(signals.Get(prefix+"Status"), "rejected") || (valid && used >= 1)
			if valid {
				windows = append(windows, quotaWindow{used: used * 100, reset: reset})
			}
		}
	}
	return windows, exhausted
}

func quotaNumber(raw string, max float64) (float64, bool) {
	value, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return value, errParse == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= max
}

func unixReset(raw string) time.Time {
	seconds, errParse := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if errParse != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func windowReset(signals http.Header, prefix string, observedAt time.Time) time.Time {
	if reset := unixReset(signals.Get(prefix + "Reset-At")); !reset.IsZero() {
		return reset
	}
	seconds, errParse := strconv.ParseInt(strings.TrimSpace(signals.Get(prefix+"Reset-After-Seconds")), 10, 64)
	// Bound relative offsets before converting to Duration to prevent overflow.
	if errParse != nil || seconds < 0 || seconds > int64(365*24*time.Hour/time.Second) {
		return time.Time{}
	}
	return observedAt.Add(time.Duration(seconds) * time.Second)
}
