package httpapi

import (
	"net/http"

	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// GET /notify：通知设置与全部端点（URL 解密回显，面板是管理端唯一入口）。
func (a *API) notify(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	settings, err := a.svc.GetNotifySettings(r.Context())
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 500)
		return
	}
	endpoints, err := a.svc.ListNotifyEndpoints(r.Context())
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 500)
		return
	}
	if endpoints == nil {
		endpoints = []store.NotifyEndpoint{}
	}
	jsonOut(w, map[string]any{"settings": settings, "endpoints": endpoints}, 200)
}

// POST /notify/settings：保存全局设置。
func (a *API) notifySettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled              bool   `json:"enabled"`
		WarnPct              int    `json:"warn_pct"`
		ErrorAlerts          bool   `json:"error_alerts"`
		SingleCostAlert      bool   `json:"single_cost_alert"`
		SingleCostMicroUSD   int64  `json:"single_cost_micro_usd"`
		SingleTokenThreshold int64  `json:"single_token_threshold"`
		Actor                string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if err := a.svc.SaveNotifySettings(r.Context(), service.NotifySettings{
		Enabled: in.Enabled, WarnPct: in.WarnPct, ErrorAlerts: in.ErrorAlerts,
		SingleCostAlert: in.SingleCostAlert, SingleCostMicroUSD: in.SingleCostMicroUSD,
		SingleTokenThreshold: in.SingleTokenThreshold,
	}, in.Actor); err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 400)
		return
	}
	out, err := a.svc.GetNotifySettings(r.Context())
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 500)
		return
	}
	jsonOut(w, out, 200)
}

// POST /notify/endpoint/save：新增（id 缺省/0）或更新端点。
func (a *API) notifyEndpointSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      int64  `json:"id"`
		Label   string `json:"label"`
		URL     string `json:"url"`
		Enabled bool   `json:"enabled"`
		Actor   string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	id, err := a.svc.SaveNotifyEndpoint(r.Context(), in.ID, in.Label, in.URL, in.Enabled, in.Actor)
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 400)
		return
	}
	jsonOut(w, map[string]int64{"id": id}, 200)
}

// POST /notify/endpoint/delete。
func (a *API) notifyEndpointDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    int64  `json:"id"`
		Actor string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if in.ID <= 0 {
		jsonOut(w, map[string]string{"error": "缺少 id"}, 400)
		return
	}
	if err := a.svc.DeleteNotifyEndpoint(r.Context(), in.ID, in.Actor); err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 404)
		return
	}
	jsonOut(w, map[string]bool{"ok": true}, 200)
}

// POST /notify/endpoint/test：发送测试消息。id>0 测已存端点，否则测 draft URL。
func (a *API) notifyEndpointTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    int64  `json:"id"`
		URL   string `json:"url"`
		Actor string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if err := a.svc.TestNotifyEndpoint(r.Context(), in.ID, in.URL, in.Actor); err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 400)
		return
	}
	jsonOut(w, map[string]bool{"ok": true}, 200)
}
