package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void *ptr; size_t len; } cpa_buffer;
typedef int (*cpa_host_call_fn)(void*, const char*, const uint8_t*, size_t, cpa_buffer*);
typedef void (*cpa_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void *host_ctx; cpa_host_call_fn call; cpa_host_free_fn free_buffer; } cpa_host_api;
typedef int (*cpa_call_fn)(char*, uint8_t*, size_t, cpa_buffer*);
typedef void (*cpa_free_fn)(void*, size_t);
typedef void (*cpa_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cpa_call_fn call; cpa_free_fn free_buffer; cpa_shutdown_fn shutdown; } cpa_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cpa_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cpa_host_api* stored_host_api;
static void store_host_api(const cpa_host_api* host) { stored_host_api = host; }
static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cpa_buffer* response) {
	if (stored_host_api == NULL || stored_host_api->call == NULL) return 1;
	return stored_host_api->call(stored_host_api->host_ctx, method, request, request_len, response);
}
static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host_api != NULL && stored_host_api->free_buffer != NULL && ptr != NULL) {
		stored_host_api->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/httpapi"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

var runtimeState struct {
	sync.Mutex
	st  *store.Store
	svc *service.Service
	api *httpapi.API
	cfg config.Config
	// notifyStop 关停告警扫描 goroutine；reconfigure/shutdown 时关闭。
	notifyStop chan struct{}
	// backupStop 关停定时备份 goroutine，同上。
	backupStop chan struct{}
	// fxStop 关停汇率刷新 goroutine，同上。
	fxStop chan struct{}
}

type imageHold struct {
	reservation store.Reservation
	plan        service.ReservePlan
	stopHeart   func()
	claim       *usageClaim
	created     time.Time
}

var (
	imageHoldsMu sync.Mutex
	imageHolds   = map[string]imageHold{}
	// imageHoldMaxAge 是拦截器持有条目的存活上限。request.complete 丢失时
	// （如宿主/插件中途重启），条目会连同其心跳 goroutine 一起泄漏，
	// 登记新条目时按该阈值回收。
	imageHoldMaxAge = time.Hour
)

// ---------- RPC 信封 ----------

type rpcEnvelope struct {
	OK     bool              `json:"ok"`
	Result json.RawMessage   `json:"result,omitempty"`
	Error  *rpcEnvelopeError `json:"error,omitempty"`
}
type rpcEnvelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---------- 注册/元数据 ----------

type rpcLifecycleRequest struct {
	ConfigYAML    json.RawMessage `json:"config_yaml"`
	SchemaVersion uint32          `json:"schema_version"`
}

// rpcMetadata 等结构体无 JSON tag，序列化为 PascalCase，与宿主 pluginapi.Metadata 契约一致。
type rpcMetadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []rpcConfigField
}
type rpcConfigField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}
type rpcCapabilities struct {
	FrontendAuthProvider          bool     `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool     `json:"frontend_auth_provider_exclusive"`
	ModelRouter                   bool     `json:"model_router"`
	Executor                      bool     `json:"executor"`
	ExecutorModelScope            string   `json:"executor_model_scope"`
	ExecutorInputFormats          []string `json:"executor_input_formats"`
	ExecutorOutputFormats         []string `json:"executor_output_formats"`
	UsagePlugin                   bool     `json:"usage_plugin"`
	ManagementAPI                 bool     `json:"management_api"`
	RequestInterceptor            bool     `json:"request_interceptor"`
	RequestLifecyclePlugin        bool     `json:"request_lifecycle_plugin"`
	ModelRegistrar                bool     `json:"model_registrar"`
}
type rpcRegistration struct {
	SchemaVersion uint32          `json:"schema_version"`
	Metadata      rpcMetadata     `json:"metadata"`
	Capabilities  rpcCapabilities `json:"capabilities"`
}

// ---------- 管理接口 ----------

type rpcManagementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}
type rpcResourceRoute struct {
	Path        string
	Menu        string
	Description string
}
type rpcManagementRegistration struct {
	Routes    []rpcManagementRoute `json:"routes,omitempty"`
	Resources []rpcResourceRoute   `json:"resources,omitempty"`
}
type rpcManagementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// ---------- 前端鉴权 ----------

type rpcFrontendAuthRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}
type rpcFrontendAuthResponse struct {
	Authenticated bool
	Principal     string
	Metadata      map[string]string
}

// ---------- 模型路由 / 执行器 ----------

type rpcModelRouteRequest struct {
	SourceFormat   string
	RequestedModel string
	Headers        http.Header
	Metadata       map[string]any
}
type rpcModelRouteResponse struct {
	Handled    bool
	TargetKind string
	Reason     string
}

type rpcExecutorRequest struct {
	AuthID          string
	AuthProvider    string
	AuthType        string
	Model           string
	Format          string
	Stream          bool
	Alt             string
	Headers         http.Header
	Query           url.Values
	OriginalRequest []byte
	SourceFormat    string
	Payload         []byte
	Metadata        map[string]any
	StreamID        string `json:"stream_id,omitempty"`
	HostCallbackID  string `json:"host_callback_id,omitempty"`
}
type rpcExecutorResponse struct {
	Payload  []byte
	Headers  http.Header
	Metadata map[string]any
}

// ---------- 宿主回调 ----------

type rpcHostModelExecutionRequest struct {
	EntryProtocol  string      `json:"entry_protocol"`
	ExitProtocol   string      `json:"exit_protocol"`
	Model          string      `json:"model"`
	Stream         bool        `json:"stream"`
	Body           []byte      `json:"body"`
	Headers        http.Header `json:"headers"`
	Query          url.Values  `json:"query"`
	Alt            string      `json:"alt"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}
type rpcHostModelExecutionResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
}
type rpcHostModelStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}
type rpcHostModelStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}
type rpcHostModelStreamReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}
type rpcHostModelStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}
type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}
type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// ---------- 请求拦截 / 生命周期 ----------

type rpcRequestInterceptRequest struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Headers        http.Header
	Body           []byte
	Metadata       map[string]any
}
type rpcRequestInterceptResponse struct {
	Headers         http.Header
	Body            []byte
	ClearHeaders    []string
	Terminate       bool
	StatusCode      int
	ResponseHeaders http.Header
	ResponseBody    []byte
}
type rpcRequestCompletion struct {
	RequestID      string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        string
	StatusCode     int
	StartedAt      time.Time
	CompletedAt    time.Time
	Metadata       map[string]any
}

// ---------- usage ----------

type rpcUsageRecord struct {
	Provider        string
	Model           string
	Alias           string
	APIKey          string
	AuthID          string
	AuthIndex       string
	AuthType        string
	Source          string
	ReasoningEffort string
	ServiceTier     string
	RequestedAt     time.Time
	Latency         time.Duration
	TTFT            time.Duration
	Failed          bool
	Failure         rpcUsageFailure
	Detail          rpcUsageDetail
}
type rpcUsageFailure struct {
	StatusCode int
	Body       string
}
type rpcUsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// ---------- C ABI ----------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cpa_host_api, p *C.cpa_plugin_api) C.int {
	if p == nil || host == nil || host.abi_version != 1 {
		return 1
	}
	C.store_host_api(host)
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
	if method == nil {
		writeResponse(out, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var body []byte
	if request != nil && n > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(n))
	}
	raw, err := safeDispatch(C.GoString(method), body)
	if err != nil {
		writeResponse(out, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(out, raw)
	return 0
}

// safeDispatch 包住 dispatch：c-shared 库里 Go panic 跨 C 边界是 abort，
// 会直接击穿宿主进程，任何插件内 panic 都必须折叠成错误信封返回。
func safeDispatch(method string, body []byte) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			raw = nil
			err = fmt.Errorf("plugin panic: %v", r)
		}
	}()
	return dispatch(method, body)
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
	st := runtimeState.st
	notifyStop := runtimeState.notifyStop
	backupStop := runtimeState.backupStop
	fxStop := runtimeState.fxStop
	runtimeState.st = nil
	runtimeState.svc = nil
	runtimeState.api = nil
	runtimeState.notifyStop = nil
	runtimeState.backupStop = nil
	runtimeState.fxStop = nil
	runtimeState.Unlock()
	if notifyStop != nil {
		close(notifyStop)
	}
	if backupStop != nil {
		close(backupStop)
	}
	if fxStop != nil {
		close(fxStop)
	}
	if st != nil {
		_ = st.Close()
	}
	imageHoldsMu.Lock()
	for _, h := range imageHolds {
		if h.stopHeart != nil {
			h.stopHeart()
		}
	}
	imageHolds = map[string]imageHold{}
	imageHoldsMu.Unlock()
	resetUsageClaims()
}

func writeResponse(out *C.cpa_buffer, raw []byte) {
	if out == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	out.ptr = ptr
	out.len = C.size_t(len(raw))
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rpcEnvelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(rpcEnvelope{OK: false, Error: &rpcEnvelopeError{Code: code, Message: message}})
	return raw
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cpa_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var env rpcEnvelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// hostCall 是宿主回调的可替换入口：生产恒为 callHost；路由 failover 的
// 集成测试经它注入桩传输（见 main_routes_test.go）。
var hostCall = callHost

func current() (st *store.Store, svc *service.Service, api *httpapi.API, cfg config.Config) {
	runtimeState.Lock()
	defer runtimeState.Unlock()
	return runtimeState.st, runtimeState.svc, runtimeState.api, runtimeState.cfg
}

// ---------- 方法调度 ----------

func dispatch(method string, body []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		inline, schema, err := decodeLifecycle(body)
		if err != nil {
			return nil, err
		}
		if err := configure(inline); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration(minSchema(schema)))
	case "frontend_auth.identifier":
		return okEnvelope(map[string]string{"identifier": config.PluginID})
	case "frontend_auth.authenticate":
		return authenticate(body)
	case "model.route":
		return routeModel(body)
	case "executor.identifier":
		return okEnvelope(map[string]string{"identifier": config.PluginID})
	case "executor.execute":
		return execute(body)
	case "executor.execute_stream":
		return executeStream(body)
	case "executor.count_tokens":
		return okEnvelope(rpcExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	case "management.register":
		return okEnvelope(managementRegistration())
	case "management.handle":
		return handleManagement(body)
	case "model.register":
		_, svc, _, _ := current()
		return modelRegister(svc)
	case "usage.handle":
		return handleUsage(body)
	case "request.intercept_before":
		return okEnvelope(rpcRequestInterceptResponse{})
	case "request.intercept_after":
		return interceptAfter(body)
	case "request.complete":
		return completeIntercepted(body)
	case "plugin.shutdown":
		cliproxyPluginShutdown()
		return okEnvelope(map[string]any{})
	default:
		return errorEnvelope("unknown_method", "unsupported plugin method "+method), nil
	}
}

func minSchema(host uint32) uint32 {
	if host == 0 || host > 3 {
		return 1
	}
	return host
}

// decodeLifecycle 解析注册请求。config_yaml 支持 base64 字符串、纯 YAML 字符串与字节数组；
// 非 JSON 的请求体按纯 YAML 处理（便于本地冒烟/测试）。
func decodeLifecycle(raw []byte) (string, uint32, error) {
	if len(raw) == 0 {
		return "", 0, nil
	}
	var req rpcLifecycleRequest
	if err := json.Unmarshal(raw, &req); err == nil {
		inline, err := decodeConfigYAML(req.ConfigYAML)
		return inline, req.SchemaVersion, err
	}
	return string(raw), 0, nil
}

func decodeConfigYAML(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text)); err == nil && strings.Contains(string(decoded), ":") {
			return string(decoded), nil
		}
		return text, nil
	}
	var b []byte
	if err := json.Unmarshal(raw, &b); err == nil {
		return string(b), nil
	}
	return "", fmt.Errorf("config_yaml 必须是 base64/纯文本字符串或字节数组")
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
	// 旧 Service 的后台心跳协程必须显式停止：reservationBeatLoop 原本无退出
	// 通道，reconfigure 换新 Service 后旧循环会永久泄漏并反复触碰已关闭的库。
	if old := runtimeState.svc; old != nil {
		old.Close()
	}
	// 旧通知循环引用的 svc 包着已关闭的库，无论后面成败都要在这里停下。
	if runtimeState.notifyStop != nil {
		close(runtimeState.notifyStop)
		runtimeState.notifyStop = nil
	}
	if runtimeState.backupStop != nil {
		close(runtimeState.backupStop)
		runtimeState.backupStop = nil
	}
	if runtimeState.fxStop != nil {
		close(runtimeState.fxStop)
		runtimeState.fxStop = nil
	}
	// 旧库已关、新库未开：失败路径必须把运行态整体置空，让后续请求拿到
	// 干净的 service_unavailable，而不是对已关闭的库报错——后者会让
	// usage.handle 的被动记录在 warn 日志之外静默丢失。
	st, err := store.Open(context.Background(), store.Options{Path: cfg.DatabasePath(), BusyTimeout: cfg.BusyTimeout.Std()})
	if err != nil {
		runtimeState.st, runtimeState.svc, runtimeState.api, runtimeState.cfg = nil, nil, nil, config.Config{}
		return err
	}
	ps, err := service.LoadPeppers(cfg, os.LookupEnv)
	if err != nil {
		_ = st.Close()
		runtimeState.st, runtimeState.svc, runtimeState.api, runtimeState.cfg = nil, nil, nil, config.Config{}
		return err
	}
	svc := service.New(st, cfg, ps)
	// 周期额度相对 UTC 的固定偏移（quota.cycle_offset_minutes）：
	// store 包级 atomic，进程内所有 CyclesFor/CycleStart 共用。
	store.SetCycleOffsetMinutes(int64(cfg.Quota.CycleOffsetMinutes))
	// ai_judge 的宿主执行钩子：服务层不直接持有 C ABI 回调，经此注入。
	// 评判调用以 openai 协议、无头信息直连 host.model.execute，非流式。
	svc.SetJudgeExecutor(func(_ context.Context, model string, body []byte) ([]byte, int, error) {
		judgeReq := rpcExecutorRequest{SourceFormat: "openai", Format: "openai"}
		respBody, _, status, err := hostModelExecute("", judgeReq, model, body, false)
		return respBody, status, err
	})
	// 租约被接管（多进程部署中出现第二个写者）时本实例降级只读，
	// 经通知端点上报；回调在心跳协程里触发，须立即返回，故异步发送。
	st.SetLeaseLostHandler(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			svc.NotifyErrorEvent(ctx, "storage", "数据库租约被其他进程接管，本实例已降级为只读模式。")
		}()
	})
	notifyStop := make(chan struct{})
	runtimeState.notifyStop = notifyStop
	go notifySweepLoop(svc, notifyStop)
	fxStop := make(chan struct{})
	runtimeState.fxStop = fxStop
	go fxRefreshLoop(svc, fxStop)
	if cfg.Backup.Enabled {
		backupStop := make(chan struct{})
		runtimeState.backupStop = backupStop
		go autoBackupLoop(svc, cfg.Backup, backupStop)
	}
	runtimeState.st = st
	runtimeState.svc = svc
	runtimeState.api = httpapi.New(svc, st, httpapi.Options{
		ManagementKey:       os.Getenv("CPA_USAGE_MANAGER_MANAGEMENT_KEY"),
		CompressionEnabled:  cfg.ResponseCompression,
		CompressionMinBytes: cfg.ResponseCompressionMinBytes,
	})
	runtimeState.cfg = cfg
	return nil
}

// ---------- 注册响应 ----------

func pluginRegistration(schema uint32) rpcRegistration {
	_, _, _, cfg := current()
	enabled := cfg.Quota.Enabled
	formats := []string{
		"openai", "chat-completions", "claude", "gemini",
		"openai-response", "responses", "codex", "openai-image", "openai-video",
	}
	caps := rpcCapabilities{UsagePlugin: true, ManagementAPI: true}
	if enabled {
		caps.FrontendAuthProvider = true
		caps.FrontendAuthProviderExclusive = true
		caps.ModelRouter = true
		caps.Executor = true
		caps.ExecutorModelScope = "both"
		caps.ExecutorInputFormats = formats
		caps.ExecutorOutputFormats = formats
		caps.RequestInterceptor = true
		caps.RequestLifecyclePlugin = true
		// 恒声明 model_registrar（空模型列表宿主自动跳过）：若按「存在启用路由」
		// 条件声明，宿主启动时还没有路由的话能力位=false，之后必须等一次
		// reconfigure 才会被当作 registrar——这是「别名没进 /v1/models」的
		// 实际踩坑路径。注册本身仍只返回启用中的别名。
		caps.ModelRegistrar = true
	}
	return rpcRegistration{
		SchemaVersion: schema,
		Metadata: rpcMetadata{
			Name:             "CPA Usage Manager",
			Version:          version,
			Author:           "drowsylazy",
			GitHubRepository: "https://github.com/drowsylazy/cpa-usage-manager",
			Logo:             "https://raw.githubusercontent.com/drowsylazy/cpa-usage-manager/main/logo.svg",
			ConfigFields: []rpcConfigField{
				{Name: "config_file", Type: "string", Description: "外部 YAML 配置路径（或环境变量 CPA_USAGE_MANAGER_CONFIG_FILE），仅宿主可设置。"},
				{Name: "data_dir", Type: "string", Description: "数据目录（0700），存放 SQLite 与 key-peppers。"},
				{Name: "database_file", Type: "string", Description: "data_dir 下的 SQLite 文件名（默认 cpa-usage-manager.db）。"},
				{Name: "busy_timeout", Type: "string", Description: "SQLite 忙等超时，如 5s。"},
				{Name: "retention_days", Type: "integer", Description: "逐请求明细与分钟聚合保留天数（1..3650）。"},
				{Name: "audit_retention_days", Type: "integer", Description: "内部审计留痕保留天数（0=跟随 retention_days，默认 90）。"},
				{Name: "backup.max_bytes", Type: "integer", Description: "备份导出/恢复的单文件上限字节（默认 256MiB）。"},
				{Name: "quota.enabled", Type: "boolean", Description: "false 退回纯统计（被动 usage 记录），不接管前端鉴权。"},
				{Name: "quota.keys.pepper_env", Type: "string", Description: "pepper 环境变量名（默认 CPA_USAGE_MANAGER_KEY_PEPPERS）。"},
				{Name: "quota.keys.pepper_file", Type: "string", Description: "data_dir 下的 pepper 文件（默认 key-peppers，0600）。"},
				{Name: "quota.keys.active_pepper_id", Type: "string", Description: "签发新 Key 时使用的 pepper 代际。"},
				{Name: "quota.limits.max_token_estimate", Type: "integer", Description: "单请求预占 Token 严格上限。"},
				{Name: "quota.limits.default_output_reserve", Type: "integer", Description: "请求体未给 max_tokens 时的输出预占。"},
				{Name: "quota.limits.require_estimate", Type: "boolean", Description: "true 时缺少用量估算的请求拒绝预占。"},
				{Name: "quota.settlement.missing_usage", Type: "enum", EnumValues: []string{"settle_reserved", "release"}, Description: "上游未返回 usage 时的结算策略。"},
				{Name: "quota.settlement.host_usage_wait", Type: "string", Description: "流式结算在上游未给 usage 时，关闭客户端流后等待宿主 usage.handle 的时长（0 关闭；非流式不等待）。"},
				{Name: "quota.stream.stale_reservation_timeout", Type: "string", Description: "无心跳在途预占自动释放时长。"},
				{Name: "quota.cycle_offset_minutes", Type: "integer", Description: "日/周/月额度周期相对 UTC 的固定偏移分钟数（480=UTC+8，日限额在本地零点归零；默认 0 保持 UTC）。"},
				{Name: "backup.enabled", Type: "boolean", Description: "每日自动备份库快照到本地目录（默认关闭）。"},
				{Name: "backup.dir", Type: "string", Description: "备份目录，相对 data_dir（默认 backups，0700）。"},
				{Name: "backup.keep", Type: "integer", Description: "保留份数，超出删最旧（默认 7）。"},
				{Name: "backup.hour", Type: "integer", Description: "每日触发的本地小时 0..23（默认 4；启动时当天已过点会补备份）。"},
				{Name: "pricing.unknown_policy", Type: "enum", EnumValues: []string{"deny", "allow", "default"}, Description: "无计价规则命中时的策略。"},
				{Name: "pricing.models_dev_sync.enabled", Type: "boolean", Description: "是否启用 models.dev 价格同步。"},
				{Name: "response_compression", Type: "boolean", Description: "管理面板 JSON/HTML 是否 gzip。"},
				{Name: "response_compression_min_bytes", Type: "integer", Description: "gzip 最小字节阈值。"},
			},
		},
		Capabilities: caps,
	}
}

func managementRegistration() rpcManagementRegistration {
	base := "/v0/management/plugins/cpa-usage-manager"
	routes := []rpcManagementRoute{
		{Method: "GET", Path: base + "/health", Description: "健康与存储统计"},
		{Method: "GET", Path: base + "/overview", Description: "总览"},
		{Method: "GET", Path: base + "/callers", Description: "归属列表"},
		{Method: "POST", Path: base + "/callers", Description: "新建/更新归属"},
		{Method: "POST", Path: base + "/callers/enabled", Description: "启用/禁用归属"},
		{Method: "GET", Path: base + "/keys", Description: "Key 列表"},
		{Method: "GET", Path: base + "/keys/candidates", Description: "Key 联想候选（kid+标签，轻量）"},
		{Method: "GET", Path: base + "/reservations/held", Description: "在途预占（进行中请求）视图"},
		{Method: "GET", Path: base + "/reservations/recent", Description: "最近已完结预占回顾（估算 vs 实结）"},
		{Method: "GET", Path: base + "/model-routes/health", Description: "路由目标健康（冷却+近期失败）"},
		{Method: "POST", Path: base + "/keys/issue", Description: "签发 Key"},
		{Method: "POST", Path: base + "/keys/update", Description: "更新 Key 策略"},
		{Method: "POST", Path: base + "/keys/rotate", Description: "轮换 Key"},
		{Method: "POST", Path: base + "/keys/reveal", Description: "解密回显 Key"},
		{Method: "POST", Path: base + "/keys/revoke", Description: "撤销 Key"},
		{Method: "POST", Path: base + "/keys/delete", Description: "删除 Key"},
		{Method: "GET", Path: base + "/pricing", Description: "计价规则列表"},
		{Method: "POST", Path: base + "/pricing", Description: "新建/更新计价规则"},
		{Method: "POST", Path: base + "/pricing/delete", Description: "删除计价规则"},
		{Method: "GET", Path: base + "/pricing/search", Description: "models.dev 计价搜索"},
		{Method: "POST", Path: base + "/pricing/reset", Description: "清空计价规则（保留免费兜底）"},
		{Method: "POST", Path: base + "/pricing/sync", Description: "models.dev 同步"},
		{Method: "GET", Path: base + "/model-routes", Description: "模型路由（集合别名）列表与评判设置"},
		{Method: "POST", Path: base + "/model-routes/save", Description: "新增/更新模型路由"},
		{Method: "POST", Path: base + "/model-routes/delete", Description: "删除模型路由"},
		{Method: "GET", Path: base + "/model-routes/judge", Description: "AI 评判设置读取"},
		{Method: "POST", Path: base + "/model-routes/judge", Description: "AI 评判设置保存"},
		{Method: "POST", Path: base + "/model-routes/test", Description: "路由规则干跑测试（不请求目标模型）"},
		{Method: "GET", Path: base + "/usage", Description: "请求明细"},
		{Method: "GET", Path: base + "/usage/summary", Description: "用量汇总"},
		{Method: "GET", Path: base + "/usage/dimension", Description: "维度分组"},
		{Method: "GET", Path: base + "/requests", Description: "请求明细（别名）"},
		{Method: "GET", Path: base + "/trends", Description: "趋势"},
		{Method: "GET", Path: base + "/costs", Description: "费用覆盖"},
		{Method: "GET", Path: base + "/balance", Description: "Key 余额"},
		{Method: "GET", Path: base + "/routes", Description: "上游路由分布"},
		{Method: "GET", Path: base + "/audit", Description: "审计事件"},
		{Method: "GET", Path: base + "/preferences", Description: "面板偏好读取"},
		{Method: "POST", Path: base + "/preferences", Description: "面板偏好保存"},
		{Method: "GET", Path: base + "/exchange-rate", Description: "汇率读取"},
		{Method: "POST", Path: base + "/exchange-rate", Description: "刷新汇率"},
		{Method: "GET", Path: base + "/notify", Description: "通知设置与端点列表"},
		{Method: "POST", Path: base + "/notify/settings", Description: "保存通知设置"},
		{Method: "POST", Path: base + "/notify/endpoint/save", Description: "新增/更新通知端点"},
		{Method: "POST", Path: base + "/notify/endpoint/delete", Description: "删除通知端点"},
		{Method: "POST", Path: base + "/notify/endpoint/test", Description: "发送通知测试消息"},
		{Method: "GET", Path: base + "/reports", Description: "定期报告配置列表"},
		{Method: "POST", Path: base + "/reports/save", Description: "新增/更新定期报告"},
		{Method: "POST", Path: base + "/reports/delete", Description: "删除定期报告"},
		{Method: "POST", Path: base + "/reports/test", Description: "立即发送测试报告"},
		{Method: "POST", Path: base + "/export/csv", Description: "CSV 导出"},
		{Method: "POST", Path: base + "/export/png", Description: "PNG 导出"},
		{Method: "GET", Path: base + "/backup", Description: "数据库备份"},
		{Method: "POST", Path: base + "/restore", Description: "数据库恢复"},
		{Method: "POST", Path: base + "/reset", Description: "重置统计"},
		{Method: "POST", Path: base + "/maintain", Description: "保留清理/VACUUM"},
		{Method: "POST", Path: base + "/dedupe", Description: "重复请求对账去重"},
	}
	return rpcManagementRegistration{
		Routes: routes,
		Resources: []rpcResourceRoute{
			{Path: "/console", Menu: "用量管理", Description: "管理面板：用量/额度/审计"},
		},
	}
}

// ---------- 前端鉴权 ----------

func authenticate(body []byte) ([]byte, error) {
	_, svc, _, _ := current()
	if svc == nil {
		return okEnvelope(rpcFrontendAuthResponse{Authenticated: false})
	}
	var req rpcFrontendAuthRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return okEnvelope(rpcFrontendAuthResponse{Authenticated: false})
	}
	if publicModelDirectory(req.Method, req.Path) {
		return okEnvelope(rpcFrontendAuthResponse{
			Authenticated: true,
			Principal:     "cpa-usage-manager:model-directory",
			Metadata:      map[string]string{"plugin": config.PluginID, "public_models": "true"},
		})
	}
	plain := bearerToken(req.Headers)
	if plain == "" {
		return okEnvelope(rpcFrontendAuthResponse{Authenticated: false})
	}
	a, err := svc.Authenticate(context.Background(), plain)
	if err != nil {
		return okEnvelope(rpcFrontendAuthResponse{Authenticated: false})
	}
	k := a.Record
	principal := strings.TrimSpace(k.Principal)
	if principal == "" {
		principal = k.KID
	}
	return okEnvelope(rpcFrontendAuthResponse{
		Authenticated: true,
		Principal:     principal,
		Metadata:      map[string]string{"plugin": config.PluginID, "kid": k.KID, "caller_id": k.CallerID, "fingerprint": k.Fingerprint},
	})
}

func publicModelDirectory(method, path string) bool {
	if !strings.EqualFold(strings.TrimSpace(method), http.MethodGet) {
		return false
	}
	switch strings.TrimRight(strings.TrimSpace(path), "/") {
	case "/v1/models", "/v1beta/models":
		return true
	default:
		return false
	}
}

func bearerToken(headers http.Header) string {
	if headers == nil {
		return ""
	}
	auth := strings.TrimSpace(headers.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[len("bearer "):])
}

// ---------- 模型路由 ----------

func routeModel(body []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if isImageProtocol(req.SourceFormat) || isImageOnlyModel(req.RequestedModel) {
		return okEnvelope(rpcModelRouteResponse{Handled: false, Reason: "native_image_protocol"})
	}
	scope := metadataString(req.Metadata, service.CallerScopeMetadataKey)
	if scope != "" {
		return okEnvelope(rpcModelRouteResponse{Handled: true, TargetKind: "self", Reason: "cpa_quota_enforced"})
	}
	// 仅接管插件密钥（cum-）；宿主原生 API Key 不接管，与原生转发链路共存。
	if _, ok := service.ParseKeyID(bearerToken(req.Headers)); ok {
		return okEnvelope(rpcModelRouteResponse{Handled: true, TargetKind: "self", Reason: "cpa_quota_enforced"})
	}
	return okEnvelope(rpcModelRouteResponse{Handled: false})
}

func isImageProtocol(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai-image", "openai-video":
		return true
	default:
		return false
	}
}

func isImageOnlyModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	switch model {
	case "gpt-image-1", "gpt-image-1.5", "gpt-image-2", "grok-imagine-image", "grok-imagine-image-quality":
		return true
	default:
		return strings.Contains(model, "imagine-image") || strings.Contains(model, "imagine-video")
	}
}

func bearerPresent(headers http.Header) bool {
	if headers == nil {
		return false
	}
	auth := headers.Get("Authorization")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth)), "bearer ")
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata[key]; ok && v != nil {
		if text, ok := v.(string); ok {
			return strings.TrimSpace(text)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

// ---------- 执行器 ----------

func execute(body []byte) ([]byte, error) {
	_, svc, _, _ := current()
	if svc == nil {
		return errorEnvelope("service_unavailable", "插件尚未注册"), nil
	}
	var req rpcExecutorRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	ctx := context.Background()
	key, err := svc.ResolveIdentity(ctx, req.Headers, req.Metadata)
	if err != nil {
		return errorEnvelope("unauthorized", err.Error()), nil
	}
	request := requestBody(req)
	startedAt := time.Now()

	// 集合别名流量：failover 循环编排（见 main_routes.go）。
	if re, failure := resolveRouting(ctx, svc, &key, req, request, false); re != nil || failure != nil {
		if failure != nil {
			return errorEnvelope(failure.code, failure.message), nil
		}
		stopHeartbeat := startReservationHeartbeat(svc, re.reservation.ID)
		defer stopHeartbeat()
		return executeRoutedLoop(ctx, re, req, request, startedAt)
	}

	plan, err := svc.BuildReservePlan(ctx, req.Model, request)
	if err != nil {
		if errors.Is(err, service.ErrModelDisabled) {
			return errorEnvelope("model_disabled", err.Error()), nil
		}
		return errorEnvelope("reserve_rejected", err.Error()), nil
	}
	reservation, err := svc.Reserve(ctx, service.ReservationRequest{KeyID: key.KID, CallerID: key.CallerID, Model: plan.Model, EstimatedTokens: plan.TokenEstimate, EstimatedInput: plan.InputEstimate, EstimatedOutput: plan.OutputEstimate, EstimatedImages: plan.ImageCount, Actor: "quota"})
	if err != nil {
		if errors.Is(err, service.ErrModelNotAllowed) {
			return errorEnvelope("model_not_allowed", err.Error()), nil
		}
		return errorEnvelope("limit_rejected", err.Error()), nil
	}
	stopHeartbeat := startReservationHeartbeat(svc, reservation.ID)
	defer stopHeartbeat()

	// 登记认领：宿主随后的 usage.handle 由本次请求消费，不再被动入库。
	claim := registerUsageClaim(key.KID, plan.Model, req.Model)
	hostBody, headers, status, errHost := hostModelExecute(req.HostCallbackID, req, strings.TrimSpace(req.Model), request, false)
	if errHost != nil {
		// 未落库任何请求行：放弃认领，宿主若上报失败用量仍走被动统计。
		claim.release(0)
		_, _ = svc.Release(ctx, reservation.ID)
		return errorEnvelope("upstream_error", errHost.Error()), nil
	}
	completedAt := time.Now()
	parsed, _ := usageparse.Parse(hostBody)
	// 非流式响应体几乎总带 usage，这里只做非阻塞探测：宿主记账发生在
	// 本次调用返回之后，同步等待只会白等一个超时。缺失的明细由认领在
	// 宽限期内按请求 ID 回填。
	// HTTP 错误或空响应体 = 上游未产生响应数据，零用量时按零成本结算。
	settleReservation(svc, plan, reservation, req, startedAt, time.Time{}, completedAt, status, parsed, usageparse.SniffModel(hostBody), hostErrorNote(status, hostBody), claim, status >= http.StatusBadRequest || len(hostBody) == 0)
	return okEnvelope(rpcExecutorResponse{Payload: hostBody, Headers: headers})
}

func executeStream(body []byte) ([]byte, error) {
	_, svc, _, _ := current()
	if svc == nil {
		return errorEnvelope("service_unavailable", "插件尚未注册"), nil
	}
	var req rpcExecutorRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	pluginStreamID := strings.TrimSpace(req.StreamID)
	if pluginStreamID == "" {
		return errorEnvelope("executor_error", "缺少 stream_id"), nil
	}
	go func() {
		var once sync.Once
		closeStream := func(errMsg string) {
			once.Do(func() { closePluginStream(pluginStreamID, errMsg) })
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				closeStream(fmt.Sprintf("panic: %v", recovered))
			}
		}()
		if err := runStream(req, pluginStreamID, closeStream); err != nil {
			closeStream(err.Error())
			return
		}
		closeStream("")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func runStream(req rpcExecutorRequest, pluginStreamID string, closeStream func(string)) error {
	_, svc, _, _ := current()
	if svc == nil {
		return errors.New("插件尚未注册")
	}
	ctx := context.Background()
	key, err := svc.ResolveIdentity(ctx, req.Headers, req.Metadata)
	if err != nil {
		return err
	}
	request := requestBody(req)
	startedAt := time.Now()

	// 集合别名流量：逐候选拨号，流建立后进入读泵（main_routes.go）。
	if re, failure := resolveRouting(ctx, svc, &key, req, request, true); re != nil || failure != nil {
		if failure != nil {
			return errors.New(failure.message)
		}
		stopHeartbeat := startReservationHeartbeat(svc, re.reservation.ID)
		defer stopHeartbeat()
		rw := newBodyRewriter(request, req.SourceFormat, req.Format, true)
		for i := range re.chain {
			stream, outcome, dialErr := dialHostStream(re, req, rw, i)
			switch outcome {
			case dialTransfer:
				continue
			case dialFailed:
				if statusErr, ok := dialErr.(errHostStatus); ok {
					// HTTP 错误：落行结算（与直连语义一致），错误文本关流。
					settleReservation(svc, re.plan, re.reservation, req, startedAt, time.Time{}, time.Now(), int(statusErr), usageparse.Usage{}, targetWithSuffix(re.chain[i], re.match.Suffix), re.failoverNote(), re.claim, true)
					return dialErr
				}
				re.claim.release(0)
				_, _ = svc.Release(ctx, re.reservation.ID)
				return dialErr
			default:
				finalTgt := targetWithSuffix(re.chain[i], re.match.Suffix)
				return pumpRoutedStream(re, req, startedAt, stream, finalTgt, pluginStreamID, closeStream)
			}
		}
		return errors.New("路由候选链为空")
	}

	plan, err := svc.BuildReservePlan(ctx, req.Model, request)
	if err != nil {
		return err
	}
	reservation, err := svc.Reserve(ctx, service.ReservationRequest{KeyID: key.KID, CallerID: key.CallerID, Model: plan.Model, EstimatedTokens: plan.TokenEstimate, EstimatedInput: plan.InputEstimate, EstimatedOutput: plan.OutputEstimate, EstimatedImages: plan.ImageCount, Actor: "quota"})
	if err != nil {
		return err
	}
	stopHeartbeat := startReservationHeartbeat(svc, reservation.ID)
	defer stopHeartbeat()

	// 登记认领：宿主随后的 usage.handle 由本次请求消费，不再被动入库。
	claim := registerUsageClaim(key.KID, plan.Model, req.Model)
	request = requestBodyWithStreamUsage(request, req.SourceFormat, req.Format)
	raw, err := callHost("host.model.execute_stream", rpcHostModelExecutionRequest{
		EntryProtocol:  service.FirstNonEmpty(req.SourceFormat, "openai"),
		ExitProtocol:   service.FirstNonEmpty(req.Format, req.SourceFormat, "openai"),
		Model:          strings.TrimSpace(req.Model),
		Stream:         true,
		Body:           request,
		Headers:        req.Headers,
		Query:          req.Query,
		Alt:            req.Alt,
		HostCallbackID: req.HostCallbackID,
	})
	if err != nil {
		claim.release(0)
		_, _ = svc.Release(ctx, reservation.ID)
		return err
	}
	var stream rpcHostModelStreamResponse
	if err := json.Unmarshal(raw, &stream); err != nil {
		claim.release(0)
		_, _ = svc.Release(ctx, reservation.ID)
		return err
	}
	if stream.StatusCode >= 400 {
		_ = closeHostModelStream(stream.StreamID)
		settleReservation(svc, plan, reservation, req, startedAt, time.Time{}, time.Now(), stream.StatusCode, usageparse.Usage{}, "", http.StatusText(stream.StatusCode), claim, true)
		return fmt.Errorf("host model status %d", stream.StatusCode)
	}
	if strings.TrimSpace(stream.StreamID) == "" {
		claim.release(0)
		_, _ = svc.Release(ctx, reservation.ID)
		return errors.New("empty host stream id")
	}
	defer func() { _ = closeHostModelStream(stream.StreamID) }()

	acc := &usageparse.Accumulator{}
	var firstChunkAt, completedAt time.Time
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	stopWatch := startStreamIdleWatchdog(stream.StreamID, &lastProgress)
	defer stopWatch()
	for {
		chunkRaw, errRead := callHost("host.model.stream_read", rpcHostModelStreamReadRequest{StreamID: stream.StreamID})
		if errRead != nil {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, plan, reservation, req, startedAt, firstChunkAt, completedAt, 502, parsed, acc.Model(), errRead.Error(), claim, firstChunkAt.IsZero())
			return errRead
		}
		var chunk rpcHostModelStreamReadResponse
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, plan, reservation, req, startedAt, firstChunkAt, completedAt, 502, parsed, acc.Model(), err.Error(), claim, firstChunkAt.IsZero())
			return err
		}
		if chunk.Error != "" {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, plan, reservation, req, startedAt, firstChunkAt, completedAt, 502, parsed, acc.Model(), chunk.Error, claim, firstChunkAt.IsZero())
			return fmt.Errorf("%s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			// 用量逐块增量解析，不在本地留存流副本（旧版曾缓冲整条流用于
			// 结算兜底，每请求最多数 MB，是内存占用的主要来源）。
			acc.FeedChunk(chunk.Payload)
			lastProgress.Store(time.Now().UnixNano())
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
			}
			if err := emitPluginStreamChunk(pluginStreamID, chunk.Payload); err != nil {
				completedAt = time.Now()
				parsed, _ := acc.Result()
				settleReservation(svc, plan, reservation, req, startedAt, firstChunkAt, completedAt, 499, parsed, acc.Model(), err.Error(), claim, firstChunkAt.IsZero())
				return err
			}
		}
		if chunk.Done {
			break
		}
	}
	completedAt = time.Now()
	parsed, _ := acc.Result()
	// 先结束对客户端的流，再等宿主用量：等待不占用客户端时延。
	// 部分 OpenAI 兼容上游不在流里回 usage，此时预占估算会成为入账金额；
	// 宿主 usage.handle 是权威口径，等到它才能按真实 token 计费。
	closeStream("")
	if parsed.IsZero() {
		if rec, ok := claim.wait(svc.Config().Quota.Settlement.HostUsageWait.Std()); ok {
			parsed = usageFromRecord(rec)
			r := buildRequest(svc, reservation, req, plan.Meta, startedAt, firstChunkAt, completedAt, 200)
			r.UpstreamModel = acc.Model()
			applyHostUsageToRequest(r, rec)
			return finishSettle(svc, reservation, r, parsed, claim, firstChunkAt.IsZero())
		}
	}
	r := buildRequest(svc, reservation, req, plan.Meta, startedAt, firstChunkAt, completedAt, 200)
	r.UpstreamModel = acc.Model()
	return finishSettle(svc, reservation, r, parsed, claim, firstChunkAt.IsZero())
}

// settleReservation 解析 usage 结算预占并写请求记录；结算失败时释放预占兜底。
// upstreamModel 是执行器从上游响应里嗅探到的真实模型名（可为空）。
// noResponse 表示上游未产生任何响应数据（HTTP 4xx/5xx、空响应体、流一条块
// 都没收到）——零用量时不再按预占估算入账（真实成本为零）。
// errNote 是失败原因摘要（路由流量的目标转移轨迹也在此），
// 写入前经 store.SanitizeErrorNote 清洗截断。
func settleReservation(svc *service.Service, plan service.ReservePlan, reservation store.Reservation, req rpcExecutorRequest, startedAt, firstChunkAt, completedAt time.Time, status int, usage usageparse.Usage, upstreamModel, errNote string, claim *usageClaim, noResponse bool) {
	r := buildRequest(svc, reservation, req, plan.Meta, startedAt, firstChunkAt, completedAt, status)
	r.UpstreamModel = upstreamModel
	r.ErrorNote = errNote
	// 认领已在结算前收到宿主口径（少见但可能）：结算前补齐展示字段，
	// 使请求行与它的分钟聚合维度一致。
	if rec, ok := claim.wait(0); ok {
		applyHostUsageToRequest(r, rec)
		if usage.IsZero() {
			usage = usageFromRecord(rec)
		}
	}
	_ = finishSettle(svc, reservation, r, usage, claim, noResponse)
}

// finishSettle 落库结算结果，并把请求行 ID 交给认领：
// 宽限期内晚到的宿主用量据此回填，无需再靠库内启发式判重。
func finishSettle(svc *service.Service, reservation store.Reservation, r *store.Request, usage usageparse.Usage, claim *usageClaim, noResponse bool) error {
	ctx := context.Background()
	_, err := svc.Settle(ctx, reservation.ID, usage, r, noResponse)
	if err != nil {
		// 未落库请求行：放弃认领，宿主回调回落到被动统计，避免用量凭空丢失。
		// 若认领已先一步收到宿主口径，此刻已无法消费——置 released 后该记录
		// 若再来会走被动入库，但已 attach 的这份只能丢弃，留日志可查。
		if rec, ok := claim.wait(0); ok {
			warnf("结算失败且认领已收到宿主用量（model=%s in=%d out=%d），该口径随放弃认领丢弃: %v",
				rec.Model, rec.Detail.InputTokens, rec.Detail.OutputTokens, err)
		}
		claim.release(0)
		_, _ = svc.Release(ctx, reservation.ID)
		return err
	}
	if rec := claim.settled(r.ID); rec != nil {
		// 结算与交付并发时由这里补回填（attach 侧当时还看不到请求 ID）。
		backfillFromHostUsage(svc, r.ID, *rec)
	}
	claim.release(usageClaimGrace)
	return nil
}

// backfillFromHostUsage 用宿主口径回填已落库请求行的 token 明细与首字延迟。
func backfillFromHostUsage(svc *service.Service, requestID string, rec rpcUsageRecord) {
	if rec.Detail.InputTokens <= 0 && rec.Detail.OutputTokens <= 0 && rec.Detail.TotalTokens <= 0 {
		return
	}
	err := svc.BackfillRequestUsageByID(context.Background(), requestID, store.UsageBackfill{
		InputTokens:         rec.Detail.InputTokens,
		OutputTokens:        rec.Detail.OutputTokens,
		ReasoningTokens:     rec.Detail.ReasoningTokens,
		CachedTokens:        rec.Detail.CachedTokens,
		CacheReadTokens:     rec.Detail.CacheReadTokens,
		CacheCreationTokens: rec.Detail.CacheCreationTokens,
		TotalTokens:         rec.Detail.TotalTokens,
		TTFTMS:              rec.TTFT.Milliseconds(),
	})
	if err != nil {
		warnf("回填请求 %s 的宿主用量失败: %v", requestID, err)
	}
}

func buildRequest(svc *service.Service, reservation store.Reservation, req rpcExecutorRequest, meta service.RequestMeta, startedAt, firstChunkAt, completedAt time.Time, status int) *store.Request {
	r := &store.Request{
		ID:                service.NewUUID(),
		TS:                startedAt,
		KeyID:             reservation.KeyID,
		CallerID:          reservation.CallerID,
		Model:             reservation.Model,
		Provider:          req.AuthProvider,
		Source:            store.RedactSource(req.SourceFormat),
		AuthID:            req.AuthID,
		AuthType:          req.AuthType,
		Tier:              meta.ResolvedTier,
		ThinkingIntensity: meta.ResolvedThinking,
		LatencyMS:         millisBetween(startedAt, completedAt),
		TTFTMS:            millisBetween(startedAt, firstChunkAt),
		GenerationMS:      millisBetween(firstChunkAt, completedAt),
		CostMicroUSD:      0,
		Priced:            true,
	}
	if status >= http.StatusBadRequest {
		r.Result = store.ResultError
		r.StatusCode = status
	} else {
		r.Result = store.ResultOK
	}
	return r
}

func millisBetween(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return 0
	}
	return to.Sub(from).Milliseconds()
}

// hostErrorNote 从宿主响应构造失败摘要：HTTP 错误行（状态码+原因短语），
// 传输层失败时为空串（错误文本在错误信封里，客户端已可见；行内记状态码
// 已足够定位）。最终清洗截断由 store.SanitizeErrorNote 在写入点完成。
func hostErrorNote(status int, hostBody []byte) string {
	if status < http.StatusBadRequest {
		return ""
	}
	note := fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))
	// 上游错误体里的 message 字段比状态短语更有定位价值，浅取一层。
	var dec struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(hostBody, &dec) == nil && dec.Error.Message != "" {
		note += ": " + dec.Error.Message
	}
	return note
}

func requestBody(req rpcExecutorRequest) []byte {
	if len(req.OriginalRequest) > 0 {
		return req.OriginalRequest
	}
	return req.Payload
}

// requestBodyWithStreamUsage 为 OpenAI 系流式请求注入 stream_options.include_usage。
//
// 用 json.Decoder 只定位顶层 stream_options 的字节区间，按位置做字节级
// 拼接：直连流式路径此前对每个请求整包 Unmarshal+Marshal 两个来回（长
// 上下文请求数 MB，是热路径上最大的无谓分配，且 Marshal 会重排序键并
// HTML 转义 `<>&`），现在除注入点外原文体逐字节保留。
func requestBodyWithStreamUsage(body []byte, sourceFormat, outputFormat string) []byte {
	format := strings.ToLower(service.FirstNonEmpty(outputFormat, sourceFormat))
	if strings.Contains(format, "claude") || strings.Contains(format, "gemini") {
		return body
	}
	if len(body) == 0 {
		return body
	}

	// 找顶层 stream_options 的值区间：optsStart<0 表示顶层没有该键。
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil {
		return body // 非 JSON 体原样返回
	} else if d, ok := t.(json.Delim); !ok || d != '{' {
		return body // 顶层不是对象
	}
	optsStart, optsEnd := -1, -1
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return body
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return body
		}
		if key, _ := keyTok.(string); key == "stream_options" {
			optsEnd = int(dec.InputOffset())
			optsStart = optsEnd - len(raw)
			break
		}
	}

	if optsStart < 0 {
		// 顶层无 stream_options：在起始 { 后整键插入（空对象不带前导逗号）。
		i := bytes.IndexByte(body, '{')
		if i < 0 {
			return body
		}
		j := i + 1
		for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n' || body[j] == '\r') {
			j++
		}
		insert := []byte(`"stream_options":{"include_usage":true}`)
		out := make([]byte, 0, len(body)+len(insert)+1)
		out = append(out, body[:i+1]...)
		out = append(out, insert...)
		if j < len(body) && body[j] != '}' {
			out = append(out, ',')
		}
		return append(out, body[i+1:]...)
	}

	raw := json.RawMessage(bytes.TrimLeft(body[optsStart:optsEnd], " \t\r\n"))
	if head, ok := rawObjectHead(raw); ok {
		// 已是对象：含 include_usage（无论取值）则原样返回，否则在对象
		// 起始 { 后补键（与旧 map 口径一致——只看键存在性）。
		if rawObjectHasKey(raw, "include_usage") {
			return body
		}
		insert := []byte(`"include_usage":true`)
		empty := rawObjectEmpty(raw)
		out := make([]byte, 0, len(body)+len(insert)+1)
		out = append(out, body[:optsStart+head+1]...)
		out = append(out, insert...)
		if !empty {
			out = append(out, ',')
		}
		return append(out, body[optsStart+head+1:]...)
	}
	// stream_options 存在但不是对象（null/标量/数组）：整体替换为对象，
	// 与旧 map 路径的覆盖行为一致。
	out := make([]byte, 0, len(body)+len(`{"include_usage":true}`))
	out = append(out, body[:optsStart]...)
	out = append(out, `{"include_usage":true}`...)
	return append(out, body[optsEnd:]...)
}

// rawObjectHead 报告 raw 以对象起始（返回 { 的偏移）。
func rawObjectHead(raw []byte) (int, bool) {
	i := bytes.IndexByte(raw, '{')
	return i, i >= 0
}

// rawObjectEmpty 报告 raw 是空对象 {}（允许内部空白）。
func rawObjectEmpty(raw []byte) bool {
	i := bytes.IndexByte(raw, '{')
	if i < 0 {
		return false
	}
	j := i + 1
	for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
		j++
	}
	return j < len(raw) && raw[j] == '}'
}

// rawObjectHasKey 扫描 raw 顶层对象是否含键 want（只扫第一层）。
func rawObjectHasKey(raw []byte, want string) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil {
		return false
	} else if d, ok := t.(json.Delim); !ok || d != '{' {
		return false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return false
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
		if key, _ := keyTok.(string); key == want {
			return true
		}
	}
	return false
}

// notifySweepLoop 每分钟做一次周期扫描：告警（额度越线/密钥过期 → shoutrrr
// 端点）与定期报告（日/周/月报到期发送）。两者内部各自以租约持有者身份执行。
func notifySweepLoop(svc *service.Service, stop <-chan struct{}) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			_, _ = svc.RunNotifySweep(ctx)
			_, _ = svc.RunReportsSweep(ctx)
			cancel()
		}
	}
}

// startReservationHeartbeat 把预占登记进服务层的集中心跳注册表：
// 单个后台协程批量续期全部在途预占，不再每请求各起一个 ticker goroutine。
func startReservationHeartbeat(svc *service.Service, reservationID string) func() {
	return svc.TrackReservation(reservationID)
}

// autoBackupLoop 每日一次把库快照写进 backup.dir 并轮转 keep 份。
// 触发时刻按本机本地时区的 backup.hour；启动时当天时刻已过且尚未备份则立即
// 补一份（重启不漏当天）。仅租约持有者执行，多实例部署不重复落盘。
func autoBackupLoop(svc *service.Service, cfg config.BackupConfig, stop <-chan struct{}) {
	dir := cfg.Dir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(svc.Config().DataDir, dir)
	}
	lastDay := ""
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	fire := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := svc.RunAutoBackup(ctx, dir, cfg.Keep); err != nil {
			log.Printf("cpa-usage-manager: 自动备份失败: %v", err)
			ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel2()
			svc.NotifyErrorEvent(ctx2, "report", "定时自动备份失败: "+err.Error())
		}
	}
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			now := time.Now()
			day := now.Format("2006-01-02")
			if lastDay == day || now.Hour() < cfg.Hour {
				continue
			}
			lastDay = day
			if svc.Store().Writable() {
				fire()
			} else {
				lastDay = "" // 只读实例不占当天名额，接管租约后照常补备份
			}
		}
	}
}

// fxRefreshLoop 周期性刷新 USD→CNY 汇率。此前汇率是纯懒加载：只有面板有人
// 调 /exchange-rate 才可能触发上游拉取，面板无人打开时缓存会一直陈旧到
// 下一次访问；CNY 计价规则保存时锁定的汇率兜底也因此长期停在旧值。
// 刷新间隔远小于 fx.DefaultTTL（6h），保证任何时刻被使用的汇率都是新鲜的。
// 失败静默重试（兜底值仍在，不影响任何功能）；仅租约持有者执行——
// 汇率缓存写在共享的 meta 表，非持有者刷新是白白打上游。
func fxRefreshLoop(svc *service.Service, stop <-chan struct{}) {
	fire := func() {
		if !svc.Store().Writable() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// RefreshExchangeRate 内部已有双源与兜底逻辑，这里的错误仅记录。
		if _, err := svc.RefreshExchangeRate(ctx); err != nil {
			log.Printf("cpa-usage-manager: 汇率自动刷新失败（下次重试）: %v", err)
		}
	}
	fire() // 启动即刷一次：不依赖面板被打开
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			fire()
		}
	}
}

func hostModelExecute(hostCallbackID string, req rpcExecutorRequest, model string, body []byte, stream bool) ([]byte, http.Header, int, error) {
	raw, err := hostCall("host.model.execute", rpcHostModelExecutionRequest{
		EntryProtocol:  service.FirstNonEmpty(req.SourceFormat, "openai"),
		ExitProtocol:   service.FirstNonEmpty(req.Format, req.SourceFormat, "openai"),
		Model:          strings.TrimSpace(model),
		Stream:         stream,
		Body:           body,
		Headers:        req.Headers,
		Query:          req.Query,
		Alt:            req.Alt,
		HostCallbackID: hostCallbackID,
	})
	if err != nil {
		return nil, nil, 0, err
	}
	var resp rpcHostModelExecutionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, 0, err
	}
	return resp.Body, resp.Headers, resp.StatusCode, nil
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	_, err := hostCall("host.stream.emit", rpcStreamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func closePluginStream(streamID, errMsg string) {
	_, _ = hostCall("host.stream.close", rpcStreamCloseRequest{StreamID: streamID, Error: strings.TrimSpace(errMsg)})
}

func closeHostModelStream(streamID string) error {
	if strings.TrimSpace(streamID) == "" {
		return nil
	}
	_, err := hostCall("host.model.stream_close", rpcHostModelStreamCloseRequest{StreamID: streamID})
	return err
}

// streamIdleTimeout 是读泵的无进度守护阈值：超过该时长没收到任何新块、
// 也没有 done/error，视为宿主侧挂死，主动关闭宿主流以解除阻塞的
// stream_read。宿主回调是 C ABI 同步调用、无取消语义——不设防的话一次
// 挂死的 stream_read 会让本 goroutine 卡死，且在途预占被心跳无限续命，
// stale_reservation_timeout 永远扫不到。阈值取远大于思考模型的合法出块
// 间隔：只兜「永不返回」的底，不干扰慢流。
const streamIdleTimeout = 10 * time.Minute

// startStreamIdleWatchdog 启动读泵的无进度守护。progress 由读泵在每收到
// 一块时刷新（Unix 纳秒，原子）；守护触发即关闭宿主流（应使阻塞中的
// stream_read 返回错误或 done），随后自行退出。stop 必须在泵返回前调用。
func startStreamIdleWatchdog(streamID string, progress *atomic.Int64) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, progress.Load())) > streamIdleTimeout {
					warnf("宿主流 %s 超过 %v 无新数据，主动关闭以解除阻塞的 stream_read", streamID, streamIdleTimeout)
					_ = closeHostModelStream(streamID)
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// ---------- 请求拦截 / 生命周期（图片/视频路径） ----------

func interceptAfter(body []byte) ([]byte, error) {
	_, svc, _, _ := current()
	if svc == nil {
		return okEnvelope(rpcRequestInterceptResponse{})
	}
	var req rpcRequestInterceptRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if !isImageProtocol(req.SourceFormat) && !isImageOnlyModel(service.FirstNonEmpty(req.RequestedModel, req.Model)) {
		return okEnvelope(rpcRequestInterceptResponse{})
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return okEnvelope(rpcRequestInterceptResponse{})
	}
	ctx := context.Background()
	key, err := svc.ResolveIdentity(ctx, req.Headers, req.Metadata)
	if err != nil {
		// 非 cum 凭证（原生 API Key）不拦截，交还宿主原生路径。
		if _, isCum := service.ParseKeyID(bearerToken(req.Headers)); !isCum {
			return okEnvelope(rpcRequestInterceptResponse{})
		}
		return okEnvelope(rejectResponse(http.StatusUnauthorized, "unauthorized: "+err.Error()))
	}
	plan, err := svc.BuildReservePlan(ctx, service.FirstNonEmpty(req.RequestedModel, req.Model), req.Body)
	if err != nil {
		if errors.Is(err, service.ErrModelDisabled) {
			return okEnvelope(rejectResponse(http.StatusForbidden, err.Error(), "model_disabled"))
		}
		return okEnvelope(rejectResponse(http.StatusPaymentRequired, err.Error()))
	}
	reservation, err := svc.Reserve(ctx, service.ReservationRequest{KeyID: key.KID, CallerID: key.CallerID, Model: plan.Model, EstimatedTokens: plan.TokenEstimate, EstimatedImages: plan.ImageCount, Actor: "quota"})
	if err != nil {
		if errors.Is(err, service.ErrModelNotAllowed) {
			return okEnvelope(rejectResponse(http.StatusForbidden, err.Error(), "model_not_allowed"))
		}
		return okEnvelope(rejectResponse(http.StatusTooManyRequests, err.Error()))
	}
	imageHoldsMu.Lock()
	removed := 0
	for id, h := range imageHolds {
		if h.created.Before(time.Now().Add(-imageHoldMaxAge)) {
			delete(imageHolds, id)
			removed++
			if h.stopHeart != nil {
				h.stopHeart() // 停掉残留条目的心跳 ticker，回收 goroutine
			}
		}
	}
	// Go 的 map 删除元素后不会缩容：一次突发把 bucket 数组撑大后即使清空
	// 也照常常驻。归零时重建，把数组还回去（与 regexpCache 超限重建同思路）。
	if removed > 0 && len(imageHolds) == 0 {
		imageHolds = make(map[string]imageHold)
	}
	imageHolds[requestID] = imageHold{
		reservation: reservation,
		plan:        plan,
		stopHeart:   startReservationHeartbeat(svc, reservation.ID),
		claim:       registerUsageClaim(key.KID, plan.Model, req.Model, req.RequestedModel),
		created:     time.Now(),
	}
	imageHoldsMu.Unlock()
	return okEnvelope(rpcRequestInterceptResponse{})
}

func completeIntercepted(body []byte) ([]byte, error) {
	_, svc, _, _ := current()
	if svc == nil {
		return okEnvelope(map[string]any{})
	}
	var req rpcRequestCompletion
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	requestID := strings.TrimSpace(req.RequestID)
	imageHoldsMu.Lock()
	hold, ok := imageHolds[requestID]
	if ok {
		delete(imageHolds, requestID)
	}
	imageHoldsMu.Unlock()
	if !ok {
		return okEnvelope(map[string]any{})
	}
	if hold.stopHeart != nil {
		hold.stopHeart()
	}
	ctx := context.Background()
	if req.Outcome != "succeeded" {
		hold.claim.release(0)
		_, _ = svc.Release(ctx, hold.reservation.ID)
		return okEnvelope(map[string]any{})
	}
	r := buildRequest(svc, hold.reservation, rpcExecutorRequest{}, hold.plan.Meta, req.StartedAt, time.Time{}, req.CompletedAt, req.StatusCode)
	if rec, ok := hold.claim.wait(0); ok {
		applyHostUsageToRequest(r, rec)
	}
	_ = finishSettle(svc, hold.reservation, r, usageparse.Usage{ImageCount: hold.plan.ImageCount}, hold.claim, false)
	return okEnvelope(map[string]any{})
}

func rejectResponse(status int, message string, codes ...string) rpcRequestInterceptResponse {
	code := "limit_rejected"
	kind := "insufficient_quota"
	if len(codes) > 0 && strings.TrimSpace(codes[0]) != "" {
		code = codes[0]
		kind = codes[0]
	}
	if status <= 0 {
		status = http.StatusForbidden
	}
	bodyJSON, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": kind, "code": code},
	})
	return rpcRequestInterceptResponse{Terminate: true, StatusCode: status, ResponseBody: bodyJSON}
}

// ---------- 管理接口 ----------

func handleManagement(body []byte) ([]byte, error) {
	var req rpcManagementRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	_, _, api, _ := current()
	if api == nil {
		return errorEnvelope("service_unavailable", "插件尚未注册"), nil
	}
	rw := newRPCResponseWriter()
	api.Handler().ServeHTTP(rw, managementRequest(req))
	return okEnvelope(rpcManagementResponse{StatusCode: rw.code, Headers: rw.hdr, Body: rw.buf.Bytes()})
}

// managementRequest 把宿主转发的管理调用还原为 net/http 请求。手工构造替代
// httptest.NewRequest：管理 RPC 只消费 method/path/query/header/body 五要素，
// 后者每次调用都为 RemoteAddr 等完整测试语义付费。
func managementRequest(req rpcManagementRequest) *http.Request {
	target := strings.TrimSpace(req.Path)
	u, err := url.Parse(target)
	if err != nil || (u.Path == "" && u.RawQuery == "") {
		u = &url.URL{Path: target}
	}
	if len(req.Query) > 0 {
		u.RawQuery = req.Query.Encode()
	}
	hdr := make(http.Header, len(req.Headers))
	for k, vals := range req.Headers {
		for _, v := range vals {
			hdr.Add(k, v)
		}
	}
	return &http.Request{
		Method:        req.Method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(req.Body)),
		ContentLength: int64(len(req.Body)),
		Host:          u.Host,
	}
}

// rpcResponseWriter 是管理 RPC 的最小 ResponseWriter：把处理器输出收进缓冲，
// 随信封 JSON 回传宿主，不携带 httptest.NewRecorder 用不到的快照语义。
type rpcResponseWriter struct {
	code int
	hdr  http.Header
	buf  bytes.Buffer
}

func newRPCResponseWriter() *rpcResponseWriter { return &rpcResponseWriter{hdr: http.Header{}} }

func (w *rpcResponseWriter) Header() http.Header { return w.hdr }

func (w *rpcResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *rpcResponseWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.buf.Write(p)
}

// ---------- 宿主用量认领 ----------
//
// 宿主的 usage.handle 回调不携带请求 ID：历史版本只能在库里按
// 「时间±15s + 延迟±150ms + 模型候选」猜哪条记录是同一请求，既会漏判
// （于是同一请求被记两次，统计翻倍）也可能误判。
//
// 认领把这件事拉回进程内：执行器在调用宿主上游之前登记一条认领，
// usage.handle 命中后把宿主口径交给该请求消费，自己不再入库。
// 认领是唯一权威的配对依据，库内启发式判重降级为跨进程/晚到回调的兜底。

const (
	// usageClaimGrace 是结算完成后继续持有认领的宽限期。
	// 宿主通常在插件执行器返回之后才记账，晚到的回调必须仍被本次请求吸收。
	usageClaimGrace = 8 * time.Second
	// usageClaimMaxAge 是认领的绝对存活上限，用于兜底清理异常路径的残留。
	usageClaimMaxAge = 10 * time.Minute
)

type usageClaim struct {
	models  map[string]bool
	keyID   string
	created time.Time
	// buckets 是该认领登记到的全部模型桶（models 的每个键一个），
	// 注销时据此 O(模型数) 摘除，不再全局线性查找。
	buckets    []*claimBucket
	registered bool

	mu       sync.Mutex
	released bool
	rec      *rpcUsageRecord
	reqID    string
	ready    chan struct{}
}

// claimBucket 是同一归一化模型名下的在途认领集合。
// 此前所有认领共享一条切片与一把全局锁，登记/注销/宿主回调匹配全部串行；
// 分桶后竞争只发生在同模型请求之间，跨模型完全并行。
type claimBucket struct {
	mu     sync.Mutex
	claims []*usageClaim
}

var (
	claimBucketsMu sync.Mutex
	claimBuckets   map[string]*claimBucket
)

// claimBucketFor 返回模型名对应的桶，惰性创建。桶在无引用后不主动回收：
// 每个仅含一把互斥锁与一个切片头，量级受「出现过的不同模型数」约束，可忽略。
func claimBucketFor(model string) *claimBucket {
	claimBucketsMu.Lock()
	defer claimBucketsMu.Unlock()
	if claimBuckets == nil {
		claimBuckets = make(map[string]*claimBucket)
	}
	b, ok := claimBuckets[model]
	if !ok {
		b = &claimBucket{}
		claimBuckets[model] = b
	}
	return b
}

// normalizeModelKey 归一化模型名：小写、去空白、去「渠道/」前缀。
// 执行器登记的是「渠道/模型」别名，宿主上报的是裸模型名，裸名才是共同锚点。
func normalizeModelKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "/"); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	return s
}

// registerUsageClaim 登记一次认领；models 是该请求可能被宿主上报的模型名。
// 登记时顺带惰性清理同桶内已过期的认领（绝对存活上限兜底，异常路径残留）。
func registerUsageClaim(keyID string, models ...string) *usageClaim {
	c := &usageClaim{
		models:  make(map[string]bool, len(models)),
		keyID:   strings.TrimSpace(keyID),
		created: time.Now(),
		ready:   make(chan struct{}),
	}
	for _, m := range models {
		if k := normalizeModelKey(m); k != "" {
			c.models[k] = true
		}
	}
	if len(c.models) == 0 {
		// 没有可匹配的模型名，认领不可能命中：不入册，等价于关闭认领。
		return c
	}
	c.registered = true
	cutoff := time.Now().Add(-usageClaimMaxAge)
	for model := range c.models {
		b := claimBucketFor(model)
		b.mu.Lock()
		kept := b.claims[:0]
		for _, v := range b.claims {
			if v.created.After(cutoff) {
				kept = append(kept, v)
			}
		}
		b.claims = append(kept, c)
		b.mu.Unlock()
		c.buckets = append(c.buckets, b)
	}
	return c
}

// release 注销认领。delay > 0 时延后注销，宽限期内继续吸收晚到的回调。
// delay 为 0 表示立即放弃认领：此后该请求的宿主回调回落到被动统计路径。
func (c *usageClaim) release(delay time.Duration) {
	if c == nil || !c.registered {
		return
	}
	if delay > 0 {
		time.AfterFunc(delay, func() { c.release(0) })
		return
	}
	// 先在 c.mu 内置 released 再摘桶：与 attach/openFor 同锁互斥，保证
	// 「选择命中（openFor）→ 交付（attach）」窗口里发生的放弃认领不会让
	// 交付落在已无人消费的认领上——那会导致宿主用量既不挂请求行也不被动
	// 入库，凭空丢失。置位后 attach 返回 false，调用方回落被动统计路径。
	c.mu.Lock()
	c.released = true
	c.mu.Unlock()
	for _, b := range c.buckets {
		b.mu.Lock()
		for i, v := range b.claims {
			if v == c {
				b.claims = append(b.claims[:i], b.claims[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
	}
	c.buckets = nil
}

// settled 登记本次认领已落库的请求行 ID，并返回此前已交付的宿主用量（若有）。
// 与 attach 互斥：两者中恰好一方能看到对方的数据，回填只会发生一次。
func (c *usageClaim) settled(requestID string) *rpcUsageRecord {
	if c == nil || !c.registered {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqID = requestID
	return c.rec
}

// attach 交付一条宿主用量记录，返回已落库的请求行 ID（尚未结算时为空）。
// 已交付过或已放弃（released）的认领拒绝交付：前者防重复，后者保证放弃后
// 的宿主回调回落被动统计而不是被吞掉。
func (c *usageClaim) attach(u rpcUsageRecord) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec != nil || c.released {
		return "", false
	}
	rec := u
	c.rec = &rec
	close(c.ready)
	return c.reqID, true
}

// openFor 报告认领在模型 k 的桶中是否仍可吸收回调（未放弃、未交付过且登记了该模型）。
func (c *usageClaim) openFor(k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.released && c.rec == nil && c.models[k]
}

// wait 等待宿主交付用量，最长 d；d <= 0 时只做一次非阻塞探测。
func (c *usageClaim) wait(d time.Duration) (rpcUsageRecord, bool) {
	if c == nil || !c.registered {
		return rpcUsageRecord{}, false
	}
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-c.ready:
		case <-timer.C:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec == nil {
		return rpcUsageRecord{}, false
	}
	return *c.rec, true
}

// claimHostUsage 为一条宿主用量记录寻找匹配的认领。
// 命中返回（已落库的请求行 ID，true）；未命中返回 ("", false)，由调用方被动入库。
//
// 选择语义与旧的全局切片版一致：同模型并发时优先 kid 精确匹配，
// 否则取最早登记的（FIFO）。扫描按模型桶进行，顺带惰性摘除过期残留。
func claimHostUsage(u rpcUsageRecord) (string, bool) {
	keys := []string{normalizeModelKey(u.Model), normalizeModelKey(u.Alias)}
	if keys[0] == "" && keys[1] == "" {
		return "", false
	}
	kid, _ := service.ParseKeyID(u.APIKey)
	var best *usageClaim
	better := func(c *usageClaim) bool {
		if best == nil {
			return true
		}
		if kid != "" {
			if c.keyID == kid && best.keyID != kid {
				return true
			}
			if c.keyID != kid && best.keyID == kid {
				return false
			}
		}
		return c.created.Before(best.created)
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		claimBucketsMu.Lock()
		b := claimBuckets[k]
		claimBucketsMu.Unlock()
		if b == nil {
			continue
		}
		cutoff := time.Now().Add(-usageClaimMaxAge)
		b.mu.Lock()
		kept := b.claims[:0]
		for _, c := range b.claims {
			if c.created.Before(cutoff) {
				continue
			}
			kept = append(kept, c)
			if c.openFor(k) && better(c) {
				best = c
			}
		}
		b.claims = kept
		b.mu.Unlock()
	}
	if best == nil {
		return "", false
	}
	return best.attach(u)
}

func resetUsageClaims() {
	claimBucketsMu.Lock()
	claimBuckets = nil
	claimBucketsMu.Unlock()
}

// applyHostUsageToRequest 用宿主口径补齐请求行的缺失字段（结算前调用）。
// 只补空值：执行器自己观测到的数据更贴近插件视角，不被覆盖。
// 结算后不可用此函数——provider/auth_type/tier 是分钟聚合的维度键，
// 改写会让请求行与它的聚合行错位。
func applyHostUsageToRequest(r *store.Request, rec rpcUsageRecord) {
	if r == nil {
		return
	}
	if r.Provider == "" {
		r.Provider = strings.TrimSpace(rec.Provider)
	}
	if r.UpstreamModel == "" {
		// 宿主上报的 Model 是上游实际路由名；与别名不同即发生了二次路由。
		if m := strings.TrimSpace(rec.Model); m != "" && m != r.Model {
			r.UpstreamModel = m
		}
	}
	if r.AuthID == "" {
		r.AuthID = strings.TrimSpace(rec.AuthID)
	}
	if r.AuthType == "" {
		r.AuthType = strings.TrimSpace(rec.AuthType)
	}
	if r.Tier == "" {
		r.Tier = strings.TrimSpace(rec.ServiceTier)
	}
	if r.ThinkingIntensity == "" {
		r.ThinkingIntensity = strings.TrimSpace(rec.ReasoningEffort)
	}
	if r.TTFTMS == 0 && rec.TTFT > 0 {
		r.TTFTMS = rec.TTFT.Milliseconds()
	}
}

// ---------- usage.handle ----------

func handleUsage(body []byte) ([]byte, error) {
	st, svc, _, cfg := current()
	if st == nil || len(body) == 0 {
		return okEnvelope(map[string]any{})
	}
	var u rpcUsageRecord
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	ctx := context.Background()
	bf := store.UsageBackfill{
		InputTokens:         u.Detail.InputTokens,
		OutputTokens:        u.Detail.OutputTokens,
		ReasoningTokens:     u.Detail.ReasoningTokens,
		CachedTokens:        u.Detail.CachedTokens,
		CacheReadTokens:     u.Detail.CacheReadTokens,
		CacheCreationTokens: u.Detail.CacheCreationTokens,
		TotalTokens:         u.Detail.TotalTokens,
		TTFTMS:              u.TTFT.Milliseconds(),
	}
	hasDetail := bf.InputTokens > 0 || bf.OutputTokens > 0 || bf.TotalTokens > 0

	// 认领优先：本次回调若属于插件执行器正在处理/刚结算的请求，
	// 由该请求消费宿主口径，绝不再被动入库——这是重复统计的根因所在。
	if id, claimed := claimHostUsage(u); claimed {
		if id != "" && hasDetail {
			if err := svc.BackfillRequestUsageByID(ctx, id, bf); err != nil {
				warnf("回填请求 %s 的宿主用量失败: %v", id, err)
			}
		}
		return okEnvelope(map[string]any{})
	}

	// 认领未命中（宽限期已过、跨进程回调、或纯统计模式）：回落到库内启发式判重。

	// ai_judge 子调用的回调没有密钥身份（插件以自身名义直连宿主，不带
	// 密钥头）：命中评判归属窗口时改记到触发请求的 Key 名下，来源固定
	// 标记为 ai_judge——否则明细里出现无主「-」行，还会被下方判重启发式误伤。
	if attr, ok := svc.AttributeJudgeUsage(u.Model); ok {
		req := usageRecordToRequest(st, u)
		req.KeyID = attr.KID
		req.CallerID = attr.CallerID
		req.Source = "ai_judge"
		if req.CallerID == "" {
			req.CallerID = store.DefaultCallerID
		}
		if cost, native, cur, _, perr := svc.PriceNative(req.Model, usageFromRecord(u)); perr == nil {
			req.CostMicroUSD = cost
			req.CostNativeMicro = native
			req.Currency = cur
		}
		// 身份已确定，不带判重候选直接落库。
		if err := st.RecordPassiveUsage(ctx, req, store.PassiveDedupeHint{Near: req.TS}); err != nil {
			warnf("记录 ai_judge 用量失败: %v", err)
		}
		return okEnvelope(map[string]any{})
	}

	if cfg.Quota.Enabled {
		models := modelCandidates(u.Model, u.Alias)
		if kid, isCum := service.ParseKeyID(u.APIKey); isCum {
			// cum- 密钥的流量必然经过执行器，已一次性入库；这里只补 token 明细。
			if hasDetail {
				if _, err := svc.BackfillRequestUsage(ctx, kid, models, u.RequestedAt, bf); err != nil {
					warnf("回填 Key %s 的宿主用量失败: %v", kid, err)
				}
			}
			return okEnvelope(map[string]any{})
		}
		// APIKey 不是插件 Key 的回调（部分兼容渠道把上游凭据放进该字段）：
		// 若能按 时间+延迟+模型 关联到执行器已入库的记录，视为同一请求，
		// 不再被动入库（否则统计翻倍），仅回填缺失用量。
		if id, dup, err := svc.FindDuplicateExecutor(ctx, models, u.RequestedAt, u.Latency.Milliseconds(),
			u.Detail.TotalTokens, u.Detail.InputTokens); err == nil && dup {
			if hasDetail {
				if err := svc.BackfillRequestUsageByID(ctx, id, bf); err != nil {
					warnf("回填请求 %s 的宿主用量失败: %v", id, err)
				}
			}
			return okEnvelope(map[string]any{})
		}
	}
	req := usageRecordToRequest(st, u)
	if cost, native, cur, _, err := svc.PriceNative(req.Model, usageFromRecord(u)); err == nil {
		req.CostMicroUSD = cost
		req.CostNativeMicro = native
		req.Currency = cur
	}
	// 入库时防重：同一写事务内探测执行器行，命中即合并、不再插行——
	// 双写重复在落库瞬间被消除，无需任何事后对账。上方的只读预检
	// 只是省一次写事务的快路径，这里的原子探测才是正确性保证。
	hint := store.PassiveDedupeHint{
		Models:      modelCandidates(u.Model, u.Alias),
		Near:        u.RequestedAt,
		LatencyMS:   u.Latency.Milliseconds(),
		TotalTokens: u.Detail.TotalTokens,
		InputTokens: u.Detail.InputTokens,
	}
	if err := st.RecordPassiveUsage(ctx, req, hint); err != nil {
		// 用量记录失败不应让宿主把整次请求判为插件错误。
		warnf("被动记录用量失败: %v", err)
	}
	return okEnvelope(map[string]any{})
}

// warnf 把插件内部的非致命异常写到 stderr，由宿主日志收集。
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[cpa-usage-manager] "+format+"\n", args...)
}

func usageFromRecord(u rpcUsageRecord) usageparse.Usage {
	return usageparse.Usage{
		InputTokens:         u.Detail.InputTokens,
		OutputTokens:        u.Detail.OutputTokens,
		ReasoningTokens:     u.Detail.ReasoningTokens,
		CachedTokens:        u.Detail.CachedTokens,
		CacheReadTokens:     u.Detail.CacheReadTokens,
		CacheCreationTokens: u.Detail.CacheCreationTokens,
		TotalTokens:         u.Detail.TotalTokens,
	}
}

// upstreamOf 判定二次路由：别名与上游实际模型名都非空且不同时，返回实际名；
// 否则返回空串（直连）。
func upstreamOf(alias, model string) string {
	alias = strings.TrimSpace(alias)
	model = strings.TrimSpace(model)
	if alias != "" && model != "" && alias != model {
		return model
	}
	return ""
}

// modelCandidates 归并宿主上报的模型名候选：别名、原始模型、去渠道前缀的裸名。
// 执行器落库常用「渠道/模型」别名，而 usage.handle 上报原始模型，判重需按候选集匹配。
func modelCandidates(model, alias string) []string {
	out := make([]string, 0, 3)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, v := range out {
			if v == s {
				return
			}
		}
		out = append(out, s)
	}
	add(alias)
	add(model)
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		add(model[i+1:])
	}
	return out
}

func usageRecordToRequest(st *store.Store, u rpcUsageRecord) store.Request {
	req := store.Request{
		ID: service.NewUUID(),
		TS: u.RequestedAt,
		// 模型展示口径统一用别名：宿主上报的 Model 是上游实际路由名
		// （如 OpenRouter 把 openrouter/ox-alpha 回报为 stealth/ox-alpha），
		// 直接落库会与执行器路径记录的别名割裂成两个维度值。
		Model:               service.FirstNonEmpty(u.Alias, u.Model),
		UpstreamModel:       upstreamOf(u.Alias, u.Model),
		Provider:            u.Provider,
		Source:              store.RedactSource(u.Source),
		AuthID:              u.AuthID,
		AuthType:            u.AuthType,
		Tier:                u.ServiceTier,
		InputTokens:         u.Detail.InputTokens,
		OutputTokens:        u.Detail.OutputTokens,
		ReasoningTokens:     u.Detail.ReasoningTokens,
		CachedTokens:        u.Detail.CachedTokens,
		CacheReadTokens:     u.Detail.CacheReadTokens,
		CacheCreationTokens: u.Detail.CacheCreationTokens,
		TotalTokens:         u.Detail.TotalTokens,
		LatencyMS:           u.Latency.Milliseconds(),
		TTFTMS:              u.TTFT.Milliseconds(),
	}
	if req.TS.IsZero() {
		req.TS = time.Now().UTC()
	}
	if u.Failed {
		req.Result = store.ResultError
	} else {
		req.Result = store.ResultOK
	}
	if kid, ok := service.ParseKeyID(u.APIKey); ok {
		req.KeyID = kid
		if k, err := st.GetKey(context.Background(), kid); err == nil {
			req.CallerID = k.CallerID
		}
	}
	if req.ThinkingIntensity == "" {
		req.ThinkingIntensity = strings.TrimSpace(u.ReasoningEffort)
	}
	if req.CallerID == "" {
		req.CallerID = store.DefaultCallerID
	}
	return req
}

var version = "0.0.1"

func main() { _ = runtime.GOOS }
