package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// 模型路由（集合别名）管理端点。规则脚本的编译校验在 service 层
// （ValidateRouteRule），这里只做参数形态与落库。

// decodeHTMLEntities 还原上游链路可能注入的 HTML 实体。规则语言的合法词表
// 不含这些序列（& 仅以 && 运算符出现），而面板到插件的某一层一旦把引号/
// 尖括号转义进存盘数据，规则将永久无法通过编译——保存期统一还原一次。
// strings.NewReplacer 单趟匹配：&amp;lt; 只还原为 &lt;，不会二次展开。
func decodeHTMLEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", "\"",
		"&#34;", "\"",
		"&#38;", "&",
		"&#39;", "'",
		"&apos;", "'",
	).Replace(s)
}

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
		CooldownPolicy  string `json:"cooldown_policy"`
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
	if in.CooldownPolicy == "" {
		in.CooldownPolicy = "block"
	}
	if in.CooldownPolicy != "block" && in.CooldownPolicy != "force" {
		jsonOut(w, map[string]string{"error": "cooldown_policy 只能是 block 或 force"}, 400)
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
	in.Alias = decodeHTMLEntities(in.Alias)
	in.Rule = decodeHTMLEntities(in.Rule)
	ctx := r.Context()
	refs, _, warning, verr := a.svc.ValidateRouteRule(ctx, in.ID, in.Alias, in.Rule, in.PricingMode)
	if verr != nil {
		jsonOut(w, map[string]string{"error": verr.Error()}, 400)
		return
	}
	rec := store.ModelRoute{ID: in.ID, Alias: in.Alias, Rule: in.Rule, CooldownSeconds: in.CooldownSeconds, CooldownPolicy: in.CooldownPolicy, PricingMode: in.PricingMode, Enabled: enabled, Refs: refs}
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

// modelRouteTest 干跑一条规则脚本：只求值候选链，不请求目标模型。
// 与 save 同样对 alias/rule 做实体还原，保证编辑器里未保存的草稿按
// 存盘后的形态参与编译。
func (a *API) modelRouteTest(w http.ResponseWriter, r *http.Request) {
	var in service.TestRouteRequest
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	in.Alias = decodeHTMLEntities(in.Alias)
	in.Rule = decodeHTMLEntities(in.Rule)
	noStore(w)
	jsonOut(w, a.svc.TestRoute(r.Context(), in), 200)
}

// GET /model-routes/health：启用路由的目标健康快照——进程内冷却状态 +
// 最近 60 分钟失败统计。回答「路由为什么绕开了 X」。
func (a *API) modelRoutesHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	items, e := a.svc.RoutesHealth(r.Context())
	if e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 500)
		return
	}
	jsonOut(w, map[string]any{"items": items}, 200)
}
