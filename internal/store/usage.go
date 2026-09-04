package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

// 用量写入的**唯一路径**。
//
// 锁定决策：一次入库同时产生逐请求记录与分钟聚合，不存在「双写两个子系统」。
// 额度结算路径（Settle）与被动统计路径（RecordUsage）都收敛到
// insertRequestTx + upsertRollupTx 这一对函数上。

// RecordUsage 记录一条与预占无关的用量（quota.enabled=false 的被动统计路径）。
func (s *Store) RecordUsage(ctx context.Context, r Request) error {
	if r.ID == "" {
		return fmt.Errorf("请求记录缺少 id")
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		err := insertRequestTx(ctx, s, tx, r)
		if errors.Is(err, errDuplicateRequest) {
			// 宿主重复回调同一请求：已入库，聚合不再累加，整体视为成功。
			return nil
		}
		if err != nil {
			return err
		}
		return upsertRollupTx(ctx, s, tx, r)
	})
}

// insertRequestTx 写入一条逐请求记录。
// 同 ID 重复写入按幂等处理（忽略后写），避免宿主重复回调造成双计。
func insertRequestTx(ctx context.Context, s *Store, tx *sql.Tx, r Request) error {
	if r.Result == "" {
		r.Result = ResultOK
	}
	// 写入收口清洗：requests 表唯一的 INSERT 点，任何调用方带来的
	// 上游凭据（宿主会把 api_key 填进 Source）都到不了库。
	r.Source = RedactSource(r.Source)
	r.ErrorNote = SanitizeErrorNote(r.ErrorNote)
	res, err := s.execHotTx(ctx, tx,
		`INSERT INTO requests (
			id, ts, key_id, caller_id, model, provider, source, upstream_model,
			auth_id, auth_label, auth_type, tier, result,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			cache_read_tokens, cache_creation_tokens, total_tokens,
			latency_ms, ttft_ms, generation_ms, tps_milli,
			thinking_intensity, cost_micro_usd, currency, cost_native_micro, priced, reservation_id,
			status_code, error_note
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		r.ID, r.TS.UTC().UnixMilli(), r.KeyID, r.CallerID, r.Model, r.Provider, r.Source, r.UpstreamModel,
		r.AuthID, r.AuthLabel, r.AuthType, r.Tier, r.Result,
		r.InputTokens, r.OutputTokens, r.ReasoningTokens, r.CachedTokens,
		r.CacheReadTokens, r.CacheCreationTokens, r.TotalTokens,
		r.LatencyMS, r.TTFTMS, r.GenerationMS, r.TPSMilli,
		r.ThinkingIntensity, int64(r.CostMicroUSD), nativeCurrency(r), int64(r.CostNativeMicro), boolInt(r.Priced), r.ReservationID,
		r.StatusCode, r.ErrorNote)
	if err != nil {
		return fmt.Errorf("写入请求记录 %s 失败: %w", r.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 已存在同 ID 记录：视为重复回调，不再累加聚合。
		return errDuplicateRequest
	}
	return nil
}

// errDuplicateRequest 是内部哨兵，表示该请求已入库、聚合不应重复累加。
var errDuplicateRequest = fmt.Errorf("请求记录已存在")

// upsertRollupTx 把一条请求累加进分钟聚合。
func upsertRollupTx(ctx context.Context, s *Store, tx *sql.Tx, r Request) error {
	if r.Result == "" {
		r.Result = ResultOK
	}
	// rollup 的 source 是维度分组值，会经 /usage/dimension 原样外显，同口径清洗。
	r.Source = RedactSource(r.Source)
	bucket := BucketMinute(r.TS)
	fail := 0
	if r.Result != ResultOK {
		fail = 1
	}
	// TTFT 为 0 表示未观测到（非流式请求），不计入均值分母。
	ttftCount := 0
	if r.TTFTMS > 0 {
		ttftCount = 1
	}

	_, err := s.execHotTx(ctx, tx,
		`INSERT INTO usage_rollups (
			bucket_minute, model, key_id, caller_id, provider, source, auth_type, tier, result,
			req_count, fail_count,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			cache_read_tokens, cache_creation_tokens, total_tokens,
			latency_sum, ttft_sum, ttft_count, generation_sum, tps_milli_sum, cost_micro_usd
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket_minute, model, key_id, caller_id, provider, source, auth_type, tier, result)
		DO UPDATE SET
			req_count             = req_count + 1,
			fail_count            = fail_count + excluded.fail_count,
			input_tokens          = input_tokens + excluded.input_tokens,
			output_tokens         = output_tokens + excluded.output_tokens,
			reasoning_tokens      = reasoning_tokens + excluded.reasoning_tokens,
			cached_tokens         = cached_tokens + excluded.cached_tokens,
			cache_read_tokens     = cache_read_tokens + excluded.cache_read_tokens,
			cache_creation_tokens = cache_creation_tokens + excluded.cache_creation_tokens,
			total_tokens          = total_tokens + excluded.total_tokens,
			latency_sum           = latency_sum + excluded.latency_sum,
			ttft_sum              = ttft_sum + excluded.ttft_sum,
			ttft_count            = ttft_count + excluded.ttft_count,
			generation_sum        = generation_sum + excluded.generation_sum,
			tps_milli_sum         = tps_milli_sum + excluded.tps_milli_sum,
			cost_micro_usd        = cost_micro_usd + excluded.cost_micro_usd`,
		bucket, r.Model, r.KeyID, r.CallerID, r.Provider, r.Source, r.AuthType, r.Tier, r.Result,
		fail,
		r.InputTokens, r.OutputTokens, r.ReasoningTokens, r.CachedTokens,
		r.CacheReadTokens, r.CacheCreationTokens, r.TotalTokens,
		r.LatencyMS, r.TTFTMS, ttftCount, r.GenerationMS, r.TPSMilli, int64(r.CostMicroUSD))
	if err != nil {
		return fmt.Errorf("累加分钟聚合失败: %w", err)
	}
	return nil
}

// GetRequest 按 ID 读取逐请求记录。
func (s *Store) GetRequest(ctx context.Context, id string) (Request, error) {
	var r Request
	err := s.Read(ctx, func(q Querier) error {
		var e error
		r, e = scanRequest(q.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM requests WHERE id = ?`, id))
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, fmt.Errorf("%w: 请求 %q", ErrNotFound, id)
	}
	if err != nil {
		return Request{}, fmt.Errorf("读取请求 %q 失败: %w", id, err)
	}
	return r, nil
}

// DistinctObservedModels 返回历史请求中出现过的全部模型名（NOCASE 去重，
// 含 model 与 upstream_model 两列——路由流量的真实目标只出现在后者）。
// 供路由保存期「别名不得撞真实模型名」校验；低频管理操作，容忍全表扫描。
func (s *Store) DistinctObservedModels(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	var out []string
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT m FROM (
				 SELECT model AS m FROM requests WHERE model <> ''
				 UNION
				 SELECT upstream_model AS m FROM requests WHERE upstream_model IS NOT NULL AND upstream_model <> ''
			 ) GROUP BY m COLLATE NOCASE ORDER BY m LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			out = append(out, name)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("读取历史模型名失败: %w", err)
	}
	return out, nil
}

// 双写判重容差。宿主 usage.handle 与执行器结算描述同一次上游调用，但两侧的
// 时刻与延迟并不同源，直接比较窗口必然失配（以下均已对照宿主源码核实）：
//
//   - 宿主 RequestedAt 是请求开始时刻，执行器行 ts 也是 startedAt——两者本就
//     相差「宿主鉴权/翻译/选择目标」的入口开销，慢网关下可达数秒；
//   - 宿主 Latency 从 reporter 构造起量，含宿主全链路（重试、协议翻译）；
//     执行器 latency 只量 host.model.execute 调用本身。同一请求两侧延迟差
//     可远超 150ms——旧谓词按延迟±150ms 配对，慢请求直接漏判，重复行残留。
//
// 结构化解法：同一次请求的宿主观测必然满足——
//
//	执行器.ts ≤ 宿主.ts（宿主先收到请求才转给执行器）
//	宿主.ts ≤ 执行器.ts + 执行器延迟 + 宿主侧尾延迟（宿主最晚在拿到响应后记账）
//
// 用「区间包含」替代「双侧窗口对称比较」：宿主 ts 必须落在执行器请求的
// 起止区间内（前界放宽 dupTSWindow 容纳入口开销），并用「完成时刻对齐」
// （两侧 ts+latency 之差）代替裸延迟比较——两侧延迟都锚定同一响应到达，
// 完成时刻的差只剩记账时点差，与两侧延迟定义不同源无关。
const (
	dupTSWindow = 15 * time.Second
	// dupCompleteWindow 是两侧完成时刻（ts+latency）允许的偏差。
	dupCompleteWindow = int64(10_000) // 毫秒
	// dupHostTailWindow 是宿主完成时刻允许落后执行器完成时刻的附加余量：
	// 宿主 usage 回调走单 worker 后台队列，积压时记账可晚于响应完成很久。
	dupHostTailWindow = int64(30_000) // 毫秒
	// dupTokenTolerance 是配对时两侧 token 计数的允许偏差（见 tokenCountsClose）。
	dupTokenTolerance = int64(64)
)

// modelMatchFragment 生成「model IN (…) OR model LIKE '%/模型'」的匹配片段。
// 执行器落库常用「渠道/模型」别名，宿主上报原始模型，需按候选集 + 后缀双匹配。
func modelMatchFragment(models []string) (string, []any) {
	models = normalizeModels(models)
	parts := make([]string, 0, len(models)+1)
	args := make([]any, 0, len(models)*2)
	parts = append(parts, "model IN ("+placeholders(len(models))+")")
	for _, m := range models {
		args = append(args, m)
	}
	for _, m := range models {
		parts = append(parts, `model LIKE ? ESCAPE '\'`)
		args = append(args, "%/"+escapeLike(m))
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// normalizeModels 去空白、去空值、去重，保持原顺序。
func normalizeModels(models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		dup := false
		for _, v := range out {
			if v == m {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, m)
		}
	}
	return out
}

// escapeLike 转义 LIKE 通配符。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// BackfillRequestUsage 给 key+model 候选在 near 前后 dupTSWindow 内最近一条零用量记录
// 补写宿主 usage.handle 上报的 token 明细（执行器落库的模型可能是别名，
// 因此按候选列表匹配：原始模型 / 别名 / 「渠道/模型」后缀）。返回是否更新。
// 只补逐请求行，分钟聚合不回填（可接受的口径差异）。
func (s *Store) BackfillRequestUsage(ctx context.Context, kid string, models []string, near time.Time, b UsageBackfill) (bool, error) {
	if len(normalizeModels(models)) == 0 {
		return false, nil
	}
	frag, margs := modelMatchFragment(models)
	args := make([]any, 0, len(margs)+3)
	args = append(args, kid)
	args = append(args, margs...)
	args = append(args, near.Add(-dupTSWindow).UnixMilli(), near.Add(dupTSWindow).UnixMilli())
	var id string
	err := s.Write(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM requests
			 WHERE key_id = ? AND `+frag+`
			   AND input_tokens = 0 AND output_tokens = 0 AND total_tokens = 0
			   AND ts BETWEEN ? AND ?
			 ORDER BY ts DESC LIMIT 1`, args...)
		if e := row.Scan(&id); e != nil {
			return e
		}
		return backfillRequestTx(ctx, tx, s, id, b)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// FindDuplicateExecutor 按结构化区间谓词关联执行器已入库的记录（判重快路径）。
// 宿主 usage.handle 的 APIKey 字段在部分兼容渠道里是上游凭据而非插件 Key，
// 无法用 kid 判重。只读快路径：命中可省去 RecordPassiveUsage 的写事务；
// 未命中由其事务内探测兜底（防 TOCTOU）。
func (s *Store) FindDuplicateExecutor(ctx context.Context, models []string, near time.Time, latencyMS, totalTokens, inputTokens int64) (string, bool, error) {
	var (
		id    string
		found bool
	)
	err := s.Read(ctx, func(q Querier) error {
		twin, ok, err := duplicateProbeTx(ctx, q, models, near, latencyMS, totalTokens, inputTokens, true)
		if err != nil || !ok {
			return err
		}
		id, found = twin.ID, true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return id, found, nil
}

// BackfillRequestUsageByID 按 ID 回填宿主上报的用量明细（含首字延迟兜底）。
func (s *Store) BackfillRequestUsageByID(ctx context.Context, id string, b UsageBackfill) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		return backfillRequestTx(ctx, tx, s, id, b)
	})
}

// duplicateProbeTx 按双写判重谓词查找另一口径的重复行。
// executorRow=true 找执行器行（key_id<>”）供被动侧探测；false 找被动行
// （key_id=”）供结算侧探测。单写者串行化保证「事务内探测→插入」原子成立：
// 两个写入方必然一先一后提交，后者一定能看到前者已提交的行。
//
// 谓词（结构化区间版，见常量块注释）：同模型族 + 时间区间包含 + 完成时刻对齐
// + token 计数相容。候选行记 e（列 ts/latency_ms），本次观测记 h（参数组）：
//
//	h.ts >= e.ts - dupTSWindow                      宿主收到请求不早于执行器启动减入口开销
//	h.ts <= e.ts + e.latency_ms + dupHostTailWindow 宿主记账不晚于执行器完成加队列尾延迟
//	|h.ts + h.latency - (e.ts + e.latency_ms)| <= dupCompleteWindow 完成时刻对齐
//	h.total/h.input 与 e 侧计数差 <= dupTokenTolerance（任一侧为 0 时不约束：
//	结算行缺 token、宿主回调被裁剪都属正常）
func duplicateProbeTx(ctx context.Context, q Querier, models []string, near time.Time, latencyMS int64, hTokens, hInput int64, executorRow bool) (Request, bool, error) {
	models = normalizeModels(models)
	if len(models) == 0 {
		return Request{}, false, nil
	}
	frag, margs := modelMatchFragment(models)
	keyCond := `key_id = ''`
	if executorRow {
		keyCond = `key_id <> ''`
	}
	hTS := near.UnixMilli()
	var hLatency int64
	if latencyMS > 0 {
		hLatency = latencyMS
	}
	hDone := hTS + hLatency
	args := make([]any, 0, len(margs)+13)
	args = append(args, margs...)
	args = append(args,
		hTS+dupTSWindow.Milliseconds(),            // e.ts <= h.ts + W  ⇔ h.ts >= e.ts - W
		hTS-dupHostTailWindow,                     // e.ts + e.latency >= h.ts - T
		hDone-dupCompleteWindow,                   // e.ts + e.latency >= h.done - C
		hDone+dupCompleteWindow+dupHostTailWindow, // e.ts + e.latency <= h.done + C + T
		hTokens, hTokens, dupTokenTolerance, // total_tokens = 0 OR h = 0 OR |e-h| <= tol
		hInput, hInput, dupTokenTolerance, // input_tokens = 0 OR h = 0 OR |e-h| <= tol
		hTS)
	rows, err := q.QueryContext(ctx,
		`SELECT `+requestColumns+` FROM requests
		 WHERE `+keyCond+` AND `+frag+`
		   AND ts <= ?
		   AND ts + latency_ms >= ?
		   AND ts + latency_ms BETWEEN ? AND ?
		   AND (total_tokens = 0 OR ? = 0 OR ABS(total_tokens - ?) <= ?)
		   AND (input_tokens = 0 OR ? = 0 OR ABS(input_tokens - ?) <= ?)
		 ORDER BY ABS(ts - ?) LIMIT 8`, args...)
	if err != nil {
		return Request{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return Request{}, false, err
		}
		return r, true, nil
	}
	if err := rows.Err(); err != nil {
		return Request{}, false, err
	}
	return Request{}, false, nil
}

// PassiveDedupeHint 携带被动入库时的事务内双查参数。
type PassiveDedupeHint struct {
	// Models 是宿主上报的模型候选（别名/原始名/裸名）。
	Models []string
	// Near 与 LatencyMS 是宿主观测的请求时刻与延迟。
	Near      time.Time
	LatencyMS int64
	// TotalTokens 与 InputTokens 是宿主侧的 token 计数，供相容性过滤
	// （同请求两侧计数差仅可能来自口径归一；0 表示不约束）。
	TotalTokens int64
	InputTokens int64
}

// RecordPassiveUsage 是被动统计路径（宿主 usage.handle 兜底）的入库入口：
// 在同一写事务内先探测执行器行，命中则把本条字段并入该行（只补零值），
// 不再插入新行——双写重复在入库时即被消除，任何时刻都不可见重复行；
// 未命中才正常落库。
func (s *Store) RecordPassiveUsage(ctx context.Context, r Request, dup PassiveDedupeHint) error {
	if r.ID == "" {
		return fmt.Errorf("请求记录缺少 id")
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	if dup.Near.IsZero() {
		dup.Near = r.TS
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		twin, found, err := duplicateProbeTx(ctx, tx, dup.Models, dup.Near, dup.LatencyMS,
			dup.TotalTokens, dup.InputTokens, true)
		if err != nil {
			return err
		}
		if found {
			return absorbIntoExecutorTx(ctx, tx, s, twin, r)
		}
		err = insertRequestTx(ctx, s, tx, r)
		if errors.Is(err, errDuplicateRequest) {
			// 宿主重复回调同一请求：已入库，聚合不再累加，整体视为成功。
			return nil
		}
		if err != nil {
			return err
		}
		return upsertRollupTx(ctx, s, tx, r)
	})
}

// absorbIntoExecutorTx 把尚未落库的被动记录并入已存在的执行器行：
// 只补 keeper 的零值/空值字段并把这些增量回补到其分钟聚合。
// 被动行从未插入，因此无需删除行或扣减聚合；费用不参与合并
// （执行器行的结算金额已入 Key 账本，改写会破坏明细与账本一致）。
func absorbIntoExecutorTx(ctx context.Context, tx *sql.Tx, s *Store, keeper Request, drop Request) error {
	delta := UsageBackfill{
		InputTokens:         pickZero(keeper.InputTokens, drop.InputTokens),
		OutputTokens:        pickZero(keeper.OutputTokens, drop.OutputTokens),
		ReasoningTokens:     pickZero(keeper.ReasoningTokens, drop.ReasoningTokens),
		CachedTokens:        pickZero(keeper.CachedTokens, drop.CachedTokens),
		CacheReadTokens:     pickZero(keeper.CacheReadTokens, drop.CacheReadTokens),
		CacheCreationTokens: pickZero(keeper.CacheCreationTokens, drop.CacheCreationTokens),
		TotalTokens:         pickZero(keeper.TotalTokens, drop.TotalTokens),
	}
	if keeper.TTFTMS == 0 && drop.TTFTMS > 0 {
		delta.TTFTMS = drop.TTFTMS
	}
	if err := fillKeeperZeroTx(ctx, tx, s, drop, keeper.ID); err != nil {
		return err
	}
	return addRollupDeltaTx(ctx, tx, s, keeper, delta)
}

// mergeRequestPairTx 把 dropee 行并入 keeper 行：仅补 keeper 的零值/空值字段，
// 删除 dropee，并同步分钟聚合（扣减 dropee 的整行贡献 + 回补并入 keeper 的增量）。
//
// 费用不参与合并：执行器行的 cost_micro_usd 已按结算金额记入 Key 账本，
// 改写会造成明细与账本口径不一致。
func mergeRequestPairTx(ctx context.Context, tx *sql.Tx, s *Store, keeper, dropee Request) error {
	delta := UsageBackfill{
		InputTokens:         pickZero(keeper.InputTokens, dropee.InputTokens),
		OutputTokens:        pickZero(keeper.OutputTokens, dropee.OutputTokens),
		ReasoningTokens:     pickZero(keeper.ReasoningTokens, dropee.ReasoningTokens),
		CachedTokens:        pickZero(keeper.CachedTokens, dropee.CachedTokens),
		CacheReadTokens:     pickZero(keeper.CacheReadTokens, dropee.CacheReadTokens),
		CacheCreationTokens: pickZero(keeper.CacheCreationTokens, dropee.CacheCreationTokens),
		TotalTokens:         pickZero(keeper.TotalTokens, dropee.TotalTokens),
	}
	if keeper.TTFTMS == 0 && dropee.TTFTMS > 0 {
		delta.TTFTMS = dropee.TTFTMS
	}
	if err := fillKeeperZeroTx(ctx, tx, s, dropee, keeper.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE id = ?`, dropee.ID); err != nil {
		return fmt.Errorf("删除重复请求 %s 失败: %w", dropee.ID, err)
	}
	if err := subtractRollupTx(ctx, tx, s, dropee); err != nil {
		return err
	}
	return addRollupDeltaTx(ctx, tx, s, keeper, delta)
}

// fillKeeperZeroTx 把 d 行的非空展示/用量字段补进 keeper 行的空缺位。
// 被 mergeRequestPairTx（行已在库）与 absorbIntoExecutorTx（行未入库）共享。
func fillKeeperZeroTx(ctx context.Context, tx *sql.Tx, s *Store, d Request, keeperID string) error {
	d.Source = RedactSource(d.Source)
	_, err := s.execHotTx(ctx, tx,
		`UPDATE requests SET
		    input_tokens         = CASE WHEN input_tokens = 0         THEN ? ELSE input_tokens END,
		    output_tokens        = CASE WHEN output_tokens = 0        THEN ? ELSE output_tokens END,
		    reasoning_tokens     = CASE WHEN reasoning_tokens = 0     THEN ? ELSE reasoning_tokens END,
		    cached_tokens        = CASE WHEN cached_tokens = 0        THEN ? ELSE cached_tokens END,
		    cache_read_tokens    = CASE WHEN cache_read_tokens = 0    THEN ? ELSE cache_read_tokens END,
		    cache_creation_tokens= CASE WHEN cache_creation_tokens = 0 THEN ? ELSE cache_creation_tokens END,
		    total_tokens         = CASE WHEN total_tokens = 0         THEN ? ELSE total_tokens END,
		    ttft_ms              = CASE WHEN ttft_ms = 0              THEN ? ELSE ttft_ms END,
		    provider             = CASE WHEN provider = ''            THEN ? ELSE provider END,
		    source               = CASE WHEN source = ''              THEN ? ELSE source END,
		    auth_type            = CASE WHEN auth_type = ''           THEN ? ELSE auth_type END,
		    tier                 = CASE WHEN tier = ''                THEN ? ELSE tier END
		 WHERE id = ?`,
		d.InputTokens, d.OutputTokens, d.ReasoningTokens,
		d.CachedTokens, d.CacheReadTokens, d.CacheCreationTokens,
		d.TotalTokens, d.TTFTMS,
		d.Provider, d.Source, d.AuthType, d.Tier,
		keeperID)
	if err != nil {
		return fmt.Errorf("合并重复请求失败: %w", err)
	}
	return nil
}

// dedupePairSQL 自连接找出「执行器行 a + 被动行 b」的历史重复对。
// 模型名按精确相等或「渠道/模型」后缀判定（避免 LIKE 元字符误匹配）。
// 谓词与 duplicateProbeTx 同口径（结构化区间版，见常量块注释）：
//
//	b.ts 落在 a 的请求区间内（a.ts-W .. a.ts+a.latency+T）
//	两侧完成时刻（ts+latency）对齐（±dupCompleteWindow，宿主侧另加尾延迟）
//	两侧 token 计数相容（任一侧为零时不约束——结算行缺 token / 被动行被裁剪都正常）
//	ai_judge 归属行（source='ai_judge'）不参与：它没有判重候选，
//	进对账只会误吞同模型的正常行。
const dedupePairSQL = `
	SELECT a.id, b.id FROM requests a JOIN requests b
	  ON b.id <> a.id
	 AND b.ts BETWEEN a.ts - ? AND a.ts + a.latency_ms + ?
	 AND b.ts + b.latency_ms BETWEEN a.ts + a.latency_ms - ?
	                          AND a.ts + a.latency_ms + ? + ?
	 AND (a.total_tokens = 0 OR b.total_tokens = 0
	   OR ABS(a.total_tokens - b.total_tokens) <= ?)
	 AND (a.input_tokens = 0 OR b.input_tokens = 0
	   OR ABS(a.input_tokens - b.input_tokens) <= ?)
	 AND b.source <> 'ai_judge'
	 AND (a.model = b.model
	   OR substr(a.model, length(a.model) - length(b.model)) = '/' || b.model
	   OR substr(b.model, length(b.model) - length(a.model)) = '/' || a.model)
	 WHERE a.key_id <> '' AND b.key_id = '' AND a.ts >= ?
	 ORDER BY a.ts, ABS(b.ts - a.ts), b.id
	 LIMIT ?`

// DedupeRequests 清理历史遗留的双写重复行（v0.2.2 之前的版本会因判重 SQL 失效
// 把同一请求记两次）。按时间贪心两两配对：每行最多参与一次合并，
// 同模型并发请求即使配错对，聚合口径依然守恒。
// 返回合并掉的行数。
func (s *Store) DedupeRequests(ctx context.Context, since time.Time) (int, error) {
	const (
		batch     = 400
		maxMerges = 200_000
	)
	sinceMS := since.UTC().UnixMilli()
	total := 0
	for total < maxMerges {
		n, err := s.dedupeBatch(ctx, sinceMS, batch)
		if err != nil {
			return total, err
		}
		total += n
		if n < batch {
			break
		}
	}
	return total, nil
}

func (s *Store) dedupeBatch(ctx context.Context, sinceMS int64, limit int) (int, error) {
	merged := 0
	err := s.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, dedupePairSQL,
			dupTSWindow.Milliseconds(), dupHostTailWindow,
			dupCompleteWindow, dupCompleteWindow, dupHostTailWindow,
			dupTokenTolerance, dupTokenTolerance,
			sinceMS, limit)
		if err != nil {
			return fmt.Errorf("扫描重复请求失败: %w", err)
		}
		type pair struct{ keep, drop string }
		pairs := make([]pair, 0, limit)
		used := make(map[string]bool)
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.keep, &p.drop); err != nil {
				rows.Close()
				return fmt.Errorf("扫描重复请求失败: %w", err)
			}
			if used[p.keep] || used[p.drop] {
				continue
			}
			used[p.keep], used[p.drop] = true, true
			pairs = append(pairs, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("遍历重复请求失败: %w", err)
		}
		rows.Close()
		for _, p := range pairs {
			keeper, err := loadRequestTx(ctx, tx, p.keep)
			if err != nil {
				return err
			}
			dropee, err := loadRequestTx(ctx, tx, p.drop)
			if err != nil {
				return err
			}
			if err := mergeRequestPairTx(ctx, tx, s, keeper, dropee); err != nil {
				return err
			}
			merged++
		}
		return nil
	})
	return merged, err
}

// pickZero 目标为 0 时取补值，否则保留原值。
func pickZero(target, fill int64) int64 {
	if target == 0 {
		return fill
	}
	return 0
}

// addRollupDeltaTx 把合并进请求行的字段增量回补到该行的分钟聚合。
func addRollupDeltaTx(ctx context.Context, tx *sql.Tx, s *Store, r Request, d UsageBackfill) error {
	if d.InputTokens == 0 && d.OutputTokens == 0 && d.ReasoningTokens == 0 &&
		d.CachedTokens == 0 && d.CacheReadTokens == 0 && d.CacheCreationTokens == 0 &&
		d.TotalTokens == 0 && d.TTFTMS == 0 {
		return nil
	}
	ttftCount := 0
	if d.TTFTMS > 0 {
		ttftCount = 1
	}
	_, err := s.execHotTx(ctx, tx,
		`UPDATE usage_rollups SET
		    input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
		    reasoning_tokens = reasoning_tokens + ?, cached_tokens = cached_tokens + ?,
		    cache_read_tokens = cache_read_tokens + ?, cache_creation_tokens = cache_creation_tokens + ?,
		    total_tokens = total_tokens + ?,
		    ttft_sum = ttft_sum + ?, ttft_count = ttft_count + ?
		 WHERE bucket_minute = ? AND model = ? AND key_id = ? AND caller_id = ? AND provider = ?
		   AND source = ? AND auth_type = ? AND tier = ? AND result = ?`,
		d.InputTokens, d.OutputTokens, d.ReasoningTokens, d.CachedTokens,
		d.CacheReadTokens, d.CacheCreationTokens, d.TotalTokens,
		d.TTFTMS, ttftCount,
		BucketMinute(r.TS), r.Model, r.KeyID, r.CallerID, r.Provider, r.Source, r.AuthType, r.Tier, r.Result)
	if err != nil {
		return fmt.Errorf("回补分钟聚合失败: %w", err)
	}
	return nil
}

// modelCandidatesOf 由已落库的模型名推导匹配候选：全名与去渠道前缀裸名。
func modelCandidatesOf(model string) []string {
	out := []string{model}
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		out = append(out, model[i+1:])
	}
	return out
}

func loadRequestTx(ctx context.Context, tx *sql.Tx, id string) (Request, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM requests WHERE id = ?`, id)
	r, err := scanRequest(row)
	if err != nil {
		return Request{}, fmt.Errorf("读取请求 %s 失败: %w", id, err)
	}
	return r, nil
}

// subtractRollupTx 从分钟聚合中扣减一条请求的贡献；计数归零则删除该聚合行。
func subtractRollupTx(ctx context.Context, tx *sql.Tx, s *Store, r Request) error {
	if r.Result == "" {
		r.Result = ResultOK
	}
	fail := 0
	if r.Result != ResultOK {
		fail = 1
	}
	ttftCount := 0
	if r.TTFTMS > 0 {
		ttftCount = 1
	}
	dim := []any{BucketMinute(r.TS), r.Model, r.KeyID, r.CallerID, r.Provider, r.Source, r.AuthType, r.Tier, r.Result}
	sets := `req_count = req_count - 1,
		fail_count = fail_count - ?,
		input_tokens = input_tokens - ?, output_tokens = output_tokens - ?,
		reasoning_tokens = reasoning_tokens - ?, cached_tokens = cached_tokens - ?,
		cache_read_tokens = cache_read_tokens - ?, cache_creation_tokens = cache_creation_tokens - ?,
		total_tokens = total_tokens - ?,
		latency_sum = latency_sum - ?, ttft_sum = ttft_sum - ?, ttft_count = ttft_count - ?,
		generation_sum = generation_sum - ?, tps_milli_sum = tps_milli_sum - ?,
		cost_micro_usd = cost_micro_usd - ?`
	vals := []any{fail, r.InputTokens, r.OutputTokens, r.ReasoningTokens, r.CachedTokens,
		r.CacheReadTokens, r.CacheCreationTokens, r.TotalTokens,
		r.LatencyMS, r.TTFTMS, ttftCount, r.GenerationMS, r.TPSMilli, int64(r.CostMicroUSD)}
	where := `bucket_minute = ? AND model = ? AND key_id = ? AND caller_id = ? AND provider = ?
		AND source = ? AND auth_type = ? AND tier = ? AND result = ?`
	if _, err := tx.ExecContext(ctx, `UPDATE usage_rollups SET `+sets+` WHERE `+where,
		append(append([]any{}, vals...), dim...)...); err != nil {
		return fmt.Errorf("扣减分钟聚合失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_rollups WHERE `+where+` AND req_count <= 0`,
		dim...); err != nil {
		return fmt.Errorf("清理空聚合行失败: %w", err)
	}
	return nil
}

func backfillRequestTx(ctx context.Context, tx *sql.Tx, s *Store, id string, b UsageBackfill) error {
	_, err := s.execHotTx(ctx, tx,
		`UPDATE requests SET
		    input_tokens = ?, output_tokens = ?, reasoning_tokens = ?,
		    cached_tokens = ?, cache_read_tokens = ?, cache_creation_tokens = ?,
		    total_tokens = ?,
		    ttft_ms = CASE WHEN ttft_ms = 0 THEN ? ELSE ttft_ms END
		 WHERE id = ?`,
		b.InputTokens, b.OutputTokens, b.ReasoningTokens,
		b.CachedTokens, b.CacheReadTokens, b.CacheCreationTokens,
		b.TotalTokens, b.TTFTMS, id)
	if err != nil {
		return fmt.Errorf("回填请求 %s 用量失败: %w", id, err)
	}
	return nil
}

// placeholders 生成 n 个 SQL 占位符。
func placeholders(n int) string {
	if n <= 0 {
		return "''"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// requestColumns 是 requests 表的完整列清单。
const requestColumns = `id, ts, key_id, caller_id, model, provider, source, upstream_model,
	auth_id, auth_label, auth_type, tier, result,
	input_tokens, output_tokens, reasoning_tokens, cached_tokens,
	cache_read_tokens, cache_creation_tokens, total_tokens,
	latency_ms, ttft_ms, generation_ms, tps_milli,
	thinking_intensity, cost_micro_usd, currency, cost_native_micro, priced, reservation_id,
	status_code, error_note`

// RequestColumns 是 requests 表完整列清单的包外只读副本：服务层的请求
// 明细查询必须引用它而不是手抄列清单——v12 曾因副本漏列导致明细接口
// 丢字段（currency/cost_native_micro）。
const RequestColumns = requestColumns

// ScanRequest 从一行（列序必须与 RequestColumns 一致）扫描请求记录。
func ScanRequest(sc interface{ Scan(...any) error }) (Request, error) { return scanRequest(sc) }

// nativeCurrency 归一请求行的币种：空值视为 USD。
func nativeCurrency(r Request) string {
	if r.Currency == "" {
		return "USD"
	}
	return r.Currency
}

// RedactSource 清洗疑似上游凭据的来源字段。
// 部分兼容渠道会把上游 API Key 填进 Source（宿主 resolveUsageSource 在 auth
// 无邮箱/账号信息时回落取 api_key）；这类值绝不入库/外显。
// 正常来源是 openai、openai-compatible-xxx、邮箱、GCP 项目 ID 等短标签。
// 整值或任一空白分隔 token 形似凭据即整串清空。
func RedactSource(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if isCredentialToken(s) {
		return ""
	}
	for _, tok := range strings.Fields(s) {
		if isCredentialToken(tok) {
			return ""
		}
	}
	return s
}

// isCredentialToken 判断单个 token 是否形似上游凭据。
// 凭据字符集为字母数字与 -_.（API Key/JWT 常见字符），@ / : = 等是邮箱、
// URL、键值对的特征，出现即排除；长度 ≥20 且同时含字母与数字即判定为
// 凭据——正常来源标签要么很短、要么不含数字，无法覆盖的只剩
// 无分隔符且不含数字的短 Key，宁可误杀不放过。
func isCredentialToken(tok string) bool {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return false
	}
	if strings.HasPrefix(tok, "sk-") && len(tok) >= 16 {
		return true
	}
	if len(tok) >= 32 && !strings.ContainsAny(tok, "-./_ ") && isAlnum(tok) {
		return true
	}
	if len(tok) < 20 {
		return false
	}
	hasLetter, hasDigit := false, false
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			hasLetter = true
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return hasLetter && hasDigit
}

func isAlnum(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return len(s) > 0
}

// scanRequest 从一行结果扫描 Request。
func scanRequest(sc interface{ Scan(...any) error }) (Request, error) {
	var r Request
	var ts, cost, costNative int64
	var priced int
	err := sc.Scan(
		&r.ID, &ts, &r.KeyID, &r.CallerID, &r.Model, &r.Provider, &r.Source, &r.UpstreamModel,
		&r.AuthID, &r.AuthLabel, &r.AuthType, &r.Tier, &r.Result,
		&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens,
		&r.CacheReadTokens, &r.CacheCreationTokens, &r.TotalTokens,
		&r.LatencyMS, &r.TTFTMS, &r.GenerationMS, &r.TPSMilli,
		&r.ThinkingIntensity, &cost, &r.Currency, &costNative, &priced, &r.ReservationID,
		&r.StatusCode, &r.ErrorNote,
	)
	if err != nil {
		return Request{}, err
	}
	r.TS = time.UnixMilli(ts).UTC()
	r.CostMicroUSD = money.Micro(cost)
	r.CostNativeMicro = money.Micro(costNative)
	if r.Currency == "" {
		r.Currency = "USD"
	}
	if r.Currency == "USD" && r.CostNativeMicro == 0 {
		r.CostNativeMicro = r.CostMicroUSD
	}
	r.Priced = priced != 0
	r.Source = RedactSource(r.Source)
	r.ErrorNote = RedactSource(r.ErrorNote)
	return r, nil
}

// errorNoteMaxLen 是 error_note 的截断长度：足够定位问题，又不至于把上游
// 的整段错误 HTML 塞进库与面板。
const errorNoteMaxLen = 200

// SanitizeErrorNote 清洗并截断失败原因摘要（导出供执行器路径写入前使用）。
// 上游错误信息可能回显请求头或凭据（与 Source 字段同一泄露面），经
// RedactSource 同款启发式清洗。
func SanitizeErrorNote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > errorNoteMaxLen {
		s = s[:errorNoteMaxLen]
	}
	return RedactSource(s)
}
