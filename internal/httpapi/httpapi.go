package httpapi

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/web"
)

type API struct {
	svc           *service.Service
	st            *store.Store
	managementKey string
	mux           *http.ServeMux
	paths         []string // base 之下已注册的管理路径后缀，供 Paths 比对宿主声明表
}

func New(svc *service.Service, st *store.Store, managementKey string) *API {
	a := &API{svc: svc, st: st, managementKey: managementKey, mux: http.NewServeMux()}
	a.register()
	return a
}

const base = "/v0/management/plugins/cpa-usage-manager"

// route 注册一条管理路径（相对 base）并登记到 paths。
// 宿主只会转发它在插件注册时声明过的路径，漏声明的路径在面板上表现为 404
// （v0.1.2 的 /pricing/search 就是这样丢掉的）；Paths 让 main.go 的声明表可被测试比对。
func (a *API) route(suffix string, h http.HandlerFunc) {
	a.paths = append(a.paths, suffix)
	a.mux.HandleFunc(base+suffix, a.auth(h))
}

// Paths 返回本包在 base 之下注册的全部管理路径后缀。
func (a *API) Paths() []string { return append([]string(nil), a.paths...) }

func (a *API) register() {
	a.mux.HandleFunc("/console", a.console)
	a.mux.HandleFunc("/v0/resource/plugins/cpa-usage-manager/console", a.console)
	a.route("/health", a.health)
	a.route("/overview", a.overview)

	a.route("/callers", a.callers)
	a.route("/callers/enabled", a.callerEnabled)

	a.route("/keys", a.keys)
	a.route("/keys/issue", a.issue)
	a.route("/keys/update", a.updateKey)
	a.route("/keys/rotate", a.rotate)
	a.route("/keys/reveal", a.reveal)
	a.route("/keys/revoke", a.revoke)
	a.route("/keys/delete", a.deleteKey)

	a.route("/pricing", a.pricing)
	a.route("/pricing/delete", a.pricingDelete)
	a.route("/pricing/search", a.pricingSearch)
	a.route("/pricing/reset", a.pricingReset)
	a.route("/pricing/sync", a.pricingSync)

	a.route("/usage", a.usage)
	a.route("/usage/summary", a.usageSummary)
	a.route("/usage/dimension", a.usageDimension)
	a.route("/requests", a.usage)
	a.route("/trends", a.trends)
	a.route("/costs", a.costs)
	a.route("/balance", a.balance)

	a.route("/audit", a.audit)
	a.route("/auth-quotas", a.authQuotas)
	a.route("/preferences", a.preferences)
	a.route("/exchange-rate", a.exchangeRate)

	a.route("/export/csv", a.exportCSV)
	a.route("/export/png", a.exportPNG)
	a.route("/backup", a.backup)
	a.route("/restore", a.restore)
	a.route("/reset", a.reset)
	a.route("/maintain", a.maintain)
	a.route("/dedupe", a.dedupe)
}
func (a *API) Handler() http.Handler { return a.gzip(a.mux) }
func (a *API) console(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(web.ConsoleHTML())
}
func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.managementKey != "" && r.Header.Get("Authorization") != "Bearer "+a.managementKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
func (a *API) gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || isBinaryPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		gw := gzip.NewWriter(w)
		defer gw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gw}, r)
	})
}

// isBinaryPath 报告路径是否属于二进制下载类接口，gzip 中介件应放行原样输出。
func isBinaryPath(p string) bool {
	return strings.HasSuffix(p, "/backup") || strings.HasSuffix(p, "/restore") ||
		strings.HasSuffix(p, "/export/csv") || strings.HasSuffix(p, "/export/png")
}

type gzipResponseWriter struct {
	http.ResponseWriter
	Writer *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) { return w.Writer.Write(b) }
func jsonOut(w http.ResponseWriter, v any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}
func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	stats, e := a.st.Stats(r.Context())
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	jsonOut(w, map[string]any{"ok": true, "stats": stats}, 200)
}
func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	stats, e := a.st.Stats(r.Context())
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	sum, e := a.svc.UsageSummary(r.Context(), service.UsageFilter{})
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	rate := a.svc.ExchangeRate(r.Context())
	jsonOut(w, map[string]any{"stats": stats, "usage": sum, "exchange_rate": rate}, 200)
}

// ---------- callers ----------

func (a *API) callers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		v, e := a.svc.ListCallers(r.Context())
		if e != nil {
			jsonOut(w, map[string]string{"error": e.Error()}, 500)
			return
		}
		jsonOut(w, map[string]any{"items": v}, 200)
	case "POST":
		var in struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Actor       string `json:"actor"`
		}
		if e := decode(r, &in); e != nil {
			jsonOut(w, map[string]string{"error": e.Error()}, 400)
			return
		}
		v, e := a.svc.UpsertCaller(r.Context(), in.ID, in.DisplayName, in.Actor)
		if e != nil {
			jsonOut(w, map[string]string{"error": e.Error()}, 400)
			return
		}
		jsonOut(w, v, 200)
	default:
		w.WriteHeader(405)
	}
}
func (a *API) callerEnabled(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
		Actor   string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if e := a.svc.SetCallerEnabled(r.Context(), in.ID, in.Enabled, in.Actor); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, map[string]bool{"ok": true}, 200)
}

// ---------- keys ----------

func (a *API) keys(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	items, total, e := a.st.ListKeys(r.Context(), store.KeyFilter{CallerID: r.URL.Query().Get("caller_id"), Search: r.URL.Query().Get("search"), Limit: atoi(r.URL.Query().Get("limit")), Offset: atoi(r.URL.Query().Get("offset"))})
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	jsonOut(w, map[string]any{"items": items, "total": total}, 200)
}
func (a *API) issue(w http.ResponseWriter, r *http.Request) {
	var in service.IssueRequest
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	out, e := a.svc.IssueKey(r.Context(), in)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	noStore(w)
	jsonOut(w, out, 200)
}

// updateKey 部分更新 Key 的策略字段。显式 null 表示清空对应字段。
func (a *API) updateKey(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if e := decode(r, &raw); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	kid := jsonStr(raw["kid"])
	actor := jsonStr(raw["actor"])
	if kid == "" {
		jsonOut(w, map[string]string{"error": "缺少 kid"}, 400)
		return
	}
	u, e := buildKeyUpdate(raw)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	out, e := a.svc.UpdateKey(r.Context(), kid, u, actor)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, out, 200)
}

func buildKeyUpdate(raw map[string]json.RawMessage) (store.KeyUpdate, error) {
	var u store.KeyUpdate
	if v, ok := raw["label"]; ok && string(v) != "null" {
		s := jsonStr(v)
		u.Label = &s
	}
	if v, ok := raw["principal"]; ok && string(v) != "null" {
		s := jsonStr(v)
		u.Principal = &s
	}
	if v, ok := raw["enabled"]; ok && string(v) != "null" {
		b := jsonBool(v)
		u.Enabled = &b
	}
	if v, ok := raw["caller_id"]; ok && string(v) != "null" {
		s := jsonStr(v)
		u.CallerID = &s
	}
	if v, ok := raw["caller_scope"]; ok && string(v) != "null" {
		s := jsonStr(v)
		u.CallerScope = &s
	}
	// expires_at：null → 清空；空串 → 清空；否则 RFC3339 解析。
	if v, ok := raw["expires_at"]; ok {
		p := (*time.Time)(nil)
		if string(v) != "null" && string(v) != `""` {
			t, e := time.Parse(time.RFC3339, jsonStr(v))
			if e != nil {
				return u, fmt.Errorf("expires_at 须为 RFC3339 或 null：%w", e)
			}
			p = &t
		}
		u.ExpiresAt = &p
	}
	for _, c := range []struct {
		key string
		dst ***money.Micro
	}{
		{"quota_micro_usd", &u.QuotaMicroUSD},
		{"daily_micro_usd", &u.DailyMicroUSD},
		{"weekly_micro_usd", &u.WeeklyMicroUSD},
		{"monthly_micro_usd", &u.MonthlyMicroUSD},
	} {
		if v, ok := raw[c.key]; ok {
			p := (*money.Micro)(nil)
			if string(v) != "null" {
				// 口径与签发路径一致：*_micro_usd 是**整数 micro-USD**，不是美元字符串。
				//
				// 既有缺陷：此处原为 money.ParseUSD(jsonStr(v))，把该字段当成美元
				// 字符串（"$1.50"）解析。而签发路径直接 json.Unmarshal 到 money.Micro
				// （整数 micro），面板也发裸整数 —— jsonStr 对裸数字返回空串，
				// ParseUSD 随即报「金额格式非法」，导致**金额限额编辑从未成功过**。
				// 两条路径必须同口径，否则同一个字段名在签发与更新时含义差 1e6 倍。
				n, e := jsonInt64(v)
				if e != nil {
					return u, fmt.Errorf("%s 必须是整数 micro-USD：%w", c.key, e)
				}
				if n < 0 {
					return u, fmt.Errorf("%s 不能为负", c.key)
				}
				m := money.Micro(n)
				p = &m
			}
			*c.dst = &p
		}
	}
	// Token 限额：与金额同样的三态语义（缺省=不改，null=清空，数字=设值）
	for _, c := range []struct {
		key string
		dst ***int64
	}{
		{"token_limit", &u.TokenLimit},
		{"daily_token_limit", &u.DailyTokenLimit},
		{"weekly_token_limit", &u.WeeklyTokenLimit},
		{"monthly_token_limit", &u.MonthlyTokenLimit},
	} {
		if v, ok := raw[c.key]; ok {
			p := (*int64)(nil)
			if string(v) != "null" {
				// 值可能是 JSON 数字（500000）或字符串（"500000"）。jsonStr 只认
				// 字符串，对裸数字返回空串 —— 必须先按数字解，再回退按字符串解。
				n, e := jsonInt64(v)
				if e != nil {
					return u, fmt.Errorf("%s 必须是整数", c.key)
				}
				if n < 0 {
					return u, fmt.Errorf("%s 不能为负", c.key)
				}
				p = &n
			}
			*c.dst = &p
		}
	}
	if v, ok := raw["max_concurrent_requests"]; ok && string(v) != "null" {
		n, e := strconv.Atoi(jsonStr(v))
		if e != nil {
			return u, fmt.Errorf("max_concurrent_requests 必须是整数")
		}
		u.MaxConcurrentRequests = &n
	}
	if v, ok := raw["allowed_models"]; ok {
		u.AllowedModels = &[]string{}
		if string(v) != "null" {
			var m []string
			if e := json.Unmarshal(v, &m); e != nil {
				return u, fmt.Errorf("allowed_models 必须是字符串数组")
			}
			u.AllowedModels = &m
		}
	}
	return u, nil
}

func jsonStr(v json.RawMessage) string {
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

// jsonInt64 解析一个可能是 JSON 数字或数字字符串的值。
//
// 面板发的是裸数字（token_limit: 500000），但历史上金额字段走的是字符串口径，
// 两种都得接。只用 jsonStr 会把裸数字读成空串（json.Unmarshal 到 string 失败
// 但错误被忽略），进而报「必须是整数」——一个只在数字入参时触发的伪校验失败。
func jsonInt64(v json.RawMessage) (int64, error) {
	var n int64
	if err := json.Unmarshal(v, &n); err == nil {
		return n, nil
	}
	// 回退：带引号的数字，或带千分位/单位的写法（前端已归一，这里只兜底纯数字）
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return 0, fmt.Errorf("既非数字也非字符串: %s", string(v))
	}
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}
func jsonBool(v json.RawMessage) bool {
	var b bool
	_ = json.Unmarshal(v, &b)
	return b
}
func (a *API) rotate(w http.ResponseWriter, r *http.Request) {
	var in struct{ KID, Actor string }
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	out, e := a.svc.RotateKey(r.Context(), in.KID, in.Actor)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	noStore(w)
	jsonOut(w, out, 200)
}
func (a *API) reveal(w http.ResponseWriter, r *http.Request) {
	var in struct{ KID, Actor string }
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	out, e := a.svc.RevealKey(r.Context(), in.KID, in.Actor)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	noStore(w)
	jsonOut(w, map[string]string{"key": out}, 200)
}
func (a *API) revoke(w http.ResponseWriter, r *http.Request) {
	var in struct{ KID, Actor string }
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	e := a.svc.RevokeKey(r.Context(), in.KID, in.Actor)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, map[string]bool{"ok": true}, 200)
}
func (a *API) deleteKey(w http.ResponseWriter, r *http.Request) {
	var in struct{ KID, Actor string }
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	e := a.svc.DeleteKey(r.Context(), in.KID, in.Actor)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, map[string]bool{"ok": true}, 200)
}

// ---------- pricing ----------

func (a *API) pricing(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, e := a.st.ListPricingRules(r.Context(), false)
		if e != nil {
			jsonOut(w, map[string]string{"error": e.Error()}, 500)
			return
		}
		jsonOut(w, map[string]any{"items": v}, 200)
		return
	}
	var in store.PricingRule
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	v, e := a.st.UpsertPricingRule(r.Context(), in)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) pricingDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    int64  `json:"id"`
		Actor string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if e := a.st.DeletePricingRule(r.Context(), in.ID); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	_ = a.st.AppendAudit(r.Context(), store.AuditEvent{Actor: in.Actor, Action: "pricing.delete", EntityType: "pricing", EntityID: strconv.FormatInt(in.ID, 10)})
	jsonOut(w, map[string]bool{"ok": true}, 200)
}
func (a *API) pricingSearch(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	v, e := a.svc.SearchModelsDev(r.Context(), nil, r.URL.Query().Get("q"), limit)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 502)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) pricingReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Actor string `json:"actor"`
	}
	_ = decode(r, &in)
	n, e := a.st.ResetPricingRules(r.Context())
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	noStore(w)
	_ = a.st.AppendAudit(r.Context(), store.AuditEvent{Actor: in.Actor, Action: "pricing.reset", EntityType: "pricing", EntityID: strconv.FormatInt(n, 10)})
	jsonOut(w, map[string]any{"ok": true, "deleted": n}, 200)
}
func (a *API) pricingSync(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Actor string `json:"actor"`
		URL   string `json:"url"`
	}
	_ = decode(r, &in)
	res, e := a.svc.SyncModelsDev(r.Context(), service.NewModelsDevSyncer(in.URL, nil), in.Actor)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, res, 200)
}

// ---------- usage / analytics ----------

// parseFilter 把 URL query 解析成统一筛选条件，语义与导出接口的
// QueryFilter 完全一致（时间接受 RFC3339 / 日期 / Unix 毫秒）。
func parseFilter(r *http.Request) service.UsageFilter {
	q := r.URL.Query()
	f := service.UsageFilter{
		KeyID:    q.Get("key_id"),
		CallerID: q.Get("caller_id"),
		Model:    q.Get("model"),
		Provider: q.Get("provider"),
		Result:   q.Get("result"),
	}
	if from := service.ParseTime(q.Get("from")); !from.IsZero() {
		f.From = from
	}
	if to := service.ParseTime(q.Get("to")); !to.IsZero() {
		f.To = to
	}
	return f
}

func (a *API) usage(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	v, e := a.svc.ListRequests(r.Context(), f, atoi(r.URL.Query().Get("limit")), atoi(r.URL.Query().Get("offset")), r.URL.Query().Get("sort"), r.URL.Query().Get("order"))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) usageDimension(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	v, e := a.svc.GroupByDimension(r.Context(), f, r.URL.Query().Get("dimension"), atoi(r.URL.Query().Get("limit")))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) usageSummary(w http.ResponseWriter, r *http.Request) {
	v, e := a.svc.Summary(r.Context(), service.UsageFilter{KeyID: r.URL.Query().Get("key_id"), Model: r.URL.Query().Get("model")}, time.Now())
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) trends(w http.ResponseWriter, r *http.Request) {
	v, e := a.svc.Trends(r.Context(), parseFilter(r), r.URL.Query().Get("grain"))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) costs(w http.ResponseWriter, r *http.Request) {
	v, e := a.svc.Costs(r.Context(), parseFilter(r))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) balance(w http.ResponseWriter, r *http.Request) {
	v, e := a.svc.Balance(r.Context(), r.URL.Query().Get("key_id"), time.Time{})
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, v, 200)
}

// ---------- audit / auth-quotas / preferences / fx ----------

func (a *API) audit(w http.ResponseWriter, r *http.Request) {
	v, e := a.st.ListAudit(r.Context(), atoi(r.URL.Query().Get("limit")), atoi(r.URL.Query().Get("offset")))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) authQuotas(w http.ResponseWriter, r *http.Request) {
	v, e := a.svc.AuthQuotas(r.Context())
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	noStore(w)
	jsonOut(w, map[string]any{"items": v}, 200)
}
func (a *API) preferences(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		v, e := a.svc.Preferences(r.Context())
		if e != nil {
			jsonOut(w, map[string]string{"error": e.Error()}, 500)
			return
		}
		jsonOut(w, v, 200)
	case "POST":
		var kv map[string]string
		if e := decode(r, &kv); e != nil {
			jsonOut(w, map[string]string{"error": e.Error()}, 400)
			return
		}
		if e := a.svc.SavePreferences(r.Context(), kv); e != nil {
			jsonOut(w, map[string]string{"error": e.Error()}, 400)
			return
		}
		jsonOut(w, map[string]bool{"ok": true}, 200)
	default:
		w.WriteHeader(405)
	}
}
func (a *API) exchangeRate(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		v, e := a.svc.RefreshExchangeRate(r.Context())
		if e != nil {
			jsonOut(w, map[string]string{"error": e.Error()}, 502)
			return
		}
		jsonOut(w, v, 200)
		return
	}
	jsonOut(w, a.svc.ExchangeRate(r.Context()), 200)
}

// ---------- export / backup / restore / reset / maintain ----------

func (a *API) exportCSV(w http.ResponseWriter, r *http.Request) {
	var in service.ExportRequest
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	noStore(w)
	name, e := a.svc.ExportCSV(r.Context(), w, in)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
}
func (a *API) exportPNG(w http.ResponseWriter, r *http.Request) {
	var in service.ExportRequest
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	noStore(w)
	name, e := a.svc.ExportPNG(r.Context(), w, in)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
}
func (a *API) backup(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="cpa-usage-manager-backup.db"`)
	res, e := a.svc.Backup(r.Context(), w, r.URL.Query().Get("actor"))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	// 头部补充备份元数据，方便脚本校验。
	w.Header().Set("X-Backup-Bytes", strconv.FormatInt(res.Bytes, 10))
}
func (a *API) restore(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Confirm-Restore") != "replace" {
		jsonOut(w, map[string]string{"error": "缺少 X-Confirm-Restore: replace 确认头"}, 400)
		return
	}
	noStore(w)
	res, e := a.svc.Restore(r.Context(), io.LimitReader(r.Body, service.BackupMaxBytes+1), r.URL.Query().Get("actor"))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, res, 200)
}
func (a *API) reset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm      string `json:"confirm"`
		Requests     *bool  `json:"requests"`
		Reservations *bool  `json:"reservations"`
		KeyCounters  *bool  `json:"key_counters"`
		Audit        *bool  `json:"audit"`
		Actor        string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if in.Confirm != "reset" {
		jsonOut(w, map[string]string{"error": "需要 {\"confirm\":\"reset\"} 确认"}, 400)
		return
	}
	opts := store.AllStatistics()
	if in.Requests != nil {
		opts.Requests = *in.Requests
	}
	if in.Reservations != nil {
		opts.Reservations = *in.Reservations
	}
	if in.KeyCounters != nil {
		opts.KeyCounters = *in.KeyCounters
	}
	if in.Audit != nil {
		opts.Audit = *in.Audit
	}
	noStore(w)
	res, e := a.svc.Reset(r.Context(), opts, in.Actor)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, res, 200)
}
func (a *API) maintain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Vacuum bool   `json:"vacuum"`
		Actor  string `json:"actor"`
	}
	_ = decode(r, &in)
	res, e := a.svc.Maintain(r.Context(), in.Vacuum, in.Actor)
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, res, 200)
}

// dedupe 手动触发历史重复请求行对账。days<=0 时按保留期回溯。
func (a *API) dedupe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Days  int    `json:"days"`
		Actor string `json:"actor"`
	}
	_ = decode(r, &in)
	var since time.Time
	if in.Days > 0 {
		since = time.Now().UTC().AddDate(0, 0, -in.Days)
	}
	n, e := a.svc.Dedupe(r.Context(), since, in.Actor)
	if e != nil {
		jsonOut(w, map[string]any{"error": e.Error(), "merged": n}, 400)
		return
	}
	jsonOut(w, map[string]any{"merged": n}, 200)
}

func atoi(s string) int { v, _ := strconv.Atoi(s); return v }
