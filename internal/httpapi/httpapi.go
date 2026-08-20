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
}

func New(svc *service.Service, st *store.Store, managementKey string) *API {
	a := &API{svc: svc, st: st, managementKey: managementKey, mux: http.NewServeMux()}
	a.register()
	return a
}

const base = "/v0/management/plugins/cpa-usage-manager"

func (a *API) register() {
	a.mux.HandleFunc("/console", a.console)
	a.mux.HandleFunc("/v0/resource/plugins/cpa-usage-manager/console", a.console)
	a.mux.HandleFunc(base+"/health", a.auth(a.health))
	a.mux.HandleFunc(base+"/overview", a.auth(a.overview))

	a.mux.HandleFunc(base+"/callers", a.auth(a.callers))
	a.mux.HandleFunc(base+"/callers/enabled", a.auth(a.callerEnabled))

	a.mux.HandleFunc(base+"/keys", a.auth(a.keys))
	a.mux.HandleFunc(base+"/keys/issue", a.auth(a.issue))
	a.mux.HandleFunc(base+"/keys/update", a.auth(a.updateKey))
	a.mux.HandleFunc(base+"/keys/rotate", a.auth(a.rotate))
	a.mux.HandleFunc(base+"/keys/reveal", a.auth(a.reveal))
	a.mux.HandleFunc(base+"/keys/revoke", a.auth(a.revoke))
	a.mux.HandleFunc(base+"/keys/delete", a.auth(a.deleteKey))

	a.mux.HandleFunc(base+"/pricing", a.auth(a.pricing))
	a.mux.HandleFunc(base+"/pricing/delete", a.auth(a.pricingDelete))
	a.mux.HandleFunc(base+"/pricing/sync", a.auth(a.pricingSync))

	a.mux.HandleFunc(base+"/usage", a.auth(a.usage))
	a.mux.HandleFunc(base+"/usage/summary", a.auth(a.usageSummary))
	a.mux.HandleFunc(base+"/usage/dimension", a.auth(a.usageDimension))
	a.mux.HandleFunc(base+"/requests", a.auth(a.usage))
	a.mux.HandleFunc(base+"/trends", a.auth(a.trends))
	a.mux.HandleFunc(base+"/costs", a.auth(a.costs))
	a.mux.HandleFunc(base+"/balance", a.auth(a.balance))

	a.mux.HandleFunc(base+"/audit", a.auth(a.audit))
	a.mux.HandleFunc(base+"/auth-quotas", a.auth(a.authQuotas))
	a.mux.HandleFunc(base+"/preferences", a.auth(a.preferences))
	a.mux.HandleFunc(base+"/exchange-rate", a.auth(a.exchangeRate))

	a.mux.HandleFunc(base+"/export/csv", a.auth(a.exportCSV))
	a.mux.HandleFunc(base+"/export/png", a.auth(a.exportPNG))
	a.mux.HandleFunc(base+"/backup", a.auth(a.backup))
	a.mux.HandleFunc(base+"/restore", a.auth(a.restore))
	a.mux.HandleFunc(base+"/reset", a.auth(a.reset))
	a.mux.HandleFunc(base+"/maintain", a.auth(a.maintain))
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
				m, e := money.ParseUSD(jsonStr(v))
				if e != nil {
					return u, fmt.Errorf("%s 金额格式非法：%w", c.key, e)
				}
				p = &m
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

func (a *API) usage(w http.ResponseWriter, r *http.Request) {
	f := service.UsageFilter{KeyID: r.URL.Query().Get("key_id"), Model: r.URL.Query().Get("model")}
	v, e := a.svc.ListRequests(r.Context(), f, atoi(r.URL.Query().Get("limit")), atoi(r.URL.Query().Get("offset")), r.URL.Query().Get("sort"), r.URL.Query().Get("order"))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) usageDimension(w http.ResponseWriter, r *http.Request) {
	f := service.UsageFilter{KeyID: r.URL.Query().Get("key_id"), Model: r.URL.Query().Get("model"), CallerID: r.URL.Query().Get("caller_id"), Provider: r.URL.Query().Get("provider"), Result: r.URL.Query().Get("result")}
	if from := service.ParseTime(r.URL.Query().Get("from")); !from.IsZero() {
		f.From = from
	}
	if to := service.ParseTime(r.URL.Query().Get("to")); !to.IsZero() {
		f.To = to
	}
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
	v, e := a.svc.Trends(r.Context(), service.UsageFilter{KeyID: r.URL.Query().Get("key_id")}, r.URL.Query().Get("grain"))
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	jsonOut(w, v, 200)
}
func (a *API) costs(w http.ResponseWriter, r *http.Request) {
	v, e := a.svc.Costs(r.Context(), service.UsageFilter{KeyID: r.URL.Query().Get("key_id")})
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

func atoi(s string) int { v, _ := strconv.Atoi(s); return v }
