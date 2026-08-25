// Package service 的模型路由层：集合别名注册表、候选链求值、目标冷却
// 与 ai_judge 执行器。
//
// 数据流（执行器视角）：
//
//	route, hit := svc.MatchRoute(ctx, req.Model)
//	env := svc.BuildRouteEnv(plan, req.Model, isStream, req.SourceFormat)
//	chain, fellBack, err := svc.ResolveChain(ctx, route, env, digest)
//	… 失败目标 → svc.MarkRouteFail(route.ID, target, route.CooldownSeconds)
package service

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/routelang"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// ErrAllTargetsCooling 表示规则求值成功但全部候选都在冷却期。
var ErrAllTargetsCooling = errors.New("service: 路由候选目标全部冷却中")

const (
	// routeSnapshotTTL 是路由快照的最长存活时间；事件失效为主，此值只兜底跨进程改写。
	routeSnapshotTTL = time.Minute

	// judgeCacheTTL / judgeCacheMax 是 ai_judge 结果缓存的生命周期与容量上限。
	judgeCacheTTL = 10 * time.Minute
	judgeCacheMax = 512

	// prefJudgeModel / prefJudgeTimeoutMS 是评判模型设置在 preferences KV 的键名。
	prefJudgeModel     = "routing_judge_model"
	prefJudgeTimeoutMS = "routing_judge_timeout_ms"

	defaultJudgeTimeout = 8000 * time.Millisecond
)

// thinkingSuffixes 是请求模型名可能携带的思考强度后缀。匹配别名前先剥掉；
// 精确全名匹配优先，因此真实模型名恰好以这些词结尾时不受影响。
var thinkingSuffixes = []string{"-thinking", "-high", "-medium", "-low", "-minimal", "-none"}

// CompiledRoute 是一条启用中的路由及其编译产物。快照内只放启用路由，
// 停用/删除经写回调即时失效。
type CompiledRoute struct {
	store.ModelRoute
	Prog *routelang.Program
}

// RouteMatch 是 MatchRoute 的命中结果。Suffix 是请求模型名携带的思考后缀，
// 转发时目标自带后缀则用目标的，否则附加原后缀。
type RouteMatch struct {
	Route  CompiledRoute
	Suffix string
}

type routeSnapshot struct {
	at      time.Time
	byAlias map[string]CompiledRoute // key = strings.ToLower(alias)
}

// StripThinkingSuffix 剥离模型名的思考强度后缀，返回基名与后缀（含连字符）。
func StripThinkingSuffix(model string) (base, suffix string) {
	trimmed := strings.TrimSpace(model)
	m := strings.ToLower(trimmed)
	for _, sfx := range thinkingSuffixes {
		if len(m) > len(sfx) && strings.HasSuffix(m, sfx) {
			return strings.TrimSpace(trimmed[:len(trimmed)-len(sfx)]), sfx
		}
	}
	return trimmed, ""
}

// invalidateRoutes 失效内存快照；由 Store 写回调触发。
func (s *Service) invalidateRoutes() { s.routeSnap.Store(nil) }

// listRoutesFn 是快照重载的取数桩点：测试替换它以统计重载次数。
var listRoutesFn = func(ctx context.Context, st *store.Store) ([]store.ModelRoute, error) {
	return st.ListModelRoutes(ctx)
}

// routeSnapshot 返回当前路由快照，过期时从存储重载。
// 编译失败的行跳过（保存期已校验；此处容忍跨进程写入的坏行，不影响其他集合）。
//
// TTL 过期瞬间所有在途请求都会走重载——用 double-checked locking 合并：
// 拿锁后复核 TTL，命中则直接复用他人刚建好的快照，避免 N×(SELECT+Compile) 惊群。
func (s *Service) routeSnapshot(ctx context.Context) (*routeSnapshot, error) {
	if snap := s.routeSnap.Load(); snap != nil && time.Since(snap.at) < routeSnapshotTTL {
		return snap, nil
	}
	s.routeReloadMu.Lock()
	defer s.routeReloadMu.Unlock()
	if snap := s.routeSnap.Load(); snap != nil && time.Since(snap.at) < routeSnapshotTTL {
		return snap, nil
	}
	rows, err := listRoutesFn(ctx, s.st)
	if err != nil {
		return nil, err
	}
	snap := &routeSnapshot{at: time.Now(), byAlias: make(map[string]CompiledRoute, len(rows))}
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		prog, err := routelang.Compile(r.Rule)
		if err != nil {
			continue
		}
		r.Refs = prog.ReferencedModels()
		snap.byAlias[strings.ToLower(strings.TrimSpace(r.Alias))] = CompiledRoute{ModelRoute: r, Prog: prog}
	}
	s.routeSnap.Store(snap)
	return snap, nil
}

// ListRoutesCompiled 列出全部路由并填充 Refs（管理端点用；含停用与坏行）。
func (s *Service) ListRoutesCompiled(ctx context.Context) ([]store.ModelRoute, error) {
	rows, err := s.st.ListModelRoutes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if prog, err := routelang.Compile(rows[i].Rule); err == nil {
			rows[i].Refs = prog.ReferencedModels()
		}
	}
	return rows, nil
}

// MatchRoute 按别名词法匹配路由：先精确全名，再剥离思考后缀后 EqualFold。
func (s *Service) MatchRoute(ctx context.Context, model string) (RouteMatch, bool) {
	model = strings.TrimSpace(model)
	snap, err := s.routeSnapshot(ctx)
	if err != nil || len(snap.byAlias) == 0 {
		return RouteMatch{}, false
	}
	base, suffix := model, ""
	try := func(name string) (CompiledRoute, bool) {
		cr, ok := snap.byAlias[strings.ToLower(name)]
		return cr, ok && cr.Prog != nil
	}
	cr, ok := try(base)
	if !ok {
		base, suffix = StripThinkingSuffix(model)
		if base == "" || base == model {
			return RouteMatch{}, false
		}
		cr, ok = try(base)
	}
	if !ok {
		return RouteMatch{}, false
	}
	return RouteMatch{Route: cr, Suffix: suffix}, true
}

// ---------- 冷却状态器 ----------

// cooldowns 键为 "routeID|lower(target)"，值为冷却截止时刻。
// 进程内状态：reconfigure 或重启丢失、多实例各自独立——failover 链本身兜底。
func cooldownKey(routeID int64, target string) string {
	return strconv.FormatInt(routeID, 10) + "|" + strings.ToLower(target)
}

// MarkRouteFail 把失败目标置入冷却。cooldownSeconds ≤0 时忽略。
func (s *Service) MarkRouteFail(routeID int64, target string, cooldownSeconds int64) {
	if cooldownSeconds <= 0 {
		return
	}
	s.coolMu.Lock()
	if s.cooldowns == nil {
		s.cooldowns = make(map[string]time.Time)
	}
	s.cooldowns[cooldownKey(routeID, target)] = time.Now().Add(time.Duration(cooldownSeconds) * time.Second)
	s.coolMu.Unlock()
}

// CooldownUntil 返回目标的冷却截止时刻（面板展示与测试断言用）。
func (s *Service) CooldownUntil(routeID int64, target string) (time.Time, bool) {
	s.coolMu.Lock()
	defer s.coolMu.Unlock()
	until, ok := s.cooldowns[cooldownKey(routeID, target)]
	return until, ok
}

// filterCooldown 过滤冷却中的目标：保序摘除，顺带清理到期条目。
func (s *Service) filterCooldown(routeID int64, chain []string) []string {
	now := time.Now()
	out := make([]string, 0, len(chain))
	s.coolMu.Lock()
	for _, t := range chain {
		key := cooldownKey(routeID, t)
		until, ok := s.cooldowns[key]
		if !ok {
			out = append(out, t)
			continue
		}
		if now.After(until) {
			delete(s.cooldowns, key)
			out = append(out, t)
			continue
		}
	}
	s.coolMu.Unlock()
	return out
}

// ---------- 候选链求值 ----------

// ResolveChain 求值路由规则得到有序候选链，再过滤冷却目标。
// fellBack 为真表示 ai_judge 失败已自动回落兜底分支（此处落审计事件）；
// 除 ErrAllTargetsCooling 外的错误表示规则运行期异常（变量缺失等），无链返回。
//
// digestFn 惰性提供请求摘要（ai_judge 的提示词素材）：Go 实参急切求值，
// 若按值传入，不含 ai_judge 的规则也要白付一次整包解析——因此只在
// UsesAI 时由调用方构造闭包（配合 sync.OnceValues），且首次调用才真正解析。
func (s *Service) ResolveChain(ctx context.Context, m RouteMatch, env *routelang.Env, digestFn func() (string, error)) ([]string, bool, error) {
	if m.Route.Prog.UsesAI() {
		if digestFn == nil {
			digestFn = func() (string, error) { return "", nil }
		}
		var once sync.Once
		var judgeErr error
		env.AI = func(cctx context.Context, options []string) (string, error) {
			once.Do(func() { _, judgeErr = s.judgeSettingsCached(cctx) })
			if judgeErr != nil {
				return "", judgeErr
			}
			digest, _ := digestFn()
			return s.aiJudge(cctx, env.Vars, digest, options)
		}
	}
	chain, fellBack, err := m.Route.Prog.Eval(ctx, env)
	if err != nil && chain == nil {
		return nil, false, err
	}
	if fellBack {
		_ = s.st.AppendAudit(ctx, store.AuditEvent{
			Action:     "route.ai_fallback",
			EntityType: "model_route",
			EntityID:   strconv.FormatInt(m.Route.ID, 10),
			Detail: map[string]any{
				"alias": m.Route.Alias,
				"error": err.Error(),
			},
		})
	}
	filtered := s.filterCooldown(m.Route.ID, chain)
	if len(filtered) == 0 {
		return nil, fellBack, ErrAllTargetsCooling
	}
	return filtered, fellBack, err
}

// BuildRouteEnv 构造规则语言的运行时变量集。input_tokens 与预占估算同口径：
// body 字节数/2+1 封顶 MaxTokenEstimate；model 为请求原名剥思考后缀。
func (s *Service) BuildRouteEnv(meta RequestMeta, rawModel string, stream bool, source string) *routelang.Env {
	in := int64(meta.BodyLen)/2 + 1
	if in > s.cfg.Quota.Limits.MaxTokenEstimate {
		in = s.cfg.Quota.Limits.MaxTokenEstimate
	}
	base, _ := StripThinkingSuffix(rawModel)
	if base == "" {
		base = meta.Model
	}
	return &routelang.Env{
		Vars: map[string]any{
			"input_tokens":    in,
			"body_len":        int64(meta.BodyLen),
			"model":           base,
			"stream":          stream,
			"thinking_effort": meta.ResolvedThinking,
			"source":          source,
		},
	}
}

// HasThinkingSuffix 报告模型名是否携带思考强度后缀。
func HasThinkingSuffix(model string) bool {
	_, sfx := StripThinkingSuffix(model)
	return sfx != ""
}

// ValidateRouteRule 在保存期校验路由配置，返回静态引用集与告警。
// 校验：语法（行列号定位）、alias 形态与唯一性、ai_judge 需已配评判模型、
// 引用模型不得命中任何启用别名（含自引用）；mode=alias 且别名无计价规则时给 warning。
func (s *Service) ValidateRouteRule(ctx context.Context, excludeID int64, alias, rule, pricingMode string) (refs []string, usesAI bool, warning string, err error) {
	base, _ := StripThinkingSuffix(alias)
	if alias == "" || base != alias {
		return nil, false, "", errors.New("service: 别名不能为空且不能携带思考强度后缀")
	}
	if len(alias) > 128 {
		return nil, false, "", errors.New("service: 别名长度不能超过 128")
	}
	if strings.Contains(alias, "/") {
		return nil, false, "", errors.New(`service: 别名不能包含 "/"`)
	}
	prog, cerr := routelang.Compile(rule)
	if cerr != nil {
		return nil, false, "", cerr
	}
	refs = prog.ReferencedModels()
	usesAI = prog.UsesAI()
	rows, derr := s.st.ListModelRoutes(ctx)
	if derr != nil {
		return nil, false, "", derr
	}
	for _, r := range rows {
		if r.ID == excludeID {
			continue
		}
		if strings.EqualFold(r.Alias, alias) {
			return nil, false, "", fmt.Errorf("service: 别名 %q 已存在（id=%d）", r.Alias, r.ID)
		}
	}
	if usesAI {
		cfg, jerr := s.judgeSettingsCached(ctx)
		if jerr != nil || cfg.Model == "" {
			return nil, false, "", errors.New("service: 规则使用了 ai_judge，请先在「AI 评判设置」中配置评判模型")
		}
	}
	refSet := make(map[string]bool, len(refs))
	for _, x := range refs {
		refSet[strings.ToLower(x)] = true
	}
	if refSet[strings.ToLower(alias)] {
		return nil, false, "", fmt.Errorf("service: 规则引用了自身别名 %q（禁止自引用），请直接引用目标模型", alias)
	}
	for _, r := range rows {
		if !r.Enabled || r.ID == excludeID {
			continue
		}
		if refSet[strings.ToLower(r.Alias)] {
			return nil, false, "", fmt.Errorf("service: 引用的 %q 本身是另一个启用的集合别名（禁止嵌套路由），如需复用请直接引用其目标模型", r.Alias)
		}
	}
	if pricingMode == "alias" {
		if _, priced, perr := s.matchPricing(ctx, alias); perr == nil && !priced {
			warning = "计价模式为 alias，但别名没有匹配的计价规则，结算将退回预占估算金额"
		}
	}
	return refs, usesAI, warning, nil
}

// JudgeSettings 是评判模型的全局设置（preferences KV 持久化）。
type JudgeSettings struct {
	Model     string `json:"model"`
	TimeoutMS int64  `json:"timeout_ms"`
}

// GetJudgeSettings 读取评判设置（30s 内存缓存），供管理端点展示。
func (s *Service) GetJudgeSettings(ctx context.Context) (JudgeSettings, error) {
	cfg, err := s.judgeSettingsCached(ctx)
	return JudgeSettings{Model: cfg.Model, TimeoutMS: cfg.TimeoutMS}, err
}

// SaveJudgeSettings 保存评判模型设置并失效内存缓存。
func (s *Service) SaveJudgeSettings(ctx context.Context, model string, timeoutMS int64) error {
	model = strings.TrimSpace(model)
	if timeoutMS <= 0 {
		timeoutMS = defaultJudgeTimeout.Milliseconds()
	}
	if timeoutMS < 500 || timeoutMS > 120000 {
		return errors.New("service: 超时须在 500~120000ms 之间")
	}
	kv := map[string]string{prefJudgeModel: model, prefJudgeTimeoutMS: strconv.FormatInt(timeoutMS, 10)}
	if err := s.st.SetPreferences(ctx, kv); err != nil {
		return err
	}
	s.flushJudgeConfig()
	return nil
}

type judgeSettings struct {
	Model     string
	TimeoutMS int64
}

// flushJudgeConfig 丢弃缓存的评判设置（保存路径调用）。
func (s *Service) flushJudgeConfig() {
	s.judgeCfgMu.Lock()
	s.judgeConfAt = time.Time{}
	s.judgeCfgMu.Unlock()
}

// judgeSettingsCached 读取评判设置（30s 内存缓存）。读取失败按未配置处理。
func (s *Service) judgeSettingsCached(ctx context.Context) (judgeSettings, error) {
	s.judgeCfgMu.Lock()
	if time.Since(s.judgeConfAt) < 30*time.Second {
		cfg := s.judgeConf
		s.judgeCfgMu.Unlock()
		return cfg, nil
	}
	s.judgeCfgMu.Unlock()

	cfg := judgeSettings{}
	if prefs, err := s.st.ListPreferences(ctx); err == nil {
		cfg.Model = strings.TrimSpace(prefs[prefJudgeModel])
		if ms, e := strconv.ParseInt(prefs[prefJudgeTimeoutMS], 10, 64); e == nil && ms > 0 {
			cfg.TimeoutMS = ms
		}
	} else {
		return judgeSettings{}, err
	}
	if cfg.TimeoutMS <= 0 {
		cfg.TimeoutMS = defaultJudgeTimeout.Milliseconds()
	}
	s.judgeCfgMu.Lock()
	s.judgeConf = cfg
	s.judgeConfAt = time.Now()
	s.judgeCfgMu.Unlock()
	return cfg, nil
}

// SetJudgeExecutor 注入宿主非流式调用实现（main.configure 注入，跨越 C ABI 边界；
// 服务层不直接持有宿主回调）。fn 收到完整 OpenAI chat-completions 请求体，
// 返回上游原始响应体。
func (s *Service) SetJudgeExecutor(fn func(ctx context.Context, model string, body []byte) ([]byte, int, error)) {
	s.judgeExec.Store(&fn)
}

// ---------- ai_judge ----------

// aiJudge 是注入 routelang.Env 的 AI 实现：缓存命中直返，否则经 single-flight
// 调用评判模型。key = judge_model + 变量快照 + options（不含摘要文本——同一
// 变量组合的分级结论视为可复用）。
func (s *Service) aiJudge(ctx context.Context, vars map[string]any, digest string, options []string) (string, error) {
	cfg, err := s.judgeSettingsCached(ctx)
	if err != nil {
		return "", fmt.Errorf("读取评判设置失败: %w", err)
	}
	if cfg.Model == "" {
		return "", errors.New("未配置评判模型（AI 评判设置），无法执行 ai_judge")
	}
	canonical, err := json.Marshal(vars)
	if err != nil {
		return "", fmt.Errorf("序列化变量失败: %w", err)
	}
	sum := sha256.Sum256(append(append([]byte(cfg.Model), 0), append(canonical, append([]byte{0}, []byte(strings.Join(options, "\x00"))...)...)...))
	key := hex.EncodeToString(sum[:])

	if v, ok := s.judgeLRU.get(key); ok {
		return v, nil
	}
	// single-flight：同 key 并发合并为一次评判调用。
	s.judgeFlMu.Lock()
	if fl, ok := s.judgeFlights[key]; ok {
		s.judgeFlMu.Unlock()
		select {
		case <-fl.done:
			return fl.val, fl.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	fl := &judgeFlight{done: make(chan struct{})}
	s.judgeFlights[key] = fl
	s.judgeFlMu.Unlock()

	val, err := s.callJudge(ctx, cfg, vars, digest, options)
	fl.val, fl.err = val, err
	close(fl.done)
	s.judgeFlMu.Lock()
	delete(s.judgeFlights, key)
	s.judgeFlMu.Unlock()
	if err == nil {
		s.judgeLRU.put(key, val)
	}
	return val, err
}

type judgeFlight struct {
	done chan struct{}
	val  string
	err  error
}

func (s *Service) callJudge(ctx context.Context, cfg judgeSettings, vars map[string]any, digest string, options []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMS)*time.Millisecond)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"model":  cfg.Model,
		"stream": false,
		"messages": []map[string]string{{
			"role":    "user",
			"content": buildJudgePrompt(varsString(vars, "model"), varsString(vars, "source"), varsBool(vars, "stream"), varsInt(vars, "input_tokens"), varsInt(vars, "body_len"), varsString(vars, "thinking_effort"), digest, options),
		}},
	})
	if err != nil {
		return "", err
	}
	fn := s.judgeExec.Load()
	if fn == nil || *fn == nil {
		return "", errors.New("评判执行器不可用（宿主尚未注入）")
	}
	resp, status, err := (*fn)(cctx, cfg.Model, body)
	if err != nil {
		return "", fmt.Errorf("评判调用失败(status=%d): %w", status, err)
	}
	text := extractResponseText(resp)
	opt, ok := pickOption(text, options)
	if !ok {
		return "", fmt.Errorf("评判输出不在候选内: %q", truncateForLog(text))
	}
	return opt, nil
}

const judgePromptTemplate = `你是 API 请求分级器。阅读下面的请求摘要，从候选标签中选出最贴切的一个。
只输出该标签本身：不要解释、不要引号、不要输出候选之外的任何内容。

## 请求指标
- 请求模型: %s
- 来源格式: %s
- 是否流式: %t
- 输入 token 估算: %d
- 请求体字节数: %d
- 推理强度: %s

## 对话内容摘录（可能截断）
%s

## 候选标签
%v`

func buildJudgePrompt(model, source string, stream bool, inTokens, bodyLen int64, effort, digest string, options []string) string {
	if effort == "" {
		effort = "(缺省)"
	}
	return fmt.Sprintf(judgePromptTemplate, model, source, stream, inTokens, bodyLen, effort, digest, strings.Join(options, ", "))
}

// RequestDigest 从请求体尽力提取对话文本：messages/contents/system/input 容器内
// content/text 字段（含数组形态），总长封顶 2000 字符。绝不返回原始 body 全文。
func RequestDigest(body []byte) string {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	var b strings.Builder
	harvestTexts(&b, root, 2000)
	return b.String()
}

// harvestTexts 深度优先采集 content/text 字符串；map 键排序保证确定性顺序。
func harvestTexts(b *strings.Builder, v any, limit int) {
	takeString := func(s string) bool {
		if b.Len() >= limit {
			return true
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if remain := limit - b.Len(); len(s) > remain {
			b.WriteString(s[:remain])
		} else {
			b.WriteString(s)
		}
		return b.Len() >= limit
	}
	var walk func(v any, depth int) bool // 返回 true 表示预算耗尽
	walk = func(v any, depth int) bool {
		if depth > 8 {
			return false
		}
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, prio := range []string{"content", "text"} {
				if vv, ok := t[prio]; ok {
					if walk(vv, depth+1) {
						return true
					}
				}
			}
			for _, k := range keys {
				if k == "content" || k == "text" || k == "role" || k == "type" {
					continue
				}
				if walk(t[k], depth+1) {
					return true
				}
			}
		case []any:
			for _, item := range t {
				if walk(item, depth+1) {
					return true
				}
			}
		case string:
			return takeString(t)
		}
		return false
	}
	walk(v, 0)
}

// extractResponseText 从上游响应体提取评判模型的文本输出：
// 先探测常见结构（choices/candidates/output），失败则通用采集。
func extractResponseText(body []byte) string {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	if choices, ok := root["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if msg, ok := c["message"].(map[string]any); ok {
				if s, ok := msg["content"].(string); ok {
					return s
				}
			}
			if txt, ok := c["text"].(string); ok {
				return txt
			}
		}
	}
	if cands, ok := root["candidates"].([]any); ok && len(cands) > 0 {
		if c, ok := cands[0].(map[string]any); ok {
			var b strings.Builder
			harvestTexts(&b, c["content"], 4000)
			return b.String()
		}
	}
	var b strings.Builder
	harvestTexts(&b, root, 4000)
	return b.String()
}

// pickOption 把评判输出对齐到候选标签：精确→忽略大小写→唯一子串命中。
func pickOption(text string, options []string) (string, bool) {
	t := strings.Trim(strings.TrimSpace(text), "\"'`“”‘’")
	if t == "" {
		return "", false
	}
	for _, o := range options {
		if t == o {
			return o, true
		}
	}
	for _, o := range options {
		if strings.EqualFold(t, o) {
			return o, true
		}
	}
	lower := strings.ToLower(t)
	hit, hits := "", 0
	for _, o := range options {
		if strings.Contains(lower, strings.ToLower(o)) {
			hits++
			hit = o
		}
	}
	if hits == 1 {
		return hit, true
	}
	return "", false
}

func truncateForLog(s string) string {
	const max = 120
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func varsString(vars map[string]any, key string) string {
	v, _ := vars[key].(string)
	return v
}
func varsBool(vars map[string]any, key string) bool {
	v, _ := vars[key].(bool)
	return v
}
func varsInt(vars map[string]any, key string) int64 {
	switch v := vars[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// ---------- LRU 缓存 ----------

type lruItem struct {
	key string
	val string
	exp time.Time
}

// judgeLRU 是定容 LRU：put 淘汰最旧项；get 惰性剔除过期项。
type judgeLRU struct {
	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
}

func newJudgeLRU(max int) *judgeLRU {
	return &judgeLRU{ll: list.New(), items: make(map[string]*list.Element, max)}
}

func (c *judgeLRU) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	it := el.Value.(*lruItem)
	if time.Now().After(it.exp) {
		c.ll.Remove(el)
		delete(c.items, key)
		return "", false
	}
	c.ll.MoveToFront(el)
	return it.val, true
}

func (c *judgeLRU) put(key, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		it := el.Value.(*lruItem)
		it.val, it.exp = val, time.Now().Add(judgeCacheTTL)
		c.ll.MoveToFront(el)
		return
	}
	if c.ll.Len() >= judgeCacheMax {
		if back := c.ll.Back(); back != nil {
			c.ll.Remove(back)
			delete(c.items, back.Value.(*lruItem).key)
		}
	}
	el := c.ll.PushFront(&lruItem{key: key, val: val, exp: time.Now().Add(judgeCacheTTL)})
	c.items[key] = el
}
