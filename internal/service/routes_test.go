package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/routelang"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

func insertRoute(t *testing.T, s *Service, alias, rule string) store.ModelRoute {
	t.Helper()
	id, err := s.st.InsertModelRoute(context.Background(), store.ModelRoute{Alias: alias, Rule: rule, CooldownSeconds: 60, PricingMode: "target", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.st.GetModelRoute(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStripThinkingSuffix(t *testing.T) {
	cases := []struct{ in, base, suffix string }{
		{"auto", "auto", ""},
		{"Auto-High", "Auto", "-high"},
		{" auto-low ", "auto", "-low"},
		{"gpt-4o-mini", "gpt-4o-mini", ""},
		{"high", "high", ""},
	}
	for _, c := range cases {
		b, s := StripThinkingSuffix(c.in)
		if b != c.base || s != c.suffix {
			t.Fatalf("StripThinkingSuffix(%q) = (%q,%q), 期望 (%q,%q)", c.in, b, s, c.base, c.suffix)
		}
	}
}

func TestMatchRouteCaseAndSuffix(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	insertRoute(t, s, "Auto", `-> priority ["a", "b"]`)
	if _, ok := s.MatchRoute(ctx, "AUTO"); !ok {
		t.Fatal("大小写不敏感匹配失败")
	}
	m, ok := s.MatchRoute(ctx, "auto-high")
	if !ok {
		t.Fatal("剥后缀匹配失败")
	}
	if m.Suffix != "-high" || m.Route.Alias != "Auto" {
		t.Fatalf("suffix=%q alias=%q", m.Suffix, m.Route.Alias)
	}
	if _, ok := s.MatchRoute(ctx, "other"); ok {
		t.Fatal("未定义别名不应命中")
	}
}

func TestRouteSnapshotInvalidationAndTTLReload(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	if _, ok := s.MatchRoute(ctx, "auto"); ok {
		t.Fatal("空表不应命中")
	}
	insertRoute(t, s, "auto", `-> "x"`)
	// 写回调已失效快照，无需等待 TTL。
	if _, ok := s.MatchRoute(ctx, "auto"); !ok {
		t.Fatal("插入后应立即生效（写回调失效）")
	}
	if err := st.DeleteModelRoute(ctx, s.mustRouteID(t, s, "auto")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MatchRoute(ctx, "auto"); ok {
		t.Fatal("删除后不应命中")
	}
}

func (s *Service) mustRouteID(t *testing.T, _ *Service, alias string) int64 {
	t.Helper()
	rows, err := s.st.ListModelRoutes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Alias == alias {
			return r.ID
		}
	}
	t.Fatalf("找不到路由 %q", alias)
	return 0
}

func TestResolveChainCooldownFilterOrder(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	row := insertRoute(t, s, "auto", `-> priority ["a", "b", "c"]`)
	prog, err := routelang.Compile(row.Rule)
	if err != nil {
		t.Fatal(err)
	}
	m := RouteMatch{Route: CompiledRoute{ModelRoute: row, Prog: prog}}
	env := &routelang.Env{Vars: map[string]any{"input_tokens": int64(1), "body_len": int64(2), "model": "auto", "stream": false, "thinking_effort": "", "source": "openai"}}

	chain, fellBack, err := s.ResolveChain(ctx, m, env, nil, nil)
	if err != nil || fellBack {
		t.Fatalf("首次求值失败: %v fellBack=%v", err, fellBack)
	}
	if len(chain) != 3 || chain[0] != "a" || chain[1] != "b" || chain[2] != "c" {
		t.Fatalf("链序错误: %v", chain)
	}
	// 冷却 b：过滤保序摘除。
	s.MarkRouteFail(row.ID, "B", row.CooldownSeconds)
	chain, _, err = s.ResolveChain(ctx, m, env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || chain[0] != "a" || chain[1] != "c" {
		t.Fatalf("冷却后应为 [a c]: %v", chain)
	}
	// 全冷却 → 哨兵。
	s.MarkRouteFail(row.ID, "a", row.CooldownSeconds)
	s.MarkRouteFail(row.ID, "c", row.CooldownSeconds)
	if _, _, err := s.ResolveChain(ctx, m, env, nil, nil); !errors.Is(err, ErrAllTargetsCooling) {
		t.Fatalf("全冷却应返回哨兵: %v", err)
	}
	// 到期自然恢复：把截止时刻拨回过去（同包内直接操作状态器，避免真实睡眠）。
	s.coolMu.Lock()
	for k := range s.cooldowns {
		s.cooldowns[k] = time.Now().Add(-time.Second)
	}
	s.coolMu.Unlock()
	chain, _, err = s.ResolveChain(ctx, m, env, nil, nil)
	if err != nil || len(chain) != 3 {
		t.Fatalf("到期后应恢复全链: %v %v", chain, err)
	}
}

func TestJudgeSettingsRoundtrip(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	cfg, err := s.judgeSettingsCached(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "" || cfg.TimeoutMS != defaultJudgeTimeout.Milliseconds() {
		t.Fatalf("默认设置错误: %+v", cfg)
	}
	if err := s.SaveJudgeSettings(ctx, " judge-x ", 4000); err != nil {
		t.Fatal(err)
	}
	s.flushJudgeConfig() // 绕过 30s 内存缓存直接验证落库值
	cfg, err = s.judgeSettingsCached(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "judge-x" || cfg.TimeoutMS != 4000 {
		t.Fatalf("保存后读取: %+v", cfg)
	}
	if err := s.SaveJudgeSettings(ctx, "judge", 100); err == nil {
		t.Fatal("超时下限校验缺失")
	}
}

func stubJudge(t *testing.T, s *Service, fn func(model string) (string, error)) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	s.SetJudgeExecutor(func(_ context.Context, model string, _ []byte) ([]byte, int, error) {
		calls.Add(1)
		out, err := fn(model)
		if err != nil {
			return nil, 500, err
		}
		return []byte(`{"choices":[{"message":{"role":"assistant","content":"` + out + `"}}]}`), 200, nil
	})
	return &calls
}

func judgeEnv(s *Service, model string) *routelang.Env {
	return s.BuildRouteEnv(ParseRequestMeta([]byte(`{"model":"` + model + `"}`)), model, true, "openai")
}

func TestAIJudgeEvalWithCache(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if err := s.SaveJudgeSettings(ctx, "judge-model", 3000); err != nil {
		t.Fatal(err)
	}
	s.flushJudgeConfig()
	calls := stubJudge(t, s, func(string) (string, error) { return "hard", nil })

	row := insertRoute(t, s, "smart", `when ai_judge(["simple", "hard"]) == "hard"
  -> priority ["opus"]
-> "mini"`)
	prog, err := routelang.Compile(row.Rule)
	if err != nil {
		t.Fatal(err)
	}
	m := RouteMatch{Route: CompiledRoute{ModelRoute: row, Prog: prog}}
	env := judgeEnv(s, "smart")

	digestFn := sync.OnceValues(func() (string, error) { return "帮我写个排序算法", nil })
	chain, fellBack, err := s.ResolveChain(ctx, m, env, digestFn, nil)
	if err != nil || fellBack {
		t.Fatalf("求值失败: %v fellBack=%v", err, fellBack)
	}
	if len(chain) != 1 || chain[0] != "opus" {
		t.Fatalf("judge=hard 应命中 opus: %v", chain)
	}
	// 相同变量组合再次求值 → 缓存命中，不再发起调用。
	env2 := judgeEnv(s, "smart")
	if _, _, err := s.ResolveChain(ctx, m, env2, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("缓存应使命令为 1 次，实际 %d", n)
	}
}

func TestAIJudgeFailureFallsBackWithAudit(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	if err := s.SaveJudgeSettings(ctx, "judge-model", 500); err != nil {
		t.Fatal(err)
	}
	s.flushJudgeConfig()
	stubJudge(t, s, func(string) (string, error) { return "", errors.New("boom") })

	row := insertRoute(t, s, "smart", `when ai_judge(["simple", "hard"]) == "hard"
  -> "opus"
-> "mini"`)
	prog, err := routelang.Compile(row.Rule)
	if err != nil {
		t.Fatal(err)
	}
	m := RouteMatch{Route: CompiledRoute{ModelRoute: row, Prog: prog}}

	chain, fellBack, err := s.ResolveChain(ctx, m, judgeEnv(s, "smart"), nil, nil)
	if !fellBack {
		t.Fatalf("AI 失败应回落兜底: err=%v fellBack=%v", err, fellBack)
	}
	var aiFB *routelang.AIFallbackError
	if !errors.As(err, &aiFB) {
		t.Fatalf("应返回 AIFallbackError: %v", err)
	}
	if len(chain) != 1 || chain[0] != "mini" {
		t.Fatalf("兜底链应为 [mini]: %v", chain)
	}
	events, aerr := st.ListAudit(ctx, 10, 0)
	if aerr != nil {
		t.Fatal(aerr)
	}
	found := false
	for _, e := range events {
		if e.Action == "route.ai_fallback" && e.EntityID == strconv.FormatInt(row.ID, 10) {
			found = true
		}
	}
	if !found {
		t.Fatal("缺少 route.ai_fallback 审计事件")
	}
}

func TestValidateRouteRuleBranches(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()

	// 斜杠别名允许（撞真实命名由下方专项检查拦截）。
	if _, _, _, err := s.ValidateRouteRule(ctx, 0, "grp/name", `-> "x"`, "target"); err != nil {
		t.Fatalf("斜杠别名应放行: %v", err)
	}
	if _, _, _, err := s.ValidateRouteRule(ctx, 0, "a-high", `-> "x"`, "target"); err == nil {
		t.Fatal("别名带思考后缀应报错")
	}
	if _, _, _, err := s.ValidateRouteRule(ctx, 0, "r1", `when foo("a") == "b"
-> "x"`, "target"); err == nil {
		t.Fatal("未知函数应报错")
	}
	// ai_judge 需先配置评判模型。
	if _, _, _, err := s.ValidateRouteRule(ctx, 0, "r1", `when ai_judge(["a"]) == "a"
-> "x"`, "target"); err == nil {
		t.Fatal("未配评判模型时使用 ai_judge 应报错")
	}
	if err := s.SaveJudgeSettings(ctx, "judge-model", 3000); err != nil {
		t.Fatal(err)
	}
	s.flushJudgeConfig()
	insertRoute(t, s, "loopA", `-> "zzz"`)
	refs, usesAI, _, err := s.ValidateRouteRule(ctx, 0, "r2", `when ai_judge(["simple","hard"]) == "hard"
  -> priority ["real-model-a", "real-model-b"]
-> "fallback-model"`, "target")
	if err != nil {
		t.Fatalf("合法规则不应报错: %v", err)
	}
	if !usesAI || len(refs) != 3 {
		t.Fatalf("usesAI=%v refs=%v", usesAI, refs)
	}
	if _, _, _, err = s.ValidateRouteRule(ctx, 0, "r3", `when ai_judge(["simple"]) == "simple"
  -> "loopA"
-> "fallback-model"`, "target"); err == nil {
		t.Fatal("引用其他启用别名应报错")
	}
	if _, _, _, err = s.ValidateRouteRule(ctx, 0, "r4", `-> "r4-target"`, "target"); err != nil {
		t.Fatalf("引用普通模型应通过: %v", err)
	}
	// 自引用（编辑自身时 excludeID 生效）。
	self := insertRoute(t, s, "selfref", `-> "plain"`)
	if _, _, _, err = s.ValidateRouteRule(ctx, self.ID, "selfref", `-> "selfref-other"`, "target"); err != nil {
		t.Fatalf("排除自身后不应误报: %v", err)
	}
	if _, _, _, err = s.ValidateRouteRule(ctx, self.ID, "selfref", `-> "SELFREF"`, "target"); err == nil {
		t.Fatal("自引用应报错")
	}
	// mode=alias 且无计价规则 → warning。
	_, _, warn, err := s.ValidateRouteRule(ctx, 0, "aliasmode", `-> "x"`, "alias")
	if err != nil || warn == "" {
		t.Fatalf("mode=alias 应给 warning: err=%v warn=%q", err, warn)
	}
	// 撞真实命名之一：别名命中其他启用路由的引用目标。
	if _, _, _, err = s.ValidateRouteRule(ctx, 0, "zzz", `-> "x"`, "target"); err == nil {
		t.Fatal("别名撞其他路由的引用目标应报错")
	}
	// 撞真实命名之二：别名命中历史请求出现过的模型名（含 upstream_model）。
	issued, err := s.IssueKey(ctx, IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Reserve(ctx, ReservationRequest{KeyID: issued.KID, Model: "hist-real-model", EstimatedTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	u := usageparse.Usage{}
	u.InputTokens = 1
	if _, err := s.Settle(ctx, res.ID, u, &store.Request{ID: "req-hist", Model: "hist-real-model", UpstreamModel: "hist-upstream-model"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hist-real-model", "Hist-Upstream-Model"} {
		if _, _, _, err = s.ValidateRouteRule(ctx, 0, name, `-> "x"`, "target"); err == nil {
			t.Fatalf("别名 %q 撞历史真实模型名应报错", name)
		}
	}
	// 已有别名自身的历史流量不构成撞名（路由行的 model 列就是别名）。
	routed := insertRoute(t, s, "routed-alias", `-> "rt-target"`)
	res2, err := s.Reserve(ctx, ReservationRequest{KeyID: issued.KID, Model: "routed-alias", EstimatedTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settle(ctx, res2.ID, u, &store.Request{ID: "req-routed", UpstreamModel: "rt-target"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.ValidateRouteRule(ctx, routed.ID, "routed-alias", `-> "rt-target"`, "target"); err != nil {
		t.Fatalf("编辑已有别名不应被自身历史流量误伤: %v", err)
	}
}

func TestJudgeSingleFlightMergesConcurrentCalls(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if err := s.SaveJudgeSettings(ctx, "judge-model", 3000); err != nil {
		t.Fatal(err)
	}
	s.flushJudgeConfig()
	release := make(chan struct{})
	var calls atomic.Int32
	s.SetJudgeExecutor(func(_ context.Context, _ string, _ []byte) ([]byte, int, error) {
		calls.Add(1)
		<-release
		return []byte(`{"choices":[{"message":{"content":"hard"}}]}`), 200, nil
	})
	row := insertRoute(t, s, "sf", `when ai_judge(["simple","hard"]) == "hard"
  -> "opus"
-> "mini"`)
	prog, _ := routelang.Compile(row.Rule)
	m := RouteMatch{Route: CompiledRoute{ModelRoute: row, Prog: prog}}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.ResolveChain(ctx, m, judgeEnv(s, "sf"), nil, nil)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("single-flight 应合并为 1 次调用，实际 %d", n)
	}
}

func TestRequestDigestExtraction(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":[{"type":"text","text":"你好世界"}]}],"stream":true}`)
	d := RequestDigest(body)
	if len(d) == 0 {
		t.Fatal("摘要不应为空")
	}
	if len(d) > 2000 {
		t.Fatalf("摘要超限: %d", len(d))
	}
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	capped := RequestDigest([]byte(`{"messages":[{"content":"` + string(long) + `"}]}`))
	if len(capped) != 2000 {
		t.Fatalf("长文本应封顶 2000，实际 %d", len(capped))
	}
}

func TestPickOptionMatching(t *testing.T) {
	opts := []string{"simple", "hard"}
	cases := []struct {
		text string
		want string
		ok   bool
	}{
		{"hard", "hard", true},
		{"  Hard \n", "hard", true},
		{"\"simple\"", "simple", true},
		{"我认为这是 hard 级别", "hard", true},
		{"simple or hard 都行", "", false},
		{"unknown", "", false},
	}
	for _, c := range cases {
		got, ok := pickOption(c.text, opts)
		if got != c.want || ok != c.ok {
			t.Fatalf("pickOption(%q) = (%q,%v), 期望 (%q,%v)", c.text, got, ok, c.want, c.ok)
		}
	}
}

// TestResolveChainDigestLazy 验证摘要惰性化：不含 ai_judge 的规则不触发
// digestFn（热路径免整包解析）；含 ai_judge 的规则恰好调用一次。
func TestResolveChainDigestLazy(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()

	calls := 0
	digestFn := func() (string, error) { calls++; return "digest-text", nil }

	row := insertRoute(t, s, "plain", `-> priority ["a", "b"]`)
	prog, err := routelang.Compile(row.Rule)
	if err != nil {
		t.Fatal(err)
	}
	m := RouteMatch{Route: CompiledRoute{ModelRoute: row, Prog: prog}}
	env := &routelang.Env{Vars: map[string]any{"input_tokens": int64(1), "body_len": int64(2), "model": "plain", "stream": false, "thinking_effort": "", "source": "openai"}}
	if _, _, err := s.ResolveChain(ctx, m, env, digestFn, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("非 ai_judge 规则不应解析请求体，实际 %d 次", calls)
	}

	if err := s.SaveJudgeSettings(ctx, "judge-model", 3000); err != nil {
		t.Fatal(err)
	}
	s.flushJudgeConfig()
	stubJudge(t, s, func(string) (string, error) { return "hard", nil })
	row2 := insertRoute(t, s, "smart", `when ai_judge(["simple","hard"]) == "hard"
  -> "opus"
-> "mini"`)
	prog2, err := routelang.Compile(row2.Rule)
	if err != nil {
		t.Fatal(err)
	}
	m2 := RouteMatch{Route: CompiledRoute{ModelRoute: row2, Prog: prog2}}
	if _, _, err := s.ResolveChain(ctx, m2, judgeEnv(s, "smart"), digestFn, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("含 ai_judge 的规则应恰好解析一次，实际 %d 次", calls)
	}
}

// TestRouteSnapshotSingleFlight 验证 TTL 失效瞬间的并发重载只放行一个构建者。
func TestRouteSnapshotSingleFlight(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	insertRoute(t, s, "auto", `-> "x"`)
	s.invalidateRoutes()

	var calls atomic.Int32
	orig := listRoutesFn
	listRoutesFn = func(ctx context.Context, st *store.Store) ([]store.ModelRoute, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond) // 拉长重载窗口，放大竞态
		return orig(ctx, st)
	}
	t.Cleanup(func() { listRoutesFn = orig })

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.MatchRoute(ctx, "auto"); !ok {
				t.Error("并发重载后应命中")
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("惊群应被合并为一次重载，实际 %d 次", got)
	}
	_ = st
}

// TestTestRouteDryRun 覆盖规则干跑：编译错误、变量口径（提示词合成体进
// input_tokens/body_len）、条件命中、冷却摘除（仅真实 ID）与 ai_judge 跳过回落。
func TestTestRouteDryRun(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()

	// 编译错误：Error 带行列号，无链。
	res := s.TestRoute(ctx, TestRouteRequest{Alias: "auto", Rule: "-> priority [\"a\",\n-> \"b\""})
	if res.Error == "" || len(res.Chain) != 0 || !strings.Contains(res.Error, "第 2 行") {
		t.Fatalf("编译错误应带定位且无链: %+v", res)
	}

	// 变量口径：prompt 合成为 messages 体，body_len/input_tokens 与真实请求同式。
	prompt := "你好世界"
	synth, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": prompt}}})
	rule := "when input_tokens <= 100000 && source == \"gemini\"\n  -> priority [\"a\", \"b\"]\n-> \"c\""
	res = s.TestRoute(ctx, TestRouteRequest{Alias: "auto", Rule: rule, Prompt: prompt, Source: "gemini", Model: "auto-high"})
	if res.Error != "" {
		t.Fatalf("求值失败: %s", res.Error)
	}
	if len(res.Chain) != 2 || res.Chain[0] != "a" || res.Chain[1] != "b" || res.FellBack || res.AISkipped {
		t.Fatalf("应命中第一分支整链: %+v", res)
	}
	wantIn := int64(len(synth))/2 + 1
	if res.Vars["input_tokens"] != wantIn || res.Vars["body_len"] != int64(len(synth)) {
		t.Fatalf("input_tokens/body_len 口径错误: want %d/%d got %v/%v",
			wantIn, len(synth), res.Vars["input_tokens"], res.Vars["body_len"])
	}
	// model 变量取模拟请求名剥思考后缀。
	if res.Vars["model"] != "auto" || res.Vars["source"] != "gemini" {
		t.Fatalf("model/source 变量错误: %v %v", res.Vars["model"], res.Vars["source"])
	}

	// 条件不命中走兜底；空 prompt 时 body_len=0、input_tokens=1。
	res = s.TestRoute(ctx, TestRouteRequest{Alias: "t2", Rule: "when input_tokens > 100\n  -> \"big\"\n-> \"small\""})
	if res.Error != "" || len(res.Chain) != 1 || res.Chain[0] != "small" {
		t.Fatalf("应走兜底分支: %+v", res)
	}
	if res.Vars["input_tokens"] != int64(1) || res.Vars["body_len"] != int64(0) {
		t.Fatalf("空提示词口径应为 0/1: %v", res.Vars)
	}

	// 冷却过滤只对已保存路由（ID>0）生效，并报告被摘除目标。
	row := insertRoute(t, s, "cd", `-> priority ["X", "Y"]`)
	s.MarkRouteFail(row.ID, "X", 60)
	res = s.TestRoute(ctx, TestRouteRequest{ID: row.ID, Alias: "cd", Rule: row.Rule})
	if res.Error != "" || len(res.Chain) != 1 || res.Chain[0] != "Y" {
		t.Fatalf("冷却目标应被摘除: %+v", res)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Target != "X" || res.Skipped[0].Until.IsZero() {
		t.Fatalf("应报告被跳过的冷却目标: %+v", res.Skipped)
	}
	// 同一规则按草稿（ID=0）干跑不做冷却过滤——草稿没有冷却历史。
	res = s.TestRoute(ctx, TestRouteRequest{Alias: "cd", Rule: row.Rule})
	if len(res.Chain) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("草稿干跑不应受冷却影响: %+v", res)
	}

	// ai_judge 默认跳过：按 AI 失败回落兜底分支，零外部副作用。
	aiRule := "when ai_judge([\"simple\"]) == \"simple\"\n  -> \"smart\"\n-> \"dumb\""
	res = s.TestRoute(ctx, TestRouteRequest{Alias: "j", Rule: aiRule})
	if !res.AISkipped || !res.FellBack || len(res.Chain) != 1 || res.Chain[0] != "dumb" {
		t.Fatalf("未勾选 run_ai 应跳过并回落: %+v", res)
	}
	// 勾选但评判模型未配置：同样回落（aiJudge 先报配置缺失），不算「跳过」。
	res = s.TestRoute(ctx, TestRouteRequest{Alias: "j", Rule: aiRule, RunAI: true})
	if res.AISkipped || !res.FellBack || len(res.Chain) != 1 || res.Chain[0] != "dumb" {
		t.Fatalf("run_ai 但未配置评判模型应回落: %+v", res)
	}
}

// TestJudgeAttribution 验证评判子调用的归属窗口：调用中与宽限期内按裸名
// 匹配归属到触发 Key，模型不符或窗口过期不归属。
func TestJudgeAttribution(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if err := s.SaveJudgeSettings(ctx, "chan/judge-x", 4000); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.AttributeJudgeUsage("judge-x"); ok {
		t.Fatal("无在途调用不应归属")
	}
	s.judgeBegin(JudgeAttribution{KID: "kid1", CallerID: "caller1"})
	attr, ok := s.AttributeJudgeUsage("provider/CHAN/JUDGE-X")
	if !ok || attr.KID != "kid1" || attr.CallerID != "caller1" {
		t.Fatalf("调用中应按裸名归属: ok=%v attr=%+v", ok, attr)
	}
	if _, ok := s.AttributeJudgeUsage("other-model"); ok {
		t.Fatal("非评判模型不应归属")
	}
	s.judgeEnd()
	if _, ok := s.AttributeJudgeUsage("judge-x"); !ok {
		t.Fatal("宽限期内应继续归属")
	}
	old := judgeAttrWindow
	judgeAttrWindow = -time.Second
	t.Cleanup(func() { judgeAttrWindow = old })
	s.judgeBegin(JudgeAttribution{KID: "kid2"})
	s.judgeEnd()
	if _, ok := s.AttributeJudgeUsage("judge-x"); ok {
		t.Fatal("窗口过期后不应归属")
	}
}
