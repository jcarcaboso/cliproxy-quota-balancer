package deployconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestApplyPreservesHostStateAndBackup(t *testing.T) {
	directory := t.TempDir()
	path, policyPath := filepath.Join(directory, "config.yaml"), filepath.Join(directory, "policy.yaml")
	original := []byte(`# preserve unrelated configuration
port: 8317
auth-dir: /root/.cli-proxy-api
api-keys: [example-not-a-real-key]
remote-management: {secret-key: example-not-a-real-secret}
routing: {session-affinity: true}
plugins:
  enabled: false
  configs:
    other: {enabled: true, priority: 1, value: keep}
`)
	policy := []byte("enabled: true\npriority: 100\nstrategy: earliest-reset\nreserve-percent: 20\nrelease-within: 30m\n")
	if errWrite := os.WriteFile(path, original, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errWrite := os.WriteFile(policyPath, policy, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errApply := Apply(path, policyPath); errApply != nil {
		t.Fatal(errApply)
	}
	updated, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var before, after map[string]any
	if errDecode := yaml.Unmarshal(original, &before); errDecode != nil {
		t.Fatal(errDecode)
	}
	if errDecode := yaml.Unmarshal(updated, &after); errDecode != nil {
		t.Fatal(errDecode)
	}
	for key, value := range before {
		if key != "plugins" && !reflect.DeepEqual(value, after[key]) {
			t.Fatalf("unrelated host setting %s changed", key)
		}
	}
	plugins := after["plugins"].(map[string]any)
	if plugins["enabled"] != true || plugins["dir"] != "/CLIProxyAPI/plugins" {
		t.Fatalf("plugin loading not configured: %#v", plugins)
	}
	configs := plugins["configs"].(map[string]any)
	if !reflect.DeepEqual(configs["other"], before["plugins"].(map[string]any)["configs"].(map[string]any)["other"]) {
		t.Fatal("another plugin's configuration changed")
	}
	if configs["quota-balancer"].(map[string]any)["reserve-percent"] != 20 {
		t.Fatal("policy thresholds not installed")
	}
	if errApply := Apply(path, policyPath); errApply != nil {
		t.Fatal(errApply)
	}
	repeated, _ := os.ReadFile(path)
	backup, _ := os.ReadFile(path + ".before-quota-balancer")
	if !bytes.Equal(updated, repeated) || !bytes.Equal(original, backup) {
		t.Fatal("configuration update is not idempotent or backup changed")
	}
	info, errStat := os.Stat(path)
	if errStat != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v %v", info, errStat)
	}
}

func TestMergeRejectsInvalidStructures(t *testing.T) {
	for _, config := range []string{"", "[]", "plugins: true", "plugins: {configs: []}", "[bad"} {
		if _, errMerge := Merge([]byte(config), []byte("enabled: true")); errMerge == nil {
			t.Fatalf("invalid structure %q accepted", config)
		}
	}
	if _, errMerge := Merge([]byte("port: 8317"), []byte("[]")); errMerge == nil {
		t.Fatal("non-mapping policy accepted")
	}
}
