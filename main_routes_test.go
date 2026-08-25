//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func TestRouteFailureEligible(t *testing.T) {
	cases := []struct {
		name   string
		status int
		text   string
		err    error
		want   bool
	}{
		{"429 可转", 429, "", nil, true},
		{"401 可转", 401, "", nil, true},
		{"403 可转", 403, "", nil, true},
		{"408 可转", 408, "", nil, true},
		{"500 可转", 500, "", nil, true},
		{"599 可转", 599, "", nil, true},
		{"600 不可转", 600, "", nil, false},
		{"400 不可转", 400, "", nil, false},
		{"422 不可转", 422, "", nil, false},
		{"普通 404 可转", 404, "", nil, true},
		{"Responses 存储 404 不可转", 404, "previous_response_id resp_abc not found", nil, false},
		{"response not found 不可转", 404, "Response not found", nil, false},
		{"限流文本可转", 0, "Rate limit exceeded", nil, true},
		{"连接重置可转", 0, "read: connection reset by peer", nil, true},
		{"取消不可转", 0, "context canceled", context.Canceled, false},
		{"超时不可转", 0, "deadline exceeded", context.DeadlineExceeded, false},
		{"无状态无特征不可转", 0, "some weird failure", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := routeFailureEligible(c.status, c.text, c.err); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestPreparePayload(t *testing.T) {
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out := preparePayload(body, "openai", "openai", "target-x", true)
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		t.Fatal("输出应为合法 JSON")
	}
	if m["model"] != "target-x" {
		t.Fatalf("model 应被改写: %v", m["model"])
	}
	opts, _ := m["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Fatal("openai 流式应注入 include_usage")
	}

	out = preparePayload(body, "claude", "claude", "target-y", true)
	m = map[string]any{}
	if json.Unmarshal(out, &m) != nil {
		t.Fatal("输出应为合法 JSON")
	}
	if _, exists := m["stream_options"]; exists {
		t.Fatal("claude 协议不应注入 stream_options")
	}

	// 非 JSON 体原样返回。
	raw := []byte("not-json")
	if out := preparePayload(raw, "openai", "openai", "t", true); string(out) != "not-json" {
		t.Fatalf("坏 JSON 应原样返回: %q", out)
	}
}

func TestTargetWithSuffix(t *testing.T) {
	if v := targetWithSuffix("gpt-4o", "-high"); v != "gpt-4o-high" {
		t.Fatalf("应附加后缀: %q", v)
	}
	if v := targetWithSuffix("gpt-4o-low", "-high"); v != "gpt-4o-low" {
		t.Fatalf("自带后缀应保留原样: %q", v)
	}
	if v := targetWithSuffix("gpt-4o", ""); v != "gpt-4o" {
		t.Fatalf("无后缀应原样: %q", v)
	}
}

func routeTestEnv(t *testing.T) (*service.Service, *store.Store) {
	t.Helper()
	c := config.Default()
	c.DataDir = t.TempDir()
	c.DatabaseFile = "exec.db"
	st, err := store.Open(context.Background(), store.Options{Path: c.DatabasePath(), OwnerID: "exec-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ps, err := service.LoadPeppers(c, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return service.New(st, c, ps), st
}

func stubHostCall(t *testing.T, fn func(method string, payload any) (json.RawMessage, error)) {
	t.Helper()
	orig := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) { return fn(method, payload) }
	t.Cleanup(func() { hostCall = orig })
}

// TestExecuteRoutedLoopFailover 覆盖：逐目标转移、单行结算落最终目标、
// route.failover 审计、失败目标进入冷却。
func TestExecuteRoutedLoopFailover(t *testing.T) {
	svc, st := routeTestEnv(t)
	ctx := context.Background()

	ruleA, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "target-a", Priority: 10, Enabled: true, Source: store.PricingSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	_ = ruleA
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "target-b", Priority: 10, Enabled: true, PriceInput: 1000, PriceOutput: 2000, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	routeID, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "auto", Rule: "-> priority [\"target-a\", \"target-b\"]", CooldownSeconds: 60, PricingMode: "target", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := svc.IssueKey(ctx, service.IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	keyRec, err := st.GetKey(ctx, issued.KID)
	if err != nil {
		t.Fatal(err)
	}

	req := rpcExecutorRequest{Model: "auto", SourceFormat: "openai", Format: "openai"}
	request := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	re, failure := resolveRouting(ctx, svc, &keyRec, req, request, false)
	if re == nil || failure != nil {
		t.Fatalf("路由解析失败: %+v %+v", re, failure)
	}

	var calls []string
	stubHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.model.execute" {
			return json.Marshal(map[string]any{})
		}
		p := payload.(rpcHostModelExecutionRequest)
		calls = append(calls, p.Model)
		if p.Model == "target-a" {
			resp := rpcHostModelExecutionResponse{StatusCode: 429}
			return json.Marshal(resp)
		}
		resp := rpcHostModelExecutionResponse{
			StatusCode: 200,
			Body:       json.RawMessage(`{"model":"target-b","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		}
		return json.Marshal(resp)
	})

	envBytes, err := executeRoutedLoop(ctx, re, req, request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var env rpcEnvelope
	if json.Unmarshal(envBytes, &env) != nil || !env.OK {
		t.Fatalf("成功尝试应返回 OK 信封: %s", envBytes)
	}
	if len(calls) != 2 || calls[0] != "target-a" || calls[1] != "target-b" {
		t.Fatalf("尝试序列错误: %v", calls)
	}

	// 单行结算：只有成功目标入 requests 行。
	page, err := svc.ListRequests(ctx, service.UsageFilter{}, 10, 0, "ts", "desc")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("应只落一行，实际 %d", len(page.Items))
	}
	row := page.Items[0]
	if row.Model != "auto" || row.UpstreamModel != "target-b" {
		t.Fatalf("model=%q upstream=%q", row.Model, row.UpstreamModel)
	}
	if row.Result != store.ResultOK || row.OutputTokens != 5 {
		t.Fatalf("行内容异常: result=%v out=%d cost=%d", row.Result, row.OutputTokens, row.CostMicroUSD)
	}
	if row.CostMicroUSD <= 0 {
		t.Fatalf("mode=target 应按目标计价: %d", row.CostMicroUSD)
	}

	// 审计事件存在。
	events, err := st.ListAudit(ctx, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == "route.failover" {
			found = true
		}
	}
	if !found {
		t.Fatal("缺少 route.failover 审计")
	}

	// 失败目标已进冷却。
	if _, cooling := svc.CooldownUntil(routeID, "target-a"); !cooling {
		t.Fatal("target-a 应处于冷却")
	}
}

// TestExecuteRoutedLoopAllCooling 覆盖全冷却时 resolveRouting 的拒绝路径。
func TestExecuteRoutedLoopAllCooling(t *testing.T) {
	svc, st := routeTestEnv(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "x", Priority: 10, Enabled: true, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "auto", Rule: "-> \"x\"", CooldownSeconds: 60, PricingMode: "target", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	issued, err := svc.IssueKey(ctx, service.IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	keyRec, err := st.GetKey(ctx, issued.KID)
	if err != nil {
		t.Fatal(err)
	}
	req := rpcExecutorRequest{Model: "auto"}
	request := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	routeRow, err := st.ListModelRoutes(ctx)
	if err != nil || len(routeRow) == 0 {
		t.Fatal(err)
	}
	svc.MarkRouteFail(routeRow[0].ID, "x", 60)

	re, failure := resolveRouting(ctx, svc, &keyRec, req, request, false)
	if re != nil || failure == nil {
		t.Fatalf("全冷却应返回失败: re=%v failure=%+v", re, failure)
	}
	if failure.code != "upstream_error" {
		t.Fatalf("code=%q", failure.code)
	}
}

// TestStreamDialFailover 覆盖流式拨号转移与读泵收尾。
func TestStreamDialFailover(t *testing.T) {
	svc, st := routeTestEnv(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "target-b", Priority: 10, Enabled: true, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "auto", Rule: "-> priority [\"target-a\", \"target-b\"]", CooldownSeconds: 60, PricingMode: "target", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	issued, err := svc.IssueKey(ctx, service.IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	keyRec, err := st.GetKey(ctx, issued.KID)
	if err != nil {
		t.Fatal(err)
	}
	req := rpcExecutorRequest{Model: "auto", SourceFormat: "openai", Format: "openai"}
	request := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	re, failure := resolveRouting(ctx, svc, &keyRec, req, request, true)
	if re == nil || failure != nil {
		t.Fatalf("路由解析失败: %+v %+v", re, failure)
	}

	streamReads := 0
	stubHostCall(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.model.execute_stream":
			p := payload.(rpcHostModelExecutionRequest)
			if p.Model == "target-a" {
				resp := rpcHostModelStreamResponse{StatusCode: 500}
				return json.Marshal(resp)
			}
			resp := rpcHostModelStreamResponse{StatusCode: 200, StreamID: "stream-1"}
			return json.Marshal(resp)
		case "host.model.stream_read":
			streamReads++
			if streamReads == 1 {
				resp := rpcHostModelStreamReadResponse{Payload: []byte(`data: {"usage":{"prompt_tokens":9}}` + "\n\n")}
				return json.Marshal(resp)
			}
			resp := rpcHostModelStreamReadResponse{Done: true}
			return json.Marshal(resp)
		default:
			return json.Marshal(map[string]any{})
		}
	})

	startedAt := time.Now()
	var dialed rpcHostModelStreamResponse
	attempts := 0
	rw := newBodyRewriter(request, req.SourceFormat, req.Format, true)
	for i := range re.chain {
		stream, outcome, dialErr := dialHostStream(re, req, rw, i)
		attempts++
		switch outcome {
		case dialTransfer:
			continue
		case dialFailed:
			t.Fatalf("不应终局失败: %v", dialErr)
		default:
			dialed = stream
			finalTgt := targetWithSuffix(re.chain[i], re.match.Suffix)
			if err := pumpRoutedStream(re, req, startedAt, dialed, finalTgt, "plugin-stream-1", func(string) {}); err != nil {
				t.Fatalf("读泵失败: %v", err)
			}
		}
	}
	if attempts != 2 {
		t.Fatalf("应拨号两次，实际 %d", attempts)
	}
	page, err := svc.ListRequests(ctx, service.UsageFilter{}, 10, 0, "ts", "desc")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("应只落一行，实际 %d", len(page.Items))
	}
	row := page.Items[0]
	if row.Model != "auto" || row.InputTokens != 9 {
		t.Fatalf("行异常: model=%q in=%d upstream=%q", row.Model, row.InputTokens, row.UpstreamModel)
	}
	if row.UpstreamModel != "target-b" {
		t.Fatalf("UpstreamModel 应兜底为 finalTgt: %q", row.UpstreamModel)
	}
}

// TestBareTargetName 锁定渠道前缀剥离：嗅探失败时的上游兜底名与宿主直报
// 的裸名同构（orcarouter/x 与 x 是同一上游模型）。
func TestBareTargetName(t *testing.T) {
	cases := [][2]string{
		{"orcarouter/deepseek-v4-flash", "deepseek-v4-flash"},
		{"deepseek-v4-flash", "deepseek-v4-flash"},
		{"a/b/c", "c"},
	}
	for _, c := range cases {
		if got := bareTargetName(c[0]); got != c[1] {
			t.Fatalf("bareTargetName(%q) = %q, 期望 %q", c[0], got, c[1])
		}
	}
}

// TestModelRegisterContract 锁定与宿主的 model.register 契约：
// 响应形状（provider/models）与 ModelInfo 字段名（PascalCase 无 tag）。
func TestModelRegisterContract(t *testing.T) {
	svc, st := routeTestEnv(t)
	ctx := context.Background()
	if _, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "auto", Rule: "-> priority [\"a\", \"b\"]", CooldownSeconds: 60, PricingMode: "target", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "off", Rule: "-> \"c\"", CooldownSeconds: 60, PricingMode: "target", Enabled: false}); err != nil {
		t.Fatal(err)
	}

	raw, err := modelRegister(svc)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("信封异常: %s", raw)
	}
	var resp struct {
		Provider string `json:"provider"`
		Models   []struct {
			ID          string `json:"ID"`
			Object      string `json:"Object"`
			OwnedBy     string `json:"OwnedBy"`
			DisplayName string `json:"DisplayName"`
			Name        string `json:"Name"`
			Description string `json:"Description"`
			UserDefined bool   `json:"UserDefined"`
		} `json:"models"`
	}
	if json.Unmarshal(env.Result, &resp) != nil {
		t.Fatalf("result 解析失败: %s", env.Result)
	}
	if resp.Provider != "cpa-usage-manager" {
		t.Fatalf("provider=%q", resp.Provider)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("停用路由不应出现，应只有 1 条: %+v", resp.Models)
	}
	m := resp.Models[0]
	if m.ID != "auto" || m.Name != "auto" || m.DisplayName != "auto" {
		t.Fatalf("别名三字段应一致: %+v", m)
	}
	if m.Object != "model" || m.OwnedBy != "cpa-usage-manager" || !m.UserDefined {
		t.Fatalf("字段异常: %+v", m)
	}
	if m.Description == "" {
		t.Fatal("Description 不应为空")
	}
}

// bucketHasClaim 报告某模型桶里是否有该 Key 的在途认领（测试检查用）。
func bucketHasClaim(kid, model string) bool {
	b := claimBucketFor(normalizeModelKey(model))
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.claims {
		if c.keyID == kid && c.registered {
			return true
		}
	}
	return false
}

// TestSlashAliasClaimExclusion 验证斜杠别名不把别名形态登记进认领桶：
// 「grp/auto」归一后落在裸名 auto 的桶，若登记会与真实模型 auto 的直连流量互相误吞；
// refs 的登记不受影响。普通别名维持原行为。
func TestSlashAliasClaimExclusion(t *testing.T) {
	svc, st := routeTestEnv(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "t9", Priority: 10, Enabled: true, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "grp/auto", Rule: `-> "t9"`, CooldownSeconds: 60, PricingMode: "target", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	issued, err := svc.IssueKey(ctx, service.IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	keyRec, err := st.GetKey(ctx, issued.KID)
	if err != nil {
		t.Fatal(err)
	}
	req := rpcExecutorRequest{Model: "grp/auto", SourceFormat: "openai", Format: "openai"}
	request := []byte(`{"model":"grp/auto","messages":[{"role":"user","content":"hi"}]}`)
	re, failure := resolveRouting(ctx, svc, &keyRec, req, request, false)
	if re == nil || failure != nil {
		t.Fatalf("路由解析失败: %+v %+v", re, failure)
	}
	if !bucketHasClaim(issued.KID, "t9") {
		t.Fatal("refs 应照常登记进认领桶")
	}
	if bucketHasClaim(issued.KID, "grp/auto") {
		t.Fatal("斜杠别名不应登记别名形态（裸名归一会撞桶）")
	}
	re.claim.release(0)

	// 普通别名：别名形态仍登记。
	if _, err := st.InsertModelRoute(ctx, store.ModelRoute{Alias: "plain2", Rule: `-> "t9"`, CooldownSeconds: 60, PricingMode: "target", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	req2 := rpcExecutorRequest{Model: "plain2", SourceFormat: "openai", Format: "openai"}
	request2 := []byte(`{"model":"plain2","messages":[{"role":"user","content":"hi"}]}`)
	re2, failure2 := resolveRouting(ctx, svc, &keyRec, req2, request2, false)
	if re2 == nil || failure2 != nil {
		t.Fatalf("路由解析失败: %+v %+v", re2, failure2)
	}
	defer re2.claim.release(0)
	if !bucketHasClaim(issued.KID, "plain2") || !bucketHasClaim(issued.KID, "t9") {
		t.Fatal("普通别名的别名形态与 refs 都应登记")
	}
}
