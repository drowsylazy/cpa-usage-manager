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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/httpapi"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
	"github.com/google/uuid"
)

var runtimeState struct {
	sync.Mutex
	st  *store.Store
	svc *service.Service
	api *httpapi.API
	cfg config.Config
}

type imageHold struct {
	reservation store.Reservation
	plan        service.ReservePlan
	stopHeart   func()
	claim       *usageClaim
}

var (
	imageHoldsMu sync.Mutex
	imageHolds   = map[string]imageHold{}
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
	raw, err := dispatch(C.GoString(method), body)
	if err != nil {
		writeResponse(out, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(out, raw)
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
	st := runtimeState.st
	runtimeState.st = nil
	runtimeState.svc = nil
	runtimeState.api = nil
	runtimeState.Unlock()
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
				{Name: "quota.enabled", Type: "boolean", Description: "false 退回纯统计（被动 usage 记录），不接管前端鉴权。"},
				{Name: "quota.keys.pepper_env", Type: "string", Description: "pepper 环境变量名（默认 CPA_USAGE_MANAGER_KEY_PEPPERS）。"},
				{Name: "quota.keys.pepper_file", Type: "string", Description: "data_dir 下的 pepper 文件（默认 key-peppers，0600）。"},
				{Name: "quota.keys.active_pepper_id", Type: "string", Description: "签发新 Key 时使用的 pepper 代际。"},
				{Name: "quota.limits.max_token_estimate", Type: "integer", Description: "单请求预占 Token 严格上限。"},
				{Name: "quota.limits.default_output_reserve", Type: "integer", Description: "请求体未给 max_tokens 时的输出预占。"},
				{Name: "quota.limits.require_estimate", Type: "boolean", Description: "true 时缺少用量估算的请求拒绝预占。"},
				{Name: "quota.settlement.missing_usage", Type: "enum", EnumValues: []string{"settle_reserved", "release"}, Description: "上游未返回 usage 时的结算策略。"},
				{Name: "quota.settlement.host_usage_wait", Type: "string", Description: "流式结算在上游未给 usage 时，关闭客户端流后等待宿主 usage.handle 的时长（0 关闭；非流式不等待）。"},
				{Name: "quota.stream.max_buffer_bytes", Type: "integer", Description: "流式结算本地缓冲上限。"},
				{Name: "quota.stream.stale_reservation_timeout", Type: "string", Description: "无心跳在途预占自动释放时长。"},
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
		{Method: "GET", Path: base + "/usage", Description: "请求明细"},
		{Method: "GET", Path: base + "/usage/summary", Description: "用量汇总"},
		{Method: "GET", Path: base + "/usage/dimension", Description: "维度分组"},
		{Method: "GET", Path: base + "/requests", Description: "请求明细（别名）"},
		{Method: "GET", Path: base + "/trends", Description: "趋势"},
		{Method: "GET", Path: base + "/costs", Description: "费用覆盖"},
		{Method: "GET", Path: base + "/balance", Description: "Key 余额"},
		{Method: "GET", Path: base + "/audit", Description: "审计事件"},
		{Method: "GET", Path: base + "/auth-quotas", Description: "OAuth 额度快照"},
		{Method: "GET", Path: base + "/preferences", Description: "面板偏好读取"},
		{Method: "POST", Path: base + "/preferences", Description: "面板偏好保存"},
		{Method: "GET", Path: base + "/exchange-rate", Description: "汇率读取"},
		{Method: "POST", Path: base + "/exchange-rate", Description: "刷新汇率"},
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
	plan, err := svc.BuildReservePlan(ctx, req.Model, request)
	if err != nil {
		if errors.Is(err, service.ErrModelDisabled) {
			return errorEnvelope("model_disabled", err.Error()), nil
		}
		return errorEnvelope("reserve_rejected", err.Error()), nil
	}
	reservation, err := svc.Reserve(ctx, service.ReservationRequest{KeyID: key.KID, CallerID: key.CallerID, Model: plan.Model, EstimatedTokens: plan.TokenEstimate, EstimatedImages: plan.ImageCount, Actor: "quota"})
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
	startedAt := time.Now()
	hostBody, headers, status, errHost := hostModelExecute(req.HostCallbackID, req, request, false)
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
	settleReservation(svc, reservation, req, request, startedAt, time.Time{}, completedAt, status, parsed, claim)
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
	plan, err := svc.BuildReservePlan(ctx, req.Model, request)
	if err != nil {
		return err
	}
	reservation, err := svc.Reserve(ctx, service.ReservationRequest{KeyID: key.KID, CallerID: key.CallerID, Model: plan.Model, EstimatedTokens: plan.TokenEstimate, EstimatedImages: plan.ImageCount, Actor: "quota"})
	if err != nil {
		return err
	}
	stopHeartbeat := startReservationHeartbeat(svc, reservation.ID)
	defer stopHeartbeat()

	// 登记认领：宿主随后的 usage.handle 由本次请求消费，不再被动入库。
	claim := registerUsageClaim(key.KID, plan.Model, req.Model)
	startedAt := time.Now()
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
		settleReservation(svc, reservation, req, request, startedAt, time.Time{}, time.Now(), stream.StatusCode, usageparse.Usage{}, claim)
		return fmt.Errorf("host model status %d", stream.StatusCode)
	}
	if strings.TrimSpace(stream.StreamID) == "" {
		claim.release(0)
		_, _ = svc.Release(ctx, reservation.ID)
		return errors.New("empty host stream id")
	}
	defer func() { _ = closeHostModelStream(stream.StreamID) }()

	acc := &usageparse.Accumulator{}
	maxBuffer := int(svc.Config().Quota.Stream.MaxBufferBytes)
	var buf []byte
	var firstChunkAt, completedAt time.Time
	for {
		chunkRaw, errRead := callHost("host.model.stream_read", rpcHostModelStreamReadRequest{StreamID: stream.StreamID})
		if errRead != nil {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, reservation, req, request, startedAt, firstChunkAt, completedAt, 502, parsed, claim)
			return errRead
		}
		var chunk rpcHostModelStreamReadResponse
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, reservation, req, request, startedAt, firstChunkAt, completedAt, 502, parsed, claim)
			return err
		}
		if chunk.Error != "" {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, reservation, req, request, startedAt, firstChunkAt, completedAt, 502, parsed, claim)
			return fmt.Errorf("%s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if len(buf) < maxBuffer {
				remain := minInt(maxBuffer-len(buf), len(chunk.Payload))
				buf = append(buf, chunk.Payload[:remain]...)
			}
			acc.FeedSSE(chunk.Payload)
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
			}
			if err := emitPluginStreamChunk(pluginStreamID, chunk.Payload); err != nil {
				completedAt = time.Now()
				parsed, _ := acc.Result()
				settleReservation(svc, reservation, req, request, startedAt, firstChunkAt, completedAt, 499, parsed, claim)
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
			r := buildRequest(svc, reservation, req, request, startedAt, firstChunkAt, completedAt, 200)
			applyHostUsageToRequest(r, rec)
			return finishSettle(svc, reservation, r, parsed, claim)
		}
	}
	return finishSettle(svc, reservation, buildRequest(svc, reservation, req, request, startedAt, firstChunkAt, completedAt, 200), parsed, claim)
}

// settleReservation 解析 usage 结算预占并写请求记录；结算失败时释放预占兜底。
func settleReservation(svc *service.Service, reservation store.Reservation, req rpcExecutorRequest, body []byte, startedAt, firstChunkAt, completedAt time.Time, status int, usage usageparse.Usage, claim *usageClaim) {
	r := buildRequest(svc, reservation, req, body, startedAt, firstChunkAt, completedAt, status)
	// 认领已在结算前收到宿主口径（少见但可能）：结算前补齐展示字段，
	// 使请求行与它的分钟聚合维度一致。
	if rec, ok := claim.wait(0); ok {
		applyHostUsageToRequest(r, rec)
		if usage.IsZero() {
			usage = usageFromRecord(rec)
		}
	}
	_ = finishSettle(svc, reservation, r, usage, claim)
}

// finishSettle 落库结算结果，并把请求行 ID 交给认领：
// 宽限期内晚到的宿主用量据此回填，无需再靠库内启发式判重。
func finishSettle(svc *service.Service, reservation store.Reservation, r *store.Request, usage usageparse.Usage, claim *usageClaim) error {
	ctx := context.Background()
	_, err := svc.Settle(ctx, reservation.ID, usage, r)
	if err != nil {
		// 未落库请求行：放弃认领，宿主回调回落到被动统计，避免用量凭空丢失。
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

func buildRequest(svc *service.Service, reservation store.Reservation, req rpcExecutorRequest, body []byte, startedAt, firstChunkAt, completedAt time.Time, status int) *store.Request {
	r := &store.Request{
		ID:                uuid.NewString(),
		TS:                startedAt,
		KeyID:             reservation.KeyID,
		CallerID:          reservation.CallerID,
		Model:             reservation.Model,
		Provider:          req.AuthProvider,
		Source:            req.SourceFormat,
		AuthID:            req.AuthID,
		AuthType:          req.AuthType,
		Tier:              requestString(body, "service_tier", "tier"),
		ThinkingIntensity: requestNestedString(body, []string{"reasoning", "effort"}, []string{"thinking", "effort"}, []string{"reasoning_effort"}),
		LatencyMS:         millisBetween(startedAt, completedAt),
		TTFTMS:            millisBetween(startedAt, firstChunkAt),
		GenerationMS:      millisBetween(firstChunkAt, completedAt),
		CostMicroUSD:      0,
		Priced:            true,
	}
	if status >= http.StatusBadRequest {
		r.Result = store.ResultError
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

func requestString(body []byte, keys ...string) string {
	var value map[string]any
	if len(body) == 0 || json.Unmarshal(body, &value) != nil {
		return ""
	}
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func requestNestedString(body []byte, paths ...[]string) string {
	var value any
	if len(body) == 0 || json.Unmarshal(body, &value) != nil {
		return ""
	}
	for _, path := range paths {
		current := value
		for _, key := range path {
			object, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = object[key]
		}
		if text, ok := current.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func requestBody(req rpcExecutorRequest) []byte {
	if len(req.OriginalRequest) > 0 {
		return req.OriginalRequest
	}
	return req.Payload
}

// requestBodyWithStreamUsage 为 OpenAI 系流式请求注入 stream_options.include_usage，
// 使终止 chunk 携带用量，保证流式结算准确。
func requestBodyWithStreamUsage(body []byte, sourceFormat, outputFormat string) []byte {
	format := strings.ToLower(service.FirstNonEmpty(outputFormat, sourceFormat))
	if strings.Contains(format, "claude") || strings.Contains(format, "gemini") {
		return body
	}
	var payload map[string]any
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return body
	}
	options, ok := payload["stream_options"].(map[string]any)
	if !ok {
		options = make(map[string]any)
		payload["stream_options"] = options
	}
	if _, exists := options["include_usage"]; exists {
		return body
	}
	options["include_usage"] = true
	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}

func startReservationHeartbeat(svc *service.Service, reservationID string) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_ = svc.TouchReservation(context.Background(), reservationID)
			}
		}
	}()
	return func() { close(done) }
}

func hostModelExecute(hostCallbackID string, req rpcExecutorRequest, body []byte, stream bool) ([]byte, http.Header, int, error) {
	raw, err := callHost("host.model.execute", rpcHostModelExecutionRequest{
		EntryProtocol:  service.FirstNonEmpty(req.SourceFormat, "openai"),
		ExitProtocol:   service.FirstNonEmpty(req.Format, req.SourceFormat, "openai"),
		Model:          strings.TrimSpace(req.Model),
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
	_, err := callHost("host.stream.emit", rpcStreamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func closePluginStream(streamID, errMsg string) {
	_, _ = callHost("host.stream.close", rpcStreamCloseRequest{StreamID: streamID, Error: strings.TrimSpace(errMsg)})
}

func closeHostModelStream(streamID string) error {
	if strings.TrimSpace(streamID) == "" {
		return nil
	}
	_, err := callHost("host.model.stream_close", rpcHostModelStreamCloseRequest{StreamID: streamID})
	return err
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
	imageHolds[requestID] = imageHold{
		reservation: reservation,
		plan:        plan,
		stopHeart:   startReservationHeartbeat(svc, reservation.ID),
		claim:       registerUsageClaim(key.KID, plan.Model, req.Model, req.RequestedModel),
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
	r := buildRequest(svc, hold.reservation, rpcExecutorRequest{}, nil, req.StartedAt, time.Time{}, req.CompletedAt, req.StatusCode)
	if rec, ok := hold.claim.wait(0); ok {
		applyHostUsageToRequest(r, rec)
	}
	_ = finishSettle(svc, hold.reservation, r, usageparse.Usage{ImageCount: hold.plan.ImageCount}, hold.claim)
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
	hr := httptest.NewRequest(req.Method, req.Path, bytes.NewReader(req.Body))
	for k, vals := range req.Headers {
		for _, v := range vals {
			hr.Header.Add(k, v)
		}
	}
	if len(req.Query) > 0 {
		hr.URL.RawQuery = req.Query.Encode()
	}
	rw := httptest.NewRecorder()
	api.Handler().ServeHTTP(rw, hr)
	return okEnvelope(rpcManagementResponse{StatusCode: rw.Code, Headers: rw.Header(), Body: rw.Body.Bytes()})
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
	models     map[string]bool
	keyID      string
	created    time.Time
	registered bool

	mu    sync.Mutex
	rec   *rpcUsageRecord
	reqID string
	ready chan struct{}
}

var (
	usageClaimsMu sync.Mutex
	usageClaims   []*usageClaim
)

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
	usageClaimsMu.Lock()
	kept := usageClaims[:0]
	for _, v := range usageClaims {
		if v.created.After(cutoff) {
			kept = append(kept, v)
		}
	}
	usageClaims = append(kept, c)
	usageClaimsMu.Unlock()
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
	usageClaimsMu.Lock()
	for i, v := range usageClaims {
		if v == c {
			usageClaims = append(usageClaims[:i], usageClaims[i+1:]...)
			break
		}
	}
	usageClaimsMu.Unlock()
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
// 调用方持有 usageClaimsMu，因此同一认领不会被并发交付两次。
func (c *usageClaim) attach(u rpcUsageRecord) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec != nil {
		return "", false
	}
	rec := u
	c.rec = &rec
	close(c.ready)
	return c.reqID, true
}

// available 报告认领是否仍可吸收回调，且模型名匹配。
func (c *usageClaim) available(keys []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec != nil {
		return false
	}
	for _, k := range keys {
		if k != "" && c.models[k] {
			return true
		}
	}
	return false
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
func claimHostUsage(u rpcUsageRecord) (string, bool) {
	keys := []string{normalizeModelKey(u.Model), normalizeModelKey(u.Alias)}
	if keys[0] == "" && keys[1] == "" {
		return "", false
	}
	kid, _ := service.ParseKeyID(u.APIKey)
	usageClaimsMu.Lock()
	defer usageClaimsMu.Unlock()
	var best *usageClaim
	for _, c := range usageClaims {
		if !c.available(keys) {
			continue
		}
		// 同模型并发时优先 kid 精确匹配，否则取最早登记的（FIFO）。
		if best == nil || (kid != "" && c.keyID == kid && best.keyID != kid) {
			best = c
		}
	}
	if best == nil {
		return "", false
	}
	return best.attach(u)
}

func resetUsageClaims() {
	usageClaimsMu.Lock()
	usageClaims = nil
	usageClaimsMu.Unlock()
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
		if id, dup, err := svc.FindDuplicateExecutor(ctx, models, u.RequestedAt, u.Latency.Milliseconds()); err == nil && dup {
			if hasDetail {
				if err := svc.BackfillRequestUsageByID(ctx, id, bf); err != nil {
					warnf("回填请求 %s 的宿主用量失败: %v", id, err)
				}
			}
			return okEnvelope(map[string]any{})
		}
	}
	req := usageRecordToRequest(st, u)
	if cost, _, err := svc.Price(req.Model, usageFromRecord(u)); err == nil {
		req.CostMicroUSD = cost
	}
	if err := st.RecordUsage(ctx, req); err != nil {
		// 用量记录失败不应让宿主把整次请求判为插件错误。
		warnf("被动记录用量失败: %v", err)
		return okEnvelope(map[string]any{})
	}
	// 落库后对账：与执行器结算行是同一请求时合并去重（消除双写竞态）。
	if _, err := st.ReconcileRequestDuplicates(ctx, req.ID); err != nil {
		warnf("请求 %s 落库对账失败: %v", req.ID, err)
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
		ID:                  uuid.NewString(),
		TS:                  u.RequestedAt,
		Model:               u.Model,
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var version = "0.0.1"

func main() { _ = runtime.GOOS }
