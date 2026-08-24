package httpapi

import (
	"net/http"
	"strconv"

	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// 模型路由（集合别名）管理端点。规则脚本的编译校验在 service 层
// （ValidateRouteRule），这里只做参数形态与落库。

func (a *API) modelRoutes(w http.ResponseWriter, r *http.Request) {
	items, err := a.svc.ListRoutesCompiled(r.Context())
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 500)
		return
	}
	judge, err := a.svc.GetJudgeSettings(r.Context())
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 500)
		return
	}
	noStore(w)
	jsonOut(w, map[string]any{"items": items, "judge": judge}, 200)
}

func (a *API) modelRouteSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID              int64  `json:"id"`
		Alias           string `json:"alias"`
		Rule            string `json:"rule"`
		CooldownSeconds int64  `json:"cooldown_seconds"`
		PricingMode     string `json:"pricing_mode"`
		Enabled         *bool  `json:"enabled"`
		Actor           string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if in.PricingMode != "target" && in.PricingMode != "alias" {
		jsonOut(w, map[string]string{"error": "pricing_mode 只能是 target 或 alias"}, 400)
		return
	}
	if in.CooldownSeconds < 0 || in.CooldownSeconds > 86400 {
		jsonOut(w, map[string]string{"error": "cooldown_seconds 须在 0~86400 之间"}, 400)
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	ctx := r.Context()
	refs, _, warning, verr := a.svc.ValidateRouteRule(ctx, in.ID, in.Alias, in.Rule, in.PricingMode)
	if verr != nil {
		jsonOut(w, map[string]string{"error": verr.Error()}, 400)
		return
	}
	rec := store.ModelRoute{ID: in.ID, Alias: in.Alias, Rule: in.Rule, CooldownSeconds: in.CooldownSeconds, PricingMode: in.PricingMode, Enabled: enabled, Refs: refs}
	var id int64
	var err error
	action := "route.save"
	if in.ID > 0 {
		err = a.st.UpdateModelRoute(ctx, in.ID, rec)
		id = in.ID
	} else {
		action = "route.create"
		id, err = a.st.InsertModelRoute(ctx, rec)
	}
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 400)
		return
	}
	_ = a.st.AppendAudit(ctx, store.AuditEvent{Actor: in.Actor, Action: action, EntityType: "model_route", EntityID: strconv.FormatInt(id, 10), Detail: map[string]any{"alias": in.Alias}})
	out := map[string]any{"ok": true, "id": id}
	if warning != "" {
		out["warning"] = warning
	}
	noStore(w)
	jsonOut(w, out, 200)
}

func (a *API) modelRouteDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    int64  `json:"id"`
		Actor string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if err := a.st.DeleteModelRoute(r.Context(), in.ID); err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 400)
		return
	}
	_ = a.st.AppendAudit(r.Context(), store.AuditEvent{Actor: in.Actor, Action: "route.delete", EntityType: "model_route", EntityID: strconv.FormatInt(in.ID, 10)})
	jsonOut(w, map[string]bool{"ok": true}, 200)
}

func (a *API) modelRouteJudge(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		judge, err := a.svc.GetJudgeSettings(r.Context())
		if err != nil {
			jsonOut(w, map[string]string{"error": err.Error()}, 500)
			return
		}
		noStore(w)
		jsonOut(w, judge, 200)
		return
	}
	var in service.JudgeSettings
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if e := a.svc.SaveJudgeSettings(r.Context(), in.Model, in.TimeoutMS); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	_ = a.st.AppendAudit(r.Context(), store.AuditEvent{Action: "route.judge.save", EntityType: "model_route", Detail: map[string]any{"model": in.Model, "timeout_ms": in.TimeoutMS}})
	judge, _ := a.svc.GetJudgeSettings(r.Context())
	jsonOut(w, judge, 200)
}
