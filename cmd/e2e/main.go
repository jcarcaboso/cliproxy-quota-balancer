// e2e tests the real proxy's HTTP routing against a local, non-billable Codex server.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type accountQuota struct {
	shortUsed, weeklyUsed   int
	shortReset, weeklyReset time.Duration
}

func main() {
	server := flag.String("server", "/CLIProxyAPI/CLIProxyAPI", "Proxy binary")
	plugins := flag.String("plugin-dir", "/CLIProxyAPI/plugins", "Plugin directory")
	flag.Parse()
	if errTest := run(*server, *plugins); errTest != nil {
		fmt.Fprintln(os.Stderr, errTest)
		os.Exit(1)
	}
}

func run(server, plugins string) error {
	available := accountQuota{50, 50, 4 * time.Hour, 48 * time.Hour}
	tests := []struct {
		name string
		a, b accountQuota
		want string
	}{
		{"five-hour reserve held", accountQuota{95, 50, 2 * time.Hour, 48 * time.Hour}, available, "b"},
		{"five-hour reserve released", accountQuota{95, 50, 30 * time.Minute, 48 * time.Hour}, available, "a"},
		{"weekly reset wins", accountQuota{50, 95, 3 * time.Hour, 15 * time.Minute}, accountQuota{50, 50, 30 * time.Minute, 48 * time.Hour}, "a"},
		{"other window reserve still holds", accountQuota{95, 95, 3 * time.Hour, 15 * time.Minute}, available, "b"},
		{"weekly exhaustion excluded", accountQuota{50, 100, 30 * time.Minute, 24 * time.Hour}, available, "b"},
		{"all reserved pool uses earliest reset", accountQuota{95, 50, 2 * time.Hour, 48 * time.Hour}, accountQuota{95, 50, 3 * time.Hour, 48 * time.Hour}, "a"},
		{"reserved five-hour account precedes weekly-only account", accountQuota{95, -1, 2 * time.Hour, 0}, accountQuota{-1, 95, 0, 24 * time.Hour}, "a"},
		{"exhausted five-hour account falls back to weekly-only reserve", accountQuota{100, -1, 2 * time.Hour, 0}, accountQuota{-1, 95, 0, 24 * time.Hour}, "b"},
		{"all exhausted pool rejects without upstream request", accountQuota{100, 50, 2 * time.Hour, 48 * time.Hour}, accountQuota{-1, 100, 0, 24 * time.Hour}, ""},
	}
	for _, test := range tests {
		for _, affinity := range []bool{false, true} {
			if errTest := scenario(server, plugins, test.a, test.b, test.want, affinity); errTest != nil {
				return fmt.Errorf("%s affinity=%v: %w", test.name, affinity, errTest)
			}
		}
		fmt.Println("PASS HTTP:", test.name)
	}
	return nil
}

func scenario(server, plugins string, a, b accountQuota, want string, affinity bool) error {
	now := time.Now()
	var mutex sync.Mutex
	var selected []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer synthetic-")
		quota := a
		if id == "b" {
			quota = b
		}
		mutex.Lock()
		selected = append(selected, id)
		mutex.Unlock()
		for _, window := range []struct {
			name          string
			minutes, used int
			reset         time.Duration
		}{{"Primary", 300, quota.shortUsed, quota.shortReset}, {"Secondary", 10080, quota.weeklyUsed, quota.weeklyReset}} {
			if window.used < 0 {
				continue // A negative fixture utilization means this limit is absent.
			}
			prefix := "X-Codex-" + window.name + "-"
			w.Header().Set(prefix+"Used-Percent", strconv.Itoa(window.used))
			w.Header().Set(prefix+"Window-Minutes", strconv.Itoa(window.minutes))
			w.Header().Set(prefix+"Reset-At", strconv.FormatInt(now.Add(window.reset).Unix(), 10))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		return errListen
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if errClose := listener.Close(); errClose != nil {
		return errClose
	}
	directory, errTemp := os.MkdirTemp("", "quota-e2e-*")
	if errTemp != nil {
		return errTemp
	}
	defer func() {
		if errRemove := os.RemoveAll(directory); errRemove != nil {
			fmt.Fprintln(os.Stderr, "remove test directory:", errRemove)
		}
	}()
	groups := []any{}
	for _, id := range []string{"a", "b"} {
		groups = append(groups, map[string]any{
			"name": "synthetic-" + id, "base-url": upstream.URL,
			"keys": []any{map[string]any{"api-key": "synthetic-" + id}},
			"models": []any{
				map[string]any{"name": "gpt-5.5", "alias": "quota-common"},
				map[string]any{"name": "gpt-5.5", "alias": "seed-" + id},
			},
		})
	}
	config, errEncode := yaml.Marshal(map[string]any{
		"config-version": 8,
		"server":         map[string]any{"host": "127.0.0.1", "port": port},
		"access":         map[string]any{"api-keys": []string{"synthetic-client"}},
		"oauth":          map[string]any{"auth-dir": filepath.Join(directory, "auth")},
		"api-keys":       map[string]any{"codex": groups},
		"routing":        map[string]any{"session-affinity": affinity},
		"plugins": map[string]any{
			"enabled": true, "dir": plugins,
			"configs": map[string]any{"quota-balancer": map[string]any{
				"enabled": true, "priority": 100, "strategy": "earliest-reset",
			}},
		},
	})
	if errEncode != nil {
		return errEncode
	}
	path := filepath.Join(directory, "config.yaml")
	if errWrite := os.WriteFile(path, config, 0600); errWrite != nil {
		return errWrite
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, server, "-config", path, "-local-model")
	command.Dir = directory
	var logs bytes.Buffer
	command.Stdout, command.Stderr = &logs, &logs
	if errStart := command.Start(); errStart != nil {
		return errStart
	}
	defer func() { cancel(); _ = command.Wait() }()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	// This deadline bounds test setup only; production upstream behavior is unchanged.
	readyDeadline := time.Now().Add(15 * time.Second)
	for {
		response, errGet := http.Get(base + "/")
		if errGet == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(readyDeadline) {
			cancel()
			_ = command.Wait()
			return fmt.Errorf("proxy failed to start: %s", logs.String())
		}
		// Startup readiness is not a TTL, cache ordering, or expiry test.
		time.Sleep(20 * time.Millisecond)
	}
	post := func(model string, stream bool) (int, error) {
		raw, _ := json.Marshal(map[string]any{"model": model, "input": "synthetic routing test", "stream": stream})
		request, errRequest := http.NewRequest(http.MethodPost, base+"/v1/responses", bytes.NewReader(raw))
		if errRequest != nil {
			return 0, errRequest
		}
		request.Header.Set("Authorization", "Bearer synthetic-client")
		request.Header.Set("Content-Type", "application/json")
		response, errDo := http.DefaultClient.Do(request)
		if errDo != nil {
			return 0, errDo
		}
		body, errRead := io.ReadAll(response.Body)
		errClose := response.Body.Close()
		if errRead != nil || errClose != nil {
			return response.StatusCode, fmt.Errorf("response read=%v close=%v", errRead, errClose)
		}
		if model != "quota-common" && response.StatusCode != 200 {
			return response.StatusCode, fmt.Errorf("seed %s failed: %s", model, body)
		}
		return response.StatusCode, nil
	}
	for _, model := range []string{"seed-a", "seed-b"} {
		if _, errPost := post(model, false); errPost != nil {
			return errPost
		}
	}
	for _, stream := range []bool{false, true} {
		mutex.Lock()
		before := len(selected)
		mutex.Unlock()
		status, errPost := post("quota-common", stream)
		if errPost != nil {
			return errPost
		}
		mutex.Lock()
		after := slices.Clone(selected)
		mutex.Unlock()
		if want == "" {
			if status < 400 || len(after) != before {
				return fmt.Errorf("expected rejection without upstream traffic; status=%d selected=%v", status, after)
			}
		} else if status != 200 || len(after) != before+1 || after[len(after)-1] != want {
			return fmt.Errorf("stream=%v status=%d selected=%v, want %s", stream, status, after, want)
		}
	}
	return nil
}
