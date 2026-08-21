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
		err := insertRequestTx(ctx, tx, r)
		if errors.Is(err, errDuplicateRequest) {
			// 宿主重复回调同一请求：已入库，聚合不再累加，整体视为成功。
			return nil
		}
		if err != nil {
			return err
		}
		return upsertRollupTx(ctx, tx, r)
	})
}

// insertRequestTx 写入一条逐请求记录。
// 同 ID 重复写入按幂等处理（忽略后写），避免宿主重复回调造成双计。
func insertRequestTx(ctx context.Context, tx *sql.Tx, r Request) error {
	if r.Result == "" {
		r.Result = ResultOK
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO requests (
			id, ts, key_id, caller_id, model, provider, source,
			auth_id, auth_label, auth_type, tier, result,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			cache_read_tokens, cache_creation_tokens, total_tokens,
			latency_ms, ttft_ms, generation_ms, tps_milli,
			thinking_intensity, cost_micro_usd, priced, reservation_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		r.ID, r.TS.UTC().UnixMilli(), r.KeyID, r.CallerID, r.Model, r.Provider, r.Source,
		r.AuthID, r.AuthLabel, r.AuthType, r.Tier, r.Result,
		r.InputTokens, r.OutputTokens, r.ReasoningTokens, r.CachedTokens,
		r.CacheReadTokens, r.CacheCreationTokens, r.TotalTokens,
		r.LatencyMS, r.TTFTMS, r.GenerationMS, r.TPSMilli,
		r.ThinkingIntensity, int64(r.CostMicroUSD), boolInt(r.Priced), r.ReservationID)
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
func upsertRollupTx(ctx context.Context, tx *sql.Tx, r Request) error {
	if r.Result == "" {
		r.Result = ResultOK
	}
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

	_, err := tx.ExecContext(ctx,
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

// 双写判重容差。宿主 usage.handle 与执行器结算描述同一次上游调用：
// 两者观测到的时刻相差不超过一次回调往返，延迟读数相差仅几毫秒。
const (
	dupTSWindow      = 15 * time.Second
	dupLatencyWindow = int64(150) // 毫秒
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
		return backfillRequestTx(ctx, tx, id, b)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// FindDuplicateExecutor 按 时间±dupTSWindow + 延迟±dupLatencyWindow + 模型候选
// 关联执行器已入库的记录。
// 宿主 usage.handle 的 APIKey 字段在部分兼容渠道里是上游凭据而非插件 Key，
// 无法用 kid 判重；同一请求的两次回调延迟仅差几毫秒，该组合足以唯一对应。
func (s *Store) FindDuplicateExecutor(ctx context.Context, models []string, near time.Time, latencyMS int64) (string, bool, error) {
	if len(normalizeModels(models)) == 0 {
		return "", false, nil
	}
	frag, margs := modelMatchFragment(models)
	args := make([]any, 0, len(margs)+5)
	args = append(args, margs...)
	args = append(args, latencyMS-dupLatencyWindow, latencyMS+dupLatencyWindow,
		near.Add(-dupTSWindow).UnixMilli(), near.Add(dupTSWindow).UnixMilli(),
		near.UnixMilli())
	var id string
	err := s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx,
			`SELECT id FROM requests
			 WHERE key_id <> '' AND `+frag+`
			   AND latency_ms BETWEEN ? AND ?
			   AND ts BETWEEN ? AND ?
			 ORDER BY ABS(ts - ?) LIMIT 1`, args...).Scan(&id)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

// BackfillRequestUsageByID 按 ID 回填宿主上报的用量明细（含首字延迟兜底）。
func (s *Store) BackfillRequestUsageByID(ctx context.Context, id string, b UsageBackfill) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		return backfillRequestTx(ctx, tx, id, b)
	})
}

// ReconcileRequestDuplicates 落库后对账，消除执行器/被动双写竞态。
//
// 同一请求可能触发两次落库（执行器结算与宿主 usage.handle），两者几乎同时发生，
// 「先查后插」的判重存在窗口。这里以 anchor 行为起点找另一口径的重复行
// （key_id 空否相反 + 模型候选匹配 + 延迟±dupLatencyWindow + 时间±dupTSWindow）：
// 保留 key_id 非空的执行器行，把被动行的零值字段（token 明细、首字延迟、
// provider/source/auth/tier 展示信息）合并进去，删除被动行，
// 并从分钟聚合中扣减被动行的贡献。返回是否发生合并。
func (s *Store) ReconcileRequestDuplicates(ctx context.Context, anchorID string) (bool, error) {
	merged := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		anchor, e := loadRequestTx(ctx, tx, anchorID)
		if e != nil {
			return e
		}
		frag, margs := modelMatchFragment(modelCandidatesOf(anchor.Model))
		keyCond := `key_id = ''`
		if anchor.KeyID == "" {
			keyCond = `key_id <> ''`
		}
		args := make([]any, 0, len(margs)+5)
		args = append(args, anchorID)
		args = append(args, margs...)
		args = append(args, anchor.LatencyMS-dupLatencyWindow, anchor.LatencyMS+dupLatencyWindow,
			anchor.TS.Add(-dupTSWindow).UnixMilli(), anchor.TS.Add(dupTSWindow).UnixMilli(),
			anchor.TS.UnixMilli())
		row := tx.QueryRowContext(ctx,
			`SELECT `+requestColumns+` FROM requests
			 WHERE id <> ? AND `+keyCond+` AND `+frag+`
			   AND latency_ms BETWEEN ? AND ?
			   AND ts BETWEEN ? AND ?
			 ORDER BY ABS(ts - ?) LIMIT 1`, args...)
		other, e := scanRequest(row)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		keeper, dropee := anchor, other
		if keeper.KeyID == "" {
			keeper, dropee = dropee, keeper
		}
		if e := mergeRequestPairTx(ctx, tx, keeper, dropee); e != nil {
			return e
		}
		merged = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return merged, nil
}

// mergeRequestPairTx 把 dropee 行并入 keeper 行：仅补 keeper 的零值/空值字段，
// 删除 dropee，并同步分钟聚合（扣减 dropee 的整行贡献 + 回补并入 keeper 的增量）。
//
// 费用不参与合并：执行器行的 cost_micro_usd 已按结算金额记入 Key 账本，
// 改写会造成明细与账本口径不一致。
func mergeRequestPairTx(ctx context.Context, tx *sql.Tx, keeper, dropee Request) error {
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
	if _, err := tx.ExecContext(ctx,
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
		dropee.InputTokens, dropee.OutputTokens, dropee.ReasoningTokens,
		dropee.CachedTokens, dropee.CacheReadTokens, dropee.CacheCreationTokens,
		dropee.TotalTokens, dropee.TTFTMS,
		dropee.Provider, dropee.Source, dropee.AuthType, dropee.Tier,
		keeper.ID); err != nil {
		return fmt.Errorf("合并重复请求失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE id = ?`, dropee.ID); err != nil {
		return fmt.Errorf("删除重复请求 %s 失败: %w", dropee.ID, err)
	}
	if err := subtractRollupTx(ctx, tx, dropee); err != nil {
		return err
	}
	return addRollupDeltaTx(ctx, tx, keeper, delta)
}

// dedupePairSQL 自连接找出「执行器行 a + 被动行 b」的历史重复对。
// 模型名按精确相等或「渠道/模型」后缀判定（避免 LIKE 元字符误匹配）。
const dedupePairSQL = `
	SELECT a.id, b.id FROM requests a JOIN requests b
	  ON b.id <> a.id
	 AND b.ts BETWEEN a.ts - ? AND a.ts + ?
	 AND b.latency_ms BETWEEN a.latency_ms - ? AND a.latency_ms + ?
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
			dupTSWindow.Milliseconds(), dupTSWindow.Milliseconds(),
			dupLatencyWindow, dupLatencyWindow, sinceMS, limit)
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
			if err := mergeRequestPairTx(ctx, tx, keeper, dropee); err != nil {
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
func addRollupDeltaTx(ctx context.Context, tx *sql.Tx, r Request, d UsageBackfill) error {
	if d.InputTokens == 0 && d.OutputTokens == 0 && d.ReasoningTokens == 0 &&
		d.CachedTokens == 0 && d.CacheReadTokens == 0 && d.CacheCreationTokens == 0 &&
		d.TotalTokens == 0 && d.TTFTMS == 0 {
		return nil
	}
	ttftCount := 0
	if d.TTFTMS > 0 {
		ttftCount = 1
	}
	_, err := tx.ExecContext(ctx,
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
func subtractRollupTx(ctx context.Context, tx *sql.Tx, r Request) error {
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

func backfillRequestTx(ctx context.Context, tx *sql.Tx, id string, b UsageBackfill) error {
	_, err := tx.ExecContext(ctx,
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
const requestColumns = `id, ts, key_id, caller_id, model, provider, source,
	auth_id, auth_label, auth_type, tier, result,
	input_tokens, output_tokens, reasoning_tokens, cached_tokens,
	cache_read_tokens, cache_creation_tokens, total_tokens,
	latency_ms, ttft_ms, generation_ms, tps_milli,
	thinking_intensity, cost_micro_usd, priced, reservation_id`

// RedactSource 清洗疑似上游凭据的来源字段。
// 部分兼容渠道会把上游 API Key 填进 Source；这类值绝不入库/外显：
// - sk- 前缀的凭据；
// - 32 位以上纯字母数字单 token（正常来源是 openai、openai-compatible-xxx 等短标签）。
func RedactSource(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "sk-") && len(s) >= 16 {
		return ""
	}
	if len(s) >= 32 && !strings.ContainsAny(s, "-./_ ") && isAlnum(s) {
		return ""
	}
	return s
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
	var ts, cost int64
	var priced int
	err := sc.Scan(
		&r.ID, &ts, &r.KeyID, &r.CallerID, &r.Model, &r.Provider, &r.Source,
		&r.AuthID, &r.AuthLabel, &r.AuthType, &r.Tier, &r.Result,
		&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens,
		&r.CacheReadTokens, &r.CacheCreationTokens, &r.TotalTokens,
		&r.LatencyMS, &r.TTFTMS, &r.GenerationMS, &r.TPSMilli,
		&r.ThinkingIntensity, &cost, &priced, &r.ReservationID,
	)
	if err != nil {
		return Request{}, err
	}
	r.TS = time.UnixMilli(ts).UTC()
	r.CostMicroUSD = money.Micro(cost)
	r.Priced = priced != 0
	r.Source = RedactSource(r.Source)
	return r, nil
}
