//go:build cgo

// verify loads the compiled library through the real CLIProxyAPI plugin host.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	"gopkg.in/yaml.v3"
)

func main() {
	directory := flag.String("plugin-dir", "dist", "Directory containing quota-balancer.so")
	flag.Parse()
	if errVerify := verify(*directory); errVerify != nil {
		fmt.Fprintln(os.Stderr, errVerify)
		os.Exit(1)
	}
}

func verify(directory string) error {
	host := pluginhost.New()
	defer host.ShutdownAll()
	configure := func(policy string) {
		enabled := true
		var raw yaml.Node
		if errDecode := yaml.Unmarshal([]byte(policy), &raw); errDecode != nil {
			panic(errDecode) // Constant test fixtures only.
		}
		host.ApplyConfig(context.Background(), pluginhost.RuntimeConfig{
			Enabled: true, Dir: directory,
			Configs: map[string]pluginhost.PluginInstanceConfig{
				"quota-balancer": {Enabled: &enabled, Priority: 100, Raw: raw},
			},
		})
	}
	configure("enabled: true\npriority: 100\nstrategy: earliest-reset\n")
	if !host.HasScheduler() {
		return fmt.Errorf("compiled quota-balancer library did not register")
	}
	now := time.Now()
	account := func(id, used string, reset time.Duration, weeklyUsed string, weeklyReset time.Duration) pluginapi.SchedulerAuthCandidate {
		headers := http.Header{
			"X-Codex-Primary-Used-Percent":   []string{used},
			"X-Codex-Primary-Window-Minutes": []string{"300"},
			"X-Codex-Primary-Reset-At":       []string{strconv.FormatInt(now.Add(reset).Unix(), 10)},
		}
		if weeklyUsed != "" {
			headers.Set("X-Codex-Secondary-Used-Percent", weeklyUsed)
			headers.Set("X-Codex-Secondary-Window-Minutes", "10080")
			headers.Set("X-Codex-Secondary-Reset-At", strconv.FormatInt(now.Add(weeklyReset).Unix(), 10))
		}
		var quota coreauth.QuotaState
		if !quota.ObserveResponseHeadersForProvider("codex", headers, now) {
			panic("host did not collect quota test fixture")
		}
		return pluginapi.SchedulerAuthCandidate{
			ID: id, Provider: "codex",
			Quota: pluginapi.SchedulerQuotaSnapshot{ObservedAt: quota.ObservedAt, Signals: quota.Signals},
		}
	}
	pick := func(name string, candidates []pluginapi.SchedulerAuthCandidate, want string, reject bool) error {
		for _, stream := range []bool{false, true} {
			response, handled, errPick := host.PickAuth(context.Background(), pluginapi.SchedulerPickRequest{
				Provider: "codex", Stream: stream, Candidates: candidates,
			})
			if errPick != nil || !handled || !response.Handled || response.AuthID != want || response.Reject != reject {
				return fmt.Errorf("%s stream=%v: response=%#v handled=%v error=%v", name, stream, response, handled, errPick)
			}
		}
		fmt.Println("PASS:", name)
		return nil
	}
	available := account("available", "50", 4*time.Hour, "50", 48*time.Hour)
	tests := []struct {
		name       string
		candidates []pluginapi.SchedulerAuthCandidate
		want       string
		reject     bool
	}{
		{"reserve held", []pluginapi.SchedulerAuthCandidate{account("held", "95", 2*time.Hour, "", 0), available}, "available", false},
		{"reserve released", []pluginapi.SchedulerAuthCandidate{account("urgent", "95", 30*time.Minute, "", 0), available}, "urgent", false},
		{"weekly reset beats five-hour reset", []pluginapi.SchedulerAuthCandidate{
			account("weekly", "50", 3*time.Hour, "95", 15*time.Minute),
			account("five-hour", "50", 30*time.Minute, "", 0),
		}, "weekly", false},
		{"weekly urgency cannot spend five-hour reserve", []pluginapi.SchedulerAuthCandidate{
			account("held", "95", 3*time.Hour, "95", 15*time.Minute), available,
		}, "available", false},
		{"weekly exhaustion excluded", []pluginapi.SchedulerAuthCandidate{
			account("empty", "50", 30*time.Minute, "100", 24*time.Hour), available,
		}, "available", false},
		{"all reserved pool rejected", []pluginapi.SchedulerAuthCandidate{account("held", "95", 2*time.Hour, "", 0)}, "", true},
		{"unknown account does not spend a known reserve", []pluginapi.SchedulerAuthCandidate{
			account("held", "95", 2*time.Hour, "", 0), {ID: "unknown", Provider: "codex"},
		}, "unknown", false},
	}
	for _, test := range tests {
		if errPick := pick(test.name, test.candidates, test.want, test.reject); errPick != nil {
			return errPick
		}
	}
	configure("enabled: true\npriority: 100\nstrategy: earliest-reset\nreserve-percent: 25\nrelease-within: 30m\n")
	for _, test := range []struct {
		name string
		when time.Duration
		want string
	}{
		{"custom reserve threshold applied after reconfigure", 2 * time.Hour, "available"},
		{"custom release threshold applied after reconfigure", 15 * time.Minute, "custom"},
	} {
		if errPick := pick(test.name, []pluginapi.SchedulerAuthCandidate{account("custom", "80", test.when, "", 0), available}, test.want, false); errPick != nil {
			return errPick
		}
	}
	fmt.Println("PASS: native C ABI, host quota collection, streaming/non-streaming picks, and hot reconfiguration")
	return nil
}
