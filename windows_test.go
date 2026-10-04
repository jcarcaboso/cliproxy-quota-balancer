//go:build cgo

package main

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func withWeekly(candidate pluginapi.SchedulerAuthCandidate, used string, remaining time.Duration) pluginapi.SchedulerAuthCandidate {
	candidate.Quota.Signals["X-Codex-Secondary-Used-Percent"] = used
	candidate.Quota.Signals["X-Codex-Secondary-Window-Minutes"] = "10080"
	candidate.Quota.Signals["X-Codex-Secondary-Reset-At"] = strconv.FormatInt(testNow.Add(remaining).Unix(), 10)
	return candidate
}

func TestWeeklyAndFiveHourResetOrdering(t *testing.T) {
	tests := []struct {
		name       string
		candidates []pluginapi.SchedulerAuthCandidate
		want       string
	}{
		{
			"weekly reset beats another account's five-hour reset",
			[]pluginapi.SchedulerAuthCandidate{
				codexCandidate("five-hour", "40", 30*time.Minute),
				withWeekly(codexCandidate("weekly", "50", 3*time.Hour), "95", 15*time.Minute),
			}, "weekly",
		},
		{
			"five-hour reset beats another account's weekly reset",
			[]pluginapi.SchedulerAuthCandidate{
				withWeekly(codexCandidate("weekly", "50", 3*time.Hour), "95", 45*time.Minute),
				codexCandidate("five-hour", "40", 30*time.Minute),
			}, "five-hour",
		},
		{
			"urgent weekly window does not spend another window's reserve",
			[]pluginapi.SchedulerAuthCandidate{
				withWeekly(codexCandidate("held", "95", 3*time.Hour), "95", 15*time.Minute),
				codexCandidate("available", "40", 30*time.Minute),
			}, "available",
		},
		{
			"weekly reserve blocks an urgent five-hour window",
			[]pluginapi.SchedulerAuthCandidate{
				withWeekly(codexCandidate("held", "95", 15*time.Minute), "95", 3*time.Hour),
				codexCandidate("available", "40", 30*time.Minute),
			}, "available",
		},
		{
			"expired weekly exhaustion no longer blocks the five-hour window",
			[]pluginapi.SchedulerAuthCandidate{
				withWeekly(codexCandidate("refreshed", "50", 15*time.Minute), "100", 0),
				codexCandidate("available", "40", 30*time.Minute),
			}, "refreshed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, candidates := range [][]pluginapi.SchedulerAuthCandidate{tt.candidates, {tt.candidates[1], tt.candidates[0]}} {
				var cursor atomic.Uint64
				got := pickQuotaAuth(candidates, testNow, &cursor, defaultConfig())
				if got.AuthID != tt.want {
					t.Fatalf("pick = %#v, want %s", got, tt.want)
				}
			}
		})
	}
}

func TestConfigurableThresholds(t *testing.T) {
	tests := []struct {
		reserve float64
		release time.Duration
		used    string
		until   time.Duration
		reject  bool
	}{
		{20, 30 * time.Minute, "80", 31 * time.Minute, true},
		{20, 30 * time.Minute, "80", 30 * time.Minute, false},
		{20, 30 * time.Minute, "79.99", time.Hour, false},
		{5, 2 * time.Hour, "95", 3 * time.Hour, true},
		{5, 2 * time.Hour, "95", 2 * time.Hour, false},
		{0, 0, "99", 3 * time.Hour, false},
		{0, 0, "100", time.Minute, true},
		{10, 0, "90", time.Second, true},
	}
	for _, tt := range tests {
		cfg := defaultConfig()
		cfg.ReservePercent, cfg.releaseWithin = tt.reserve, tt.release
		var cursor atomic.Uint64
		got := pickQuotaAuth([]pluginapi.SchedulerAuthCandidate{codexCandidate("account", tt.used, tt.until)}, testNow, &cursor, cfg)
		if got.Reject != tt.reject {
			t.Fatalf("reserve=%v release=%v used=%s until=%v pick=%#v", tt.reserve, tt.release, tt.used, tt.until, got)
		}
	}
}

func configureYAML(t *testing.T, yaml string) error {
	t.Helper()
	raw, errMarshal := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{[]byte(yaml)})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	_, errConfigure := handleMethod(pluginabi.MethodPluginReconfigure, raw)
	return errConfigure
}

func TestThresholdConfigValidation(t *testing.T) {
	t.Cleanup(func() { currentConfig.Store(defaultConfig()) })
	if errConfigure := configureYAML(t, "reserve-percent: 20\nrelease-within: 30m"); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	valid := currentConfig.Load().(pluginConfig)
	if valid.ReservePercent != 20 || valid.releaseWithin != 30*time.Minute {
		t.Fatalf("configuration = %#v", valid)
	}
	for _, yaml := range []string{
		"reserve-percent: -1", "reserve-percent: 100", "reserve-percent: .nan",
		"reserve-percent: .inf", "reserve-percent: text", "release-within: -1h",
		"release-within: invalid", "release-within: ''", "strategy: missing", "[invalid",
	} {
		if errConfigure := configureYAML(t, yaml); errConfigure == nil {
			t.Fatalf("invalid configuration %q accepted", yaml)
		}
		if currentConfig.Load().(pluginConfig) != valid {
			t.Fatalf("invalid configuration %q replaced working configuration", yaml)
		}
	}
	if errConfigure := configureYAML(t, "reserve-percent: 0\nrelease-within: 0s"); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	if cfg := currentConfig.Load().(pluginConfig); cfg.ReservePercent != 0 || cfg.releaseWithin != 0 {
		t.Fatalf("zero thresholds did not apply: %#v", cfg)
	}
	if errConfigure := configureYAML(t, ""); errConfigure != nil || currentConfig.Load().(pluginConfig) != defaultConfig() {
		t.Fatalf("defaults did not restore: %v", errConfigure)
	}
}

// The oracle uses generated window facts directly, independently of header parsing.
func TestRandomPoolsMatchPolicyOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 21))
	for iteration := 0; iteration < 5000; iteration++ {
		cfg := defaultConfig()
		cfg.ReservePercent = float64(rng.IntN(31))
		cfg.releaseWithin = time.Duration(rng.IntN(121)) * time.Minute
		type ranked struct {
			id    string
			reset time.Time
		}
		var eligible []ranked
		var candidates []pluginapi.SchedulerAuthCandidate
		for account := 0; account < 1+rng.IntN(10); account++ {
			id := strconv.Itoa(account)
			shortUsed, weeklyUsed := rng.IntN(101), rng.IntN(101)
			shortTime := time.Duration(1+rng.IntN(300)) * time.Minute
			weeklyTime := time.Duration(1+rng.IntN(10080)) * time.Minute
			candidates = append(candidates, withWeekly(codexCandidate(id, strconv.Itoa(shortUsed), shortTime), strconv.Itoa(weeklyUsed), weeklyTime))
			blocked := shortUsed == 100 || weeklyUsed == 100 ||
				(float64(shortUsed) >= 100-cfg.ReservePercent && shortTime > cfg.releaseWithin) ||
				(float64(weeklyUsed) >= 100-cfg.ReservePercent && weeklyTime > cfg.releaseWithin)
			if !blocked {
				eligible = append(eligible, ranked{id: id, reset: testNow.Add(min(shortTime, weeklyTime))})
			}
		}
		slices.SortFunc(eligible, func(a, b ranked) int {
			if result := a.reset.Compare(b.reset); result != 0 {
				return result
			}
			if a.id < b.id {
				return -1
			}
			if a.id > b.id {
				return 1
			}
			return 0
		})
		rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
		var cursor atomic.Uint64
		got := pickQuotaAuth(candidates, testNow, &cursor, cfg)
		if len(eligible) == 0 {
			if !got.Reject {
				t.Fatalf("iteration %d selected from a fully blocked pool: %#v", iteration, got)
			}
		} else if got.AuthID != eligible[0].id || got.Reject {
			t.Fatalf("iteration %d got %#v, want %s", iteration, got, eligible[0].id)
		}
	}
}
