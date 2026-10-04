//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const earliestReset = "earliest-reset"

type pluginConfig struct {
	Strategy       string  `yaml:"strategy"`
	ReservePercent float64 `yaml:"reserve-percent"`
	ReleaseWithin  string  `yaml:"release-within"`
	releaseWithin  time.Duration
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Strategy: earliestReset, ReservePercent: 10,
		ReleaseWithin: "1h", releaseWithin: time.Hour,
	}
}

var currentConfig atomic.Value
var fallbackCursor atomic.Uint64

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	var raw []byte
	if method == nil {
		raw, _ = pluginabi.NewErrorEnvelope("invalid_method", "method is required")
	} else {
		var requestBytes []byte
		if request != nil && requestLen > 0 {
			requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
		}
		var errHandle error
		raw, errHandle = handleMethod(C.GoString(method), requestBytes)
		if errHandle != nil {
			raw, _ = pluginabi.NewErrorEnvelope("plugin_error", errHandle.Error())
		}
	}
	if len(raw) > 0 {
		response.ptr = C.CBytes(raw)
		response.len = C.size_t(len(raw))
	}
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(raw) > 0 {
			if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		cfg := defaultConfig()
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		cfg.Strategy = strings.ToLower(strings.TrimSpace(cfg.Strategy))
		if cfg.Strategy == "" {
			cfg.Strategy = earliestReset
		}
		switch cfg.Strategy {
		case earliestReset, pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst:
		default:
			return nil, fmt.Errorf("unsupported balancing strategy %q", cfg.Strategy)
		}
		if _, valid := quotaNumber(fmt.Sprint(cfg.ReservePercent), 100); !valid || cfg.ReservePercent == 100 {
			return nil, fmt.Errorf("reserve-percent must be finite and between 0 inclusive and 100 exclusive")
		}
		duration, errParse := time.ParseDuration(cfg.ReleaseWithin)
		if errParse != nil || duration < 0 {
			return nil, fmt.Errorf("release-within must be a nonnegative duration such as 1h or 30m")
		}
		cfg.releaseWithin = duration
		currentConfig.Store(cfg)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		cfg := defaultConfig()
		if value := currentConfig.Load(); value != nil {
			cfg = value.(pluginConfig)
		}
		if cfg.Strategy != earliestReset {
			return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: cfg.Strategy})
		}
		return okEnvelope(pickQuotaAuth(req.Candidates, time.Now(), &fallbackCursor, cfg))
	default:
		return pluginabi.NewErrorEnvelope("unknown_method", "unknown method: "+method)
	}
}

func pluginRegistration() any {
	return struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "quota-balancer",
			Version:          "0.1.1",
			Author:           "jcarcaboso",
			GitHubRepository: "https://github.com/jcarcaboso/cliproxy-quota-balancer",
			ConfigFields: []pluginapi.ConfigField{{
				Name:        "strategy",
				Type:        pluginapi.ConfigFieldTypeEnum,
				EnumValues:  []string{earliestReset, pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst},
				Description: "Prefer the account whose five-hour or weekly quota resets soonest; other strategies use built-in routing.",
			}, {
				Name: "reserve-percent", Type: pluginapi.ConfigFieldTypeNumber,
				Description: "Remaining quota percentage to preserve in each window until release-within. Default 10; zero disables the reserve.",
			}, {
				Name: "release-within", Type: pluginapi.ConfigFieldTypeString,
				Description: "Release a window's reserve when its reset is this close. Default 1h; zero keeps the reserve until reset.",
			}},
		},
		Capabilities: map[string]bool{"scheduler": true},
	}
}

func okEnvelope(value any) ([]byte, error) {
	result, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}
