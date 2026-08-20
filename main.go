package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void *ptr; size_t len; } cpa_buffer;
typedef struct { uint32_t abi_version; void *host_ctx; void *call; void *free_buffer; } cpa_host_api;
typedef int (*cpa_call_fn)(char*, uint8_t*, size_t, cpa_buffer*);
typedef void (*cpa_free_fn)(void*, size_t);
typedef void (*cpa_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cpa_call_fn call; cpa_free_fn free_buffer; cpa_shutdown_fn shutdown; } cpa_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cpa_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/httpapi"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

var runtimeState struct {
	sync.Mutex
	st  *store.Store
	svc *service.Service
	api *httpapi.API
	cfg config.Config
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cpa_host_api, p *C.cpa_plugin_api) C.int {
	if p == nil {
		return 1
	}
	p.abi_version = 1
	p.call = C.cpa_call_fn(C.cliproxyPluginCall)
	p.free_buffer = C.cpa_free_fn(C.cliproxyPluginFree)
	p.shutdown = C.cpa_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, n C.size_t, out *C.cpa_buffer) C.int {
	if out != nil {
		out.ptr = nil
		out.len = 0
	}
	m := C.GoString(method)
	var body []byte
	if request != nil && n > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(n))
	}
	resp, err := dispatch(m, body)
	if err != nil {
		resp, _ = json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	}
	if out != nil && len(resp) > 0 {
		p := C.CBytes(resp)
		out.ptr = p
		out.len = C.size_t(len(resp))
	}
	runtime.KeepAlive(method)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(p unsafe.Pointer, _ C.size_t) {
	if p != nil {
		C.free(p)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	runtimeState.Lock()
	defer runtimeState.Unlock()
	if runtimeState.st != nil {
		_ = runtimeState.st.Close()
		runtimeState.st = nil
	}
	runtimeState.svc = nil
	runtimeState.api = nil
}

func dispatch(method string, body []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML    string `json:"config_yaml"`
			SchemaVersion uint32 `json:"schema_version"`
		}
		_ = json.Unmarshal(body, &req)
		inline := req.ConfigYAML
		if inline == "" {
			inline = string(body)
		}
		if err := configure(inline); err != nil {
			return nil, err
		}
		return success(map[string]any{"schema_version": minSchema(req.SchemaVersion), "metadata": map[string]string{"id": config.PluginID, "name": "CPA Usage Manager", "version": version}, "capabilities": map[string]any{"usage_plugin": true, "management_api": true, "frontend_auth_provider": runtimeState.cfg.Quota.Enabled, "frontend_auth_provider_exclusive": runtimeState.cfg.Quota.Enabled, "model_router": runtimeState.cfg.Quota.Enabled, "executor": runtimeState.cfg.Quota.Enabled}})
	case "frontend_auth.identifier":
		return success(map[string]string{"identifier": config.PluginID})
	case "frontend_auth.authenticate":
		var req struct {
			Key           string `json:"key"`
			Authorization string `json:"authorization"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		key := req.Key
		if key == "" {
			key = strings.TrimPrefix(req.Authorization, "Bearer ")
		}
		runtimeState.Lock()
		svc := runtimeState.svc
		runtimeState.Unlock()
		if svc == nil {
			return nil, fmt.Errorf("插件尚未注册")
		}
		a, err := svc.Authenticate(context.Background(), key)
		if err != nil {
			return nil, err
		}
		return success(map[string]any{"ok": true, "kid": a.Record.KID, "caller_id": a.Record.CallerID})
	case "usage.handle":
		runtimeState.Lock()
		st := runtimeState.st
		runtimeState.Unlock()
		if st == nil {
			return nil, fmt.Errorf("插件尚未注册")
		}
		var req store.Request
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		if err := st.RecordUsage(context.Background(), req); err != nil {
			return nil, err
		}
		return success(map[string]bool{"accepted": true})
	case "management.register":
		return success(map[string]any{"routes": []string{"/v0/management/plugins/cpa-usage-manager/*"}, "resources": []string{"/console"}})
	case "management.handle":
		var req struct {
			Method, Path string
			Headers      map[string]string
			Body         json.RawMessage
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		runtimeState.Lock()
		api := runtimeState.api
		runtimeState.Unlock()
		if api == nil {
			return nil, fmt.Errorf("插件尚未注册")
		}
		hr := httptest.NewRequest(req.Method, req.Path, strings.NewReader(string(req.Body)))
		for k, v := range req.Headers {
			hr.Header.Set(k, v)
		}
		rw := httptest.NewRecorder()
		api.Handler().ServeHTTP(rw, hr)
		return success(map[string]any{"status": rw.Code, "headers": rw.Header(), "body": json.RawMessage(rw.Body.Bytes())})
	case "plugin.shutdown":
		cliproxyPluginShutdown()
		return success(map[string]any{})
	default:
		return nil, fmt.Errorf("unsupported plugin method %q", method)
	}
}

func success(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(b)})
}
func minSchema(host uint32) uint32 {
	if host == 0 || host > 3 {
		return 1
	}
	return host
}

func configure(inline string) error {
	runtimeState.Lock()
	defer runtimeState.Unlock()
	cfg, err := config.Load(inline, os.LookupEnv)
	if err != nil {
		return err
	}
	if err = cfg.EnsureDataDir(); err != nil {
		return err
	}
	if runtimeState.st != nil {
		_ = runtimeState.st.Close()
	}
	st, err := store.Open(context.Background(), store.Options{Path: cfg.DatabasePath(), BusyTimeout: cfg.BusyTimeout.Std()})
	if err != nil {
		return err
	}
	ps, err := service.LoadPeppers(cfg, os.LookupEnv)
	if err != nil {
		_ = st.Close()
		return err
	}
	svc := service.New(st, cfg, ps)
	runtimeState.st = st
	runtimeState.svc = svc
	runtimeState.api = httpapi.New(svc, st, os.Getenv("CPA_USAGE_MANAGER_MANAGEMENT_KEY"))
	runtimeState.cfg = cfg
	return nil
}

var version = "dev"

func main() { _ = runtime.GOOS }
