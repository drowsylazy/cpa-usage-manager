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

// BackfillRequestUsage 给 key+model 候选在 near 前后 15 秒内最近一条零用量记录
// 补写宿主 usage.handle 上报的 token 明细（执行器落库的模型可能是别名，
// 因此按候选列表匹配：原始模型 / 别名 / 「渠道/模型」后缀）。返回是否更新。
// 只补逐请求行，分钟聚合不回填（可接受的口径差异）。
// modelMatchFragment 生成「model IN (…) OR model LIKE '渠道/模型'」的匹配片段。
// 执行器落库常用「渠道/模型」别名，宿主上报原始模型，需按候选集 + 后缀双匹配。
func modelMatchFragment(models []string) (string, []any) {
	in := placeholders(len(models))
	like := strings.TrimSuffix(strings.Repeat("OR model LIKE ? ESCAPE '\\' ,", len(models)), ",")
	frag := "(model IN (" + in + ") " + like + ")"
	args := make([]any, 0, len(models)*2)
	for _, m := range models {
		args = append(args, m)
	}
	for _, m := range models {
		args = append(args, "%/"+escapeLike(m))
	}
	return frag, args
}

// escapeLike 转义 LIKE 通配符。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

func (s *Store) BackfillRequestUsage(ctx context.Context, kid string, models []string, near time.Time, b UsageBackfill) (bool, error) {
	if len(models) == 0 {
		return false, nil
	}
	frag, margs := modelMatchFragment(models)
	args := make([]any, 0, len(margs)+3)
	args = append(args, kid)
	args = append(args, margs...)
	args = append(args, near.Add(-15*time.Second).UnixMilli(), near.Add(15*time.Second).UnixMilli())
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

// FindDuplicateExecutor 按 时间±15s + 延迟±150ms + 模型候选 关联执行器已入库的记录。
// 宿主 usage.handle 的 APIKey 字段在部分兼容渠道里是上游凭据而非插件 Key，
// 无法用 kid 判重；同一请求的两次回调延迟仅差几毫秒，该组合足以唯一对应。
func (s *Store) FindDuplicateExecutor(ctx context.Context, models []string, near time.Time, latencyMS int64) (string, bool, error) {
	if len(models) == 0 {
		return "", false, nil
	}
	frag, margs := modelMatchFragment(models)
	args := make([]any, 0, len(margs)+4)
	args = append(args, margs...)
	args = append(args, latencyMS-150, latencyMS+150,
		near.Add(-15*time.Second).UnixMilli(), near.Add(15*time.Second).UnixMilli())
	var id string
	row := s.readDB.QueryRowContext(ctx,
		`SELECT id FROM requests
		 WHERE key_id <> '' AND `+frag+`
		   AND latency_ms BETWEEN ? AND ?
		   AND ts BETWEEN ? AND ?
		 ORDER BY ts DESC LIMIT 1`, args...)
	if err := row.Scan(&id); err != nil {
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
