// 模型路由的执行器侧实现：候选链 failover 循环、请求体改写与失败判定。
// 服务层（internal/service/routes.go）负责别名匹配与链求值；这里只做转发编排。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

// routedExecution 是一次命中集合别名的执行上下文：预占与认领在循环外
// 建立一次，多个候选目标共用；中间失败的尝试只记审计，不入 requests 行。
type routedExecution struct {
	svc         *service.Service
	match       service.RouteMatch
	chain       []string
	fellBack    bool
	plan        service.ReservePlan
	reservation store.Reservation
	claim       *usageClaim
	// failoverTrail 收集本次请求的目标转移轨迹（"tgt→next(原因)"），
	// 随最终结算行写入 error_note——中间尝试不入 requests 行，这里是唯一
	// 的故障轨迹；此前落 audit_events（请求路径高频写、表却长期保留），已退役。
	failoverTrail []string
}

// routeFailure 是路由路径上预占/求值阶段的结构化失败：非流式入口转
// RPC 信封，流式入口转 Go 错误（经 closeStream 送达客户端）。
type routeFailure struct {
	code    string
	message string
}

// resolveRouting 在请求命中集合别名时完成 链求值 → 计价计划 → 预占 → 认领。
// 返回值：(nil, nil) 未命中别名，调用方走直连老路；(re, nil) 已就绪；
// (_, failure) 路由流量被拒（全冷却 / 校验拒绝 / 预占被拒）。
//
// 请求体只解析一次（与直连路径同基线）：ParseRequestMeta 的结果贯穿
// 环境变量与预占计划；摘要文本仅在规则含 ai_judge 时惰性构建。
func resolveRouting(ctx context.Context, svc *service.Service, key *store.PluginKey, req rpcExecutorRequest, request []byte, stream bool) (*routedExecution, *routeFailure) {
	match, hit := svc.MatchRoute(ctx, req.Model)
	if !hit {
		return nil, nil
	}
	baseAlias, _ := service.StripThinkingSuffix(req.Model)
	meta := service.ParseRequestMeta(request)
	env := svc.BuildRouteEnv(meta, req.Model, stream, req.SourceFormat, key)
	var digestFn func() (string, error)
	if match.Route.Prog.UsesAI() {
		digestFn = sync.OnceValues(func() (string, error) { return service.RequestDigest(request), nil })
	}
	// 评判子调用归属到触发请求的插件 Key：宿主随后的被动用量回调据此
	// 改记密钥，不再以无主「-」行出现在明细里。
	attr := &service.JudgeAttribution{KID: key.KID, CallerID: key.CallerID}
	chain, fellBack, cerr := svc.ResolveChain(ctx, &match, env, digestFn, attr)
	// cerr 非空但 chain 也非空：ai_judge 失败已回落兜底分支（错误文本经
	// match.AIFallbackErr 随结算行落 error_note），请求本身不受阻，
	// 按 DESIGN §12.2 继续执行。
	if cerr != nil && chain == nil {
		if errors.Is(cerr, service.ErrAllTargetsCooling) {
			return nil, &routeFailure{"upstream_error", "模型集合 " + baseAlias + " 的候选目标全部冷却中，请稍后重试"}
		}
		return nil, &routeFailure{"upstream_error", "路由规则求值失败: " + cerr.Error()}
	}
	re := &routedExecution{svc: svc, match: match, chain: chain, fellBack: fellBack}

	pricingName := baseAlias
	if match.Route.PricingMode == "target" {
		pricingName = chain[0]
	}
	plan, err := svc.BuildReservePlanFromMeta(ctx, baseAlias, meta, pricingName)
	if err != nil {
		if errors.Is(err, service.ErrModelDisabled) {
			return nil, &routeFailure{"model_disabled", err.Error()}
		}
		return nil, &routeFailure{"reserve_rejected", err.Error()}
	}
	re.plan = plan
	resReq := service.ReservationRequest{
		KeyID: key.KID, CallerID: key.CallerID, Model: plan.Model,
		EstimatedTokens: plan.TokenEstimate, EstimatedImages: plan.ImageCount, Actor: "quota",
	}
	if plan.Priced && match.Route.PricingMode == "target" {
		override := plan.Rule
		resReq.PricingOverride = &override
	}
	reservation, err := svc.Reserve(ctx, resReq)
	if err != nil {
		if errors.Is(err, service.ErrModelNotAllowed) {
			return nil, &routeFailure{"model_not_allowed", err.Error()}
		}
		return nil, &routeFailure{"limit_rejected", err.Error()}
	}
	re.reservation = reservation

	// 认领集 = 全部引用目标的裸名与带后缀形态（宿主上报的是实际执行的目标名）。
	// 斜杠别名不登记别名形态：认领桶按裸名归一（去渠道前缀），「grp/auto」会落进
	// 裸名 auto 的桶，与同 Key 对真实模型 auto 的直连流量互相误吞；而别名请求的
	// 上报匹配实际依赖 refs，别名形态只是防御性冗余。撞真实命名已在保存期拦截。
	models := make([]string, 0, len(match.Route.Refs)*2+3)
	if !strings.Contains(match.Route.Alias, "/") {
		models = append(models, plan.Model, req.Model, match.Route.Alias)
	}
	for _, ref := range match.Route.Refs {
		models = append(models, ref)
		if sfx := match.Suffix; sfx != "" && !service.HasThinkingSuffix(ref) {
			models = append(models, ref+sfx)
		}
	}
	re.claim = registerUsageClaim(key.KID, models...)
	return re, nil
}

// targetWithSuffix 应用思考后缀规则：目标自带后缀则用目标的，否则附加原后缀。
func targetWithSuffix(target, suffix string) string {
	if suffix == "" || service.HasThinkingSuffix(target) {
		return target
	}
	return target + suffix
}

// bareTargetName 剥离候选目标的「渠道/」前缀。上游落库口径是真实上游模型
// 名：响应嗅探失败时回退到裸名，与非流式嗅探结果、宿主直报的裸名同构，
// 上游路由聚合不再因前缀裂成两行（orcarouter/x 与 x 是同一上游模型）。
func bareTargetName(target string) string {
	if i := strings.LastIndex(target, "/"); i >= 0 && i+1 < len(target) {
		return target[i+1:]
	}
	return target
}

// ---------- model.register（集合别名单独进 /v1/models）----------

// rpcRegisteredModel 字段与宿主 ModelInfo 对齐（PascalCase、无 JSON tag）：
// ID/Object/OwnedBy/DisplayName/Name/Description/UserDefined。
type rpcRegisteredModel struct {
	ID          string
	Object      string
	OwnedBy     string
	DisplayName string
	Name        string
	Description string
	UserDefined bool
}

type rpcModelRegisterResponse struct {
	Provider string               `json:"provider"`
	Models   []rpcRegisteredModel `json:"models"`
}

// modelRegistrarEnabled 已删除：model_registrar 能力位改为恒声明
// （quota.enabled 时），避免「宿主启动时无路由 → 能力位 false → 建路由后
// 必须等一次 reconfigure」的坑；model.register 对空列表返回空 models，
// 宿主 RegisterModels 自动跳过。

func modelRegister(svc *service.Service) ([]byte, error) {
	resp := rpcModelRegisterResponse{Provider: config.PluginID, Models: []rpcRegisteredModel{}}
	if svc == nil {
		return okEnvelope(resp)
	}
	rows, err := svc.ListRoutesCompiled(context.Background())
	if err != nil {
		return okEnvelope(resp)
	}
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		resp.Models = append(resp.Models, rpcRegisteredModel{
			ID:          r.Alias,
			Object:      "model",
			OwnedBy:     "cpa-usage-manager",
			DisplayName: r.Alias,
			Name:        r.Alias,
			Description: fmt.Sprintf("集合别名 · %d 个目标", len(r.Refs)),
			UserDefined: true,
		})
	}
	return okEnvelope(resp)
}

// bodyRewriter 把「格式判定 + 整包解析」提到候选循环外：解析一次，
// 每个候选只改 model 字段后重新序列化。failover N 个目标从 N 次往返
// 降为 1 次解析 + N 次序列化；非 JSON 体原样透传（ok=false）。
type bodyRewriter struct {
	raw       []byte
	stream    bool
	openaiFam bool
	ok        bool
	m         map[string]any
}

func newBodyRewriter(raw []byte, sourceFormat, outputFormat string, stream bool) *bodyRewriter {
	b := &bodyRewriter{raw: raw, stream: stream}
	format := strings.ToLower(service.FirstNonEmpty(outputFormat, sourceFormat))
	b.openaiFam = !strings.Contains(format, "claude") && !strings.Contains(format, "gemini")
	if len(raw) == 0 || json.Unmarshal(raw, &b.m) != nil {
		return b
	}
	b.ok = true
	return b
}

// build 返回目标为 targetModel 的请求体（共享底层 map，按候选逐次改写）。
func (b *bodyRewriter) build(targetModel string) []byte {
	if b == nil || !b.ok {
		return b.raw
	}
	b.m["model"] = targetModel
	if b.stream && b.openaiFam {
		options, ok := b.m["stream_options"].(map[string]any)
		if !ok {
			options = make(map[string]any)
			b.m["stream_options"] = options
		}
		if _, exists := options["include_usage"]; !exists {
			options["include_usage"] = true
		}
	}
	updated, err := json.Marshal(b.m)
	if err != nil {
		return b.raw
	}
	return updated
}

// preparePayload 单发场景的便捷封装。
func preparePayload(body []byte, sourceFormat, outputFormat, targetModel string, stream bool) []byte {
	return newBodyRewriter(body, sourceFormat, outputFormat, stream).build(targetModel)
}

// routeFailureEligible 判定一次尝试失败是否值得转移到下一候选。
//
// 可转：401/402/403/408/429、5xx、连接类传输错误；404 除 Responses 存储
// 类文本（previous_response_id 引用不存在）外可转。不可转：400/422、
// 取消/超时、其余无状态码错误。
func routeFailureEligible(status int, errText string, err error) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
	}
	text := strings.ToLower(errText)
	for _, frag := range []string{
		"rate limit", "quota exceeded", "unauthorized", "forbidden",
		"connection reset", "broken pipe", "no such host",
		"bad gateway", "service unavailable", "gateway timeout", "overloaded",
		"stream disconnected", "eof",
	} {
		if strings.Contains(text, frag) {
			return true
		}
	}
	switch {
	case status == 404:
		// Responses 存储引用类 404 换目标无意义。
		return !strings.Contains(text, "previous response") &&
			!strings.Contains(text, "response not found") &&
			!strings.Contains(text, "resp_")
	case status == 401, status == 402, status == 403, status == 408, status == 429:
		return true
	case status >= 500 && status < 600:
		return true
	}
	return false
}

// cooldownSecondsFor 返回本次失败应记的冷却时长：429 且上游带了可解析的
// Retry-After（秒数或 HTTP 日期）时采用之（钳到 1s~10min，防异常大值把
// 目标永久摘除），否则用路由配置的 cooldown_seconds。
func cooldownSecondsFor(defaultSec int64, status int, headers http.Header) int64 {
	if status != 429 || len(headers) == 0 {
		return defaultSec
	}
	ra := strings.TrimSpace(headers.Get("Retry-After"))
	if ra == "" {
		return defaultSec
	}
	var until time.Time
	if secs, err := strconv.ParseInt(ra, 10, 64); err == nil {
		until = time.Now().Add(time.Duration(secs) * time.Second)
	} else if t, err := http.ParseTime(ra); err == nil {
		until = t
	} else {
		return defaultSec
	}
	d := time.Until(until)
	if d < time.Second {
		d = time.Second
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return int64(d.Seconds())
}

// appendFailoverTrail 记录一次目标转移轨迹（中间尝试不入 requests 行，
// 轨迹随最终结算行落 error_note；route.failover 审计已退役——请求路径的
// 高频写入不应进长期保留的 audit_events 表）。
func (re *routedExecution) appendFailoverTrail(from, to string, status int, cause string) {
	if len(cause) > 60 {
		cause = cause[:60]
	}
	if cause == "" {
		cause = "status_" + strconv.Itoa(status)
	}
	if len(re.failoverTrail) < 4 { // 轨迹上限：超长链只留前 4 跳
		re.failoverTrail = append(re.failoverTrail, from+"→"+to+"("+cause+")")
	}
}

// failoverNote 汇总本次请求的异常轨迹为 error_note 文本：ai_judge 回落
// 事实（若有）+ 目标转移链。无任何异常时返回空串。
func (re *routedExecution) failoverNote() string {
	parts := make([]string, 0, len(re.failoverTrail)+1)
	if msg := re.match.AIFallbackErr; msg != "" {
		if len(msg) > 80 {
			msg = msg[:80]
		}
		parts = append(parts, "ai_judge 回落: "+msg)
	}
	parts = append(parts, re.failoverTrail...)
	return strings.Join(parts, "; ")
}

func routeFailureCause(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return "status_" + strconv.Itoa(status)
}

// executeRoutedLoop 非流式 failover 主循环。成功或终局失败都结算一次并落单行；
// 可转移失败冷却当前目标后换下一个。
func executeRoutedLoop(ctx context.Context, re *routedExecution, req rpcExecutorRequest, request []byte, startedAt time.Time) ([]byte, error) {
	svc := re.svc
	route := re.match.Route
	rw := newBodyRewriter(request, req.SourceFormat, req.Format, false)
	for i, tgt := range re.chain {
		finalTgt := targetWithSuffix(tgt, re.match.Suffix)
		payload := rw.build(finalTgt)
		hostBody, headers, status, errHost := hostModelExecute(req.HostCallbackID, req, finalTgt, payload, false)
		upstream := service.FirstNonEmpty(usageparse.SniffModel(hostBody), bareTargetName(finalTgt))
		if errHost == nil && status < 400 {
			svc.MarkRouteSuccess(route.ID, tgt)
			parsed, _ := usageparse.Parse(hostBody)
			settleReservation(svc, re.plan, re.reservation, req, startedAt, time.Time{}, time.Now(), status, parsed, upstream, re.failoverNote(), re.claim)
			return okEnvelope(rpcExecutorResponse{Payload: hostBody, Headers: headers})
		}
		if i < len(re.chain)-1 && routeFailureEligible(status, routeFailureCause(status, errHost), errHost) {
			svc.MarkRouteFail(route.ID, tgt, cooldownSecondsFor(route.CooldownSeconds, status, headers))
			re.appendFailoverTrail(tgt, re.chain[i+1], status, routeFailureCause(status, errHost))
			continue
		}
		// 终局：沿用直连语义——传输层失败不落行释放预占；HTTP 错误体透传并落行。
		if errHost != nil {
			re.claim.release(0)
			_, _ = svc.Release(ctx, re.reservation.ID)
			return errorEnvelope("upstream_error", errHost.Error()), nil
		}
		parsed, _ := usageparse.Parse(hostBody)
		settleReservation(svc, re.plan, re.reservation, req, startedAt, time.Time{}, time.Now(), status, parsed, upstream, re.failoverNote(), re.claim)
		return okEnvelope(rpcExecutorResponse{Payload: hostBody, Headers: headers})
	}
	// 不可达：ResolveChain 保证链非空，末次迭代必为终局分支。
	return errorEnvelope("upstream_error", "路由候选链为空"), nil
}

// dialOutcome 是单候选流式拨号的结果。
type dialOutcome int

const (
	dialOK       dialOutcome = iota // 流已建立（StatusCode<400 且 StreamID 就绪）
	dialTransfer                    // 可转移失败：已冷却标记+审计，换下一候选
	dialFailed                      // 终局失败：调用方按既有语义收尾，err 为原因
)

// dialHostStream 对单个候选目标发起流式拨号。首字节尚未产出，这里是
// 流式路径唯一可切换目标的窗口。rw 由调用方在循环外解析一次。
func dialHostStream(re *routedExecution, req rpcExecutorRequest, rw *bodyRewriter, index int) (rpcHostModelStreamResponse, dialOutcome, error) {
	tgt := re.chain[index]
	finalTgt := targetWithSuffix(tgt, re.match.Suffix)
	payload := rw.build(finalTgt)
	raw, err := hostCall("host.model.execute_stream", rpcHostModelExecutionRequest{
		EntryProtocol:  service.FirstNonEmpty(req.SourceFormat, "openai"),
		ExitProtocol:   service.FirstNonEmpty(req.Format, req.SourceFormat, "openai"),
		Model:          finalTgt,
		Stream:         true,
		Body:           payload,
		Headers:        req.Headers,
		Query:          req.Query,
		Alt:            req.Alt,
		HostCallbackID: req.HostCallbackID,
	})
	var stream rpcHostModelStreamResponse
	if err == nil {
		err = json.Unmarshal(raw, &stream)
	}
	// 空 StreamID 只在「成功状态码」下才是协议异常；HTTP 错误响应本就没有流。
	if err == nil && stream.StatusCode < 400 && stream.StreamID == "" {
		err = errors.New("empty host stream id")
	}
	if err != nil {
		if index < len(re.chain)-1 && routeFailureEligible(0, err.Error(), err) {
			re.svc.MarkRouteFail(re.match.Route.ID, tgt, re.match.Route.CooldownSeconds)
			re.appendFailoverTrail(tgt, re.chain[index+1], 0, err.Error())
			return stream, dialTransfer, nil
		}
		return stream, dialFailed, err
	}
	if stream.StatusCode >= 400 {
		_ = closeHostModelStream(stream.StreamID)
		if index < len(re.chain)-1 && routeFailureEligible(stream.StatusCode, "", nil) {
			re.svc.MarkRouteFail(re.match.Route.ID, tgt, cooldownSecondsFor(re.match.Route.CooldownSeconds, stream.StatusCode, stream.Headers))
			re.appendFailoverTrail(tgt, re.chain[index+1], stream.StatusCode, "")
			return rpcHostModelStreamResponse{}, dialTransfer, nil
		}
		return stream, dialFailed, errHostStatus(stream.StatusCode)
	}
	// 流建立即成功：上游已 2xx 受理，清除可能残留的冷却，不必等流结束。
	re.svc.MarkRouteSuccess(re.match.Route.ID, tgt)
	return stream, dialOK, nil
}

type errHostStatus int

func (e errHostStatus) Error() string { return "host model status " + strconv.Itoa(int(e)) }

// pumpRoutedStream 是流建立后的读泵循环：逐块增量解析用量并向客户端发射。
// 首字节已出后不再切换目标；所有收尾分支以 finalTgt 兜底 UpstreamModel。
func pumpRoutedStream(re *routedExecution, req rpcExecutorRequest, startedAt time.Time, stream rpcHostModelStreamResponse, finalTgt, pluginStreamID string, closeStream func(string)) error {
	svc := re.svc
	defer func() { _ = closeHostModelStream(stream.StreamID) }()
	// 嗅探失败的回退名剥渠道前缀（见 bareTargetName）。
	upstreamFallback := bareTargetName(finalTgt)
	acc := &usageparse.Accumulator{}
	var firstChunkAt, completedAt time.Time
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	stopWatch := startStreamIdleWatchdog(stream.StreamID, &lastProgress)
	defer stopWatch()
	for {
		chunkRaw, errRead := hostCall("host.model.stream_read", rpcHostModelStreamReadRequest{StreamID: stream.StreamID})
		if errRead != nil {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, re.plan, re.reservation, req, startedAt, firstChunkAt, completedAt, 502, parsed, service.FirstNonEmpty(acc.Model(), upstreamFallback), errRead.Error(), re.claim)
			return errRead
		}
		var chunk rpcHostModelStreamReadResponse
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, re.plan, re.reservation, req, startedAt, firstChunkAt, completedAt, 502, parsed, service.FirstNonEmpty(acc.Model(), upstreamFallback), err.Error(), re.claim)
			return err
		}
		if chunk.Error != "" {
			completedAt = time.Now()
			parsed, _ := acc.Result()
			settleReservation(svc, re.plan, re.reservation, req, startedAt, firstChunkAt, completedAt, 502, parsed, service.FirstNonEmpty(acc.Model(), upstreamFallback), chunk.Error, re.claim)
			return errors.New(chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			acc.FeedChunk(chunk.Payload)
			lastProgress.Store(time.Now().UnixNano())
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
			}
			if err := emitPluginStreamChunk(pluginStreamID, chunk.Payload); err != nil {
				completedAt = time.Now()
				parsed, _ := acc.Result()
				settleReservation(svc, re.plan, re.reservation, req, startedAt, firstChunkAt, completedAt, 499, parsed, service.FirstNonEmpty(acc.Model(), upstreamFallback), err.Error(), re.claim)
				return err
			}
		}
		if chunk.Done {
			break
		}
	}
	completedAt = time.Now()
	parsed, _ := acc.Result()
	closeStream("")
	if parsed.IsZero() {
		if rec, ok := re.claim.wait(svc.Config().Quota.Settlement.HostUsageWait.Std()); ok {
			parsed = usageFromRecord(rec)
			r := buildRequest(svc, re.reservation, req, re.plan.Meta, startedAt, firstChunkAt, completedAt, 200)
			r.UpstreamModel = service.FirstNonEmpty(acc.Model(), upstreamFallback)
			applyHostUsageToRequest(r, rec)
			return finishSettle(svc, re.reservation, r, parsed, re.claim)
		}
	}
	r := buildRequest(svc, re.reservation, req, re.plan.Meta, startedAt, firstChunkAt, completedAt, 200)
	r.UpstreamModel = service.FirstNonEmpty(acc.Model(), upstreamFallback)
	return finishSettle(svc, re.reservation, r, parsed, re.claim)
}
