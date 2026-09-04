package httpapi

import (
	"fmt"
	"strings"
	"testing"
)

func TestModelRouteRoutes(t *testing.T) {
	a := newTestAPI(t)

	// 初始空列表 + 默认 judge 设置。
	w := do(t, a, "GET", base+"/model-routes", "")
	var list struct {
		Items []struct {
			ID      int64    `json:"id"`
			Alias   string   `json:"alias"`
			Rule    string   `json:"rule"`
			Refs    []string `json:"refs"`
			Enabled bool     `json:"enabled"`
		} `json:"items"`
		Judge struct {
			Model     string `json:"model"`
			TimeoutMS int64  `json:"timeout_ms"`
		} `json:"judge"`
	}
	decodeJSON(t, w, &list)
	if len(list.Items) != 0 || list.Judge.Model != "" {
		t.Fatalf("初始状态异常: %+v %+v", list.Items, list.Judge)
	}

	// 语法错误带行列号。
	badRule := "-> priority [\"a\",\n-> \"b\""
	w = do(t, a, "POST", base+"/model-routes/save", fmt.Sprintf(`{"alias":"auto","rule":%q,"cooldown_seconds":30,"pricing_mode":"target"}`, badRule))
	if w.Code != 400 {
		t.Fatalf("坏规则应 400，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "第 2 行") {
		t.Fatalf("错误应带行列定位: %s", w.Body)
	}

	// 非法 pricing_mode / cooldown。
	w = do(t, a, "POST", base+"/model-routes/save", `{"alias":"auto","rule":"-> \"x\"","pricing_mode":"weird"}`)
	if w.Code != 400 {
		t.Fatalf("非法 pricing_mode 应 400: %d", w.Code)
	}
	w = do(t, a, "POST", base+"/model-routes/save", `{"alias":"auto","rule":"-> \"x\"","pricing_mode":"target","cooldown_seconds":99999}`)
	if w.Code != 400 {
		t.Fatalf("超界 cooldown 应 400: %d", w.Code)
	}

	// 正常创建（enabled 缺省 true），refs 回填。
	rule := "when input_tokens <= 8000\n  -> weighted { \"gpt-4o-mini\": 3, \"deepseek-chat\": 1 }\n-> priority [\"claude-sonnet-4\", \"gemini-2.5-pro\"]"
	body := fmt.Sprintf(`{"alias":"Auto","rule":%q,"cooldown_seconds":30,"pricing_mode":"target","actor":"t"}`, rule)
	w = do(t, a, "POST", base+"/model-routes/save", body)
	var saved struct {
		OK      bool   `json:"ok"`
		ID      int64  `json:"id"`
		Warning string `json:"warning"`
	}
	decodeJSON(t, w, &saved)
	if !saved.OK || saved.ID <= 0 || saved.Warning != "" {
		t.Fatalf("创建结果异常: %+v", saved)
	}

	w = do(t, a, "GET", base+"/model-routes", "")
	decodeJSON(t, w, &list)
	if len(list.Items) != 1 {
		t.Fatalf("应有一条路由: %+v", list.Items)
	}
	it := list.Items[0]
	if it.Alias != "Auto" || len(it.Refs) != 4 {
		t.Fatalf("refs 应为 4 个: %+v", it)
	}

	// NOCASE 重名拒绝。
	dup := fmt.Sprintf(`{"alias":"AUTO","rule":%q,"pricing_mode":"target"}`, rule)
	if w = do(t, a, "POST", base+"/model-routes/save", dup); w.Code != 400 {
		t.Fatalf("NOCASE 重名应 400: %d", w.Code)
	}

	// 更新（同 id）+ 停用。
	upd := fmt.Sprintf(`{"id":%d,"alias":"Auto","rule":%q,"cooldown_seconds":0,"pricing_mode":"alias","enabled":false,"actor":"t"}`, saved.ID, rule)
	var updOut struct {
		OK      bool   `json:"ok"`
		ID      int64  `json:"id"`
		Warning string `json:"warning"`
	}
	w = do(t, a, "POST", base+"/model-routes/save", upd)
	decodeJSON(t, w, &updOut)
	if !updOut.OK {
		t.Fatalf("更新失败: %s", w.Body)
	}
	w = do(t, a, "GET", base+"/model-routes", "")
	decodeJSON(t, w, &list)
	if list.Items[0].Enabled || list.Items[0].ID != saved.ID {
		t.Fatalf("停用更新未生效: %+v", list.Items[0])
	}

	// ai_judge 规则在未配评判模型时保存期拒绝。
	aiRule := "when ai_judge([\"simple\",\"hard\"]) == \"hard\"\n  -> \"opus\"\n-> \"mini\""
	if w = do(t, a, "POST", base+"/model-routes/save", fmt.Sprintf(`{"alias":"smart","rule":%q,"pricing_mode":"target"}`, aiRule)); w.Code != 400 {
		t.Fatalf("未配评判模型的 ai_judge 规则应 400: %d", w.Code)
	}

	// judge 设置保存后同一规则可保存；judge 端点回读一致。
	if w = do(t, a, "POST", base+"/model-routes/judge", `{"model":"judge-x","timeout_ms":4000}`); w.Code != 200 {
		t.Fatalf("judge 保存失败: %d", w.Code)
	}
	w = do(t, a, "GET", base+"/model-routes/judge", "")
	var judge struct {
		Model     string `json:"model"`
		TimeoutMS int64  `json:"timeout_ms"`
	}
	decodeJSON(t, w, &judge)
	if judge.Model != "judge-x" || judge.TimeoutMS != 4000 {
		t.Fatalf("judge 设置不一致: %+v", judge)
	}
	w = do(t, a, "POST", base+"/model-routes/judge", `{"model":"j","timeout_ms":100}`)
	if w.Code != 400 {
		t.Fatalf("judge 超时下限校验缺失: %d", w.Code)
	}

	// 删除。
	w = do(t, a, "POST", base+"/model-routes/delete", fmt.Sprintf(`{"id":%d,"actor":"t"}`, saved.ID))
	var del struct {
		OK bool `json:"ok"`
	}
	decodeJSON(t, w, &del)
	if !del.OK {
		t.Fatal("删除失败")
	}
	if w = do(t, a, "POST", base+"/model-routes/delete", `{"id":999}`); w.Code != 400 {
		t.Fatalf("删除不存在路由应 400: %d", w.Code)
	}
}

// TestModelRouteEntityDecode 验证保存期 HTML 实体还原：被中间层转义的规则
// 落库为原文、可正常编译，回显不再带实体。
func TestModelRouteEntityDecode(t *testing.T) {
	a := newTestAPI(t)
	if w := do(t, a, "POST", base+"/model-routes/judge", `{"model":"judge-x","timeout_ms":4000}`); w.Code != 200 {
		t.Fatalf("judge 设置失败: %d", w.Code)
	}
	escaped := `when ai_judge([&#34;simple&#34;, &#34;hard&#34;]) == &#34;hard&#34;
  -&gt; &#34;openrouter/ox-alpha&#34;
-&gt; &#34;orcarouter/deepseek-v4-flash&#34;`
	w := do(t, a, "POST", base+"/model-routes/save",
		fmt.Sprintf(`{"alias":"esc","rule":%q,"cooldown_seconds":30,"pricing_mode":"target"}`, escaped))
	var saved struct {
		OK bool `json:"ok"`
	}
	decodeJSON(t, w, &saved)
	if !saved.OK {
		t.Fatalf("转义规则经还原后应可保存: %s", w.Body)
	}
	w = do(t, a, "GET", base+"/model-routes", "")
	var list struct {
		Items []struct {
			Alias string   `json:"alias"`
			Rule  string   `json:"rule"`
			Refs  []string `json:"refs"`
		} `json:"items"`
	}
	decodeJSON(t, w, &list)
	if len(list.Items) != 1 {
		t.Fatalf("应有 1 条路由: %+v", list.Items)
	}
	want := "when ai_judge([\"simple\", \"hard\"]) == \"hard\"\n  -> \"openrouter/ox-alpha\"\n-> \"orcarouter/deepseek-v4-flash\""
	if list.Items[0].Rule != want {
		t.Fatalf("落库应为原文: %q", list.Items[0].Rule)
	}
	if len(list.Items[0].Refs) != 2 || list.Items[0].Refs[0] != "openrouter/ox-alpha" {
		t.Fatalf("refs 应含斜杠目标: %v", list.Items[0].Refs)
	}
}

// TestModelRouteTestEndpoint 干跑端点：查询语义，编译/求值错误随 200 走
// error 字段；规则经实体还原后参与编译；结果回传候选链与变量快照。
func TestModelRouteTestEndpoint(t *testing.T) {
	a := newTestAPI(t)

	w := do(t, a, "POST", base+"/model-routes/test", `{"alias":"auto","rule":"when"}`)
	if w.Code != 200 {
		t.Fatalf("干跑坏规则应 200，得到 %d", w.Code)
	}
	var bad struct {
		Error string `json:"error"`
		Vars  map[string]any
	}
	decodeJSON(t, w, &bad)
	if !strings.Contains(bad.Error, "第 1 行") || bad.Vars == nil {
		t.Fatalf("错误应带定位且 vars 恒在: %+v", bad)
	}

	w = do(t, a, "POST", base+"/model-routes/test",
		`{"id":0,"alias":"auto","rule":"-&gt; priority [&quot;a&quot;,&quot;b&quot;]","model":"AUTO-HIGH","source":"gemini","prompt":"hi","run_ai":false}`)
	var out struct {
		Chain   []string `json:"chain"`
		Skipped []struct {
			Target string `json:"target"`
		} `json:"skipped"`
		FellBack  bool           `json:"fell_back"`
		AISkipped bool           `json:"ai_skipped"`
		Vars      map[string]any `json:"vars"`
		Error     string         `json:"error"`
	}
	decodeJSON(t, w, &out)
	if out.Error != "" || len(out.Chain) != 2 || out.Chain[0] != "a" || out.Chain[1] != "b" {
		t.Fatalf("干跑应还原实体并回传整链: %+v", out)
	}
	if out.AISkipped || out.FellBack || len(out.Skipped) != 0 {
		t.Fatalf("简单干跑不应有 AI/冷却痕迹: %+v", out)
	}
	if out.Vars["model"] != "AUTO" || out.Vars["source"] != "gemini" {
		t.Fatalf("变量快照错误: %v", out.Vars)
	}

	// 非法 JSON 体 → 400。
	if w = do(t, a, "POST", base+"/model-routes/test", `{`); w.Code != 400 {
		t.Fatalf("畸形请求体应 400: %d", w.Code)
	}
}
