package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	row := s.readDB.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM requests WHERE id = ?`, id)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, fmt.Errorf("%w: 请求 %q", ErrNotFound, id)
	}
	if err != nil {
		return Request{}, fmt.Errorf("读取请求 %q 失败: %w", id, err)
	}
	return r, nil
}

// BackfillRequestUsage 给 key+model 在 near 前后 15 秒内最近一条零用量记录
// 补写宿主 usage.handle 上报的 token 明细。返回是否找到并更新了记录。
// 只补逐请求行，分钟聚合不回填（可接受的口径差异）。
func (s *Store) BackfillRequestUsage(ctx context.Context, kid, model string, near time.Time, b UsageBackfill) (bool, error) {
	var id string
	err := s.Write(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM requests
			 WHERE key_id = ? AND model = ?
			   AND input_tokens = 0 AND output_tokens = 0 AND total_tokens = 0
			   AND ts BETWEEN ? AND ?
			 ORDER BY ts DESC LIMIT 1`,
			kid, model, near.Add(-15*time.Second).UnixMilli(), near.Add(15*time.Second).UnixMilli())
		if e := row.Scan(&id); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx,
			`UPDATE requests SET input_tokens=?, output_tokens=?, reasoning_tokens=?,
			    cached_tokens=?, cache_read_tokens=?, cache_creation_tokens=?, total_tokens=?
			 WHERE id = ? AND total_tokens = 0`,
			b.InputTokens, b.OutputTokens, b.ReasoningTokens,
			b.CachedTokens, b.CacheReadTokens, b.CacheCreationTokens, b.TotalTokens, id)
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// requestColumns 是 requests 表的完整列清单。
const requestColumns = `id, ts, key_id, caller_id, model, provider, source,
	auth_id, auth_label, auth_type, tier, result,
	input_tokens, output_tokens, reasoning_tokens, cached_tokens,
	cache_read_tokens, cache_creation_tokens, total_tokens,
	latency_ms, ttft_ms, generation_ms, tps_milli,
	thinking_intensity, cost_micro_usd, priced, reservation_id`

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
	return r, nil
}
