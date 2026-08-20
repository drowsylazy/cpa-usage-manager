package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// dimensionColumns 是允许分组的维度白名单。
// 只能取自这张表，绝不拼接用户输入，避免 SQL 注入。
var dimensionColumns = map[string]string{
	"model":     "model",
	"provider":  "provider",
	"source":    "source",
	"auth_type": "auth_type",
	"tier":      "tier",
	"result":    "result",
	"key_id":    "key_id",
	"caller_id": "caller_id",
}

// requestDimensionColumns 额外支持只存在于逐请求表的维度。
var requestDimensionColumns = map[string]string{
	"auth_id":            "auth_id",
	"auth_label":         "auth_label",
	"thinking_intensity": "thinking_intensity",
}

// Dimensions 返回全部可用的分组维度名，供面板构建下拉框。
func Dimensions() []string {
	out := make([]string, 0, len(dimensionColumns)+len(requestDimensionColumns))
	for k := range dimensionColumns {
		out = append(out, k)
	}
	for k := range requestDimensionColumns {
		out = append(out, k)
	}
	// 固定顺序，便于前端稳定渲染与测试断言。
	order := []string{"model", "provider", "source", "auth_id", "auth_label", "auth_type",
		"tier", "result", "thinking_intensity", "key_id", "caller_id"}
	sorted := make([]string, 0, len(out))
	for _, name := range order {
		for _, have := range out {
			if have == name {
				sorted = append(sorted, name)
				break
			}
		}
	}
	return sorted
}

// DimensionRow 是一个维度取值的聚合结果。
type DimensionRow struct {
	Value string `json:"value"`

	Requests int64 `json:"requests"`
	Failures int64 `json:"failures"`

	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`

	CostMicroUSD money.Micro `json:"cost_micro_usd"`

	// LatencyAvgMS / TTFTAvgMS / TPSAvg 由 sum/count 推导，count 为 0 时为 0。
	LatencyAvgMS int64 `json:"latency_avg_ms"`
	TTFTAvgMS    int64 `json:"ttft_avg_ms"`
	TPSAvgMilli  int64 `json:"tps_avg_milli"`

	// CacheHitRateBP 是缓存命中率，单位为万分比（basis point），避免浮点。
	CacheHitRateBP int64 `json:"cache_hit_rate_bp"`
}

// DimensionReport 是一次维度分组查询的结果。
type DimensionReport struct {
	Dimension string         `json:"dimension"`
	Rows      []DimensionRow `json:"rows"`
	Total     DimensionRow   `json:"total"`
}

// GroupByDimension 按给定维度聚合用量。
//
// 默认走分钟聚合表（usage_rollups），维度不在聚合表里时退回逐请求表。
// limit <= 0 时返回全部分组。
func (s *Service) GroupByDimension(ctx context.Context, f UsageFilter, dimension string, limit int) (DimensionReport, error) {
	dimension = strings.TrimSpace(dimension)
	if dimension == "" {
		dimension = "model"
	}
	column, fromRollups := dimensionColumns[dimension]
	if !fromRollups {
		var ok bool
		column, ok = requestDimensionColumns[dimension]
		if !ok {
			return DimensionReport{}, fmt.Errorf("不支持的分组维度 %q", dimension)
		}
	}

	var query string
	var args []any
	if fromRollups {
		clause, a := rollupFilter(f)
		args = a
		query = `SELECT ` + column + `,
			COALESCE(SUM(req_count),0), COALESCE(SUM(fail_count),0),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro_usd),0),
			COALESCE(SUM(latency_sum),0), COALESCE(SUM(ttft_sum),0),
			COALESCE(SUM(tps_milli_sum),0), COALESCE(SUM(ttft_count),0)
			FROM usage_rollups` + clause + ` GROUP BY 1`
	} else {
		clause, a := requestFilter(f)
		args = a
		query = `SELECT ` + column + `,
			COUNT(*), COALESCE(SUM(CASE WHEN result <> 'ok' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro_usd),0),
			COALESCE(SUM(latency_ms),0), COALESCE(SUM(ttft_ms),0),
			COALESCE(SUM(tps_milli),0), COALESCE(SUM(CASE WHEN ttft_ms > 0 THEN 1 ELSE 0 END),0)
			FROM requests` + clause + ` GROUP BY 1`
	}
	query += ` ORDER BY 11 DESC, 10 DESC, 1`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	out := DimensionReport{Dimension: dimension}
	err := s.st.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r DimensionRow
			var cost, latencySum, ttftSum, tpsSum, ttftCount int64
			if err := rows.Scan(&r.Value, &r.Requests, &r.Failures,
				&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens,
				&r.CacheReadTokens, &r.CacheCreationTokens, &r.TotalTokens, &cost,
				&latencySum, &ttftSum, &tpsSum, &ttftCount); err != nil {
				return err
			}
			r.CostMicroUSD = money.Micro(cost)
			r.LatencyAvgMS = divOrZero(latencySum, r.Requests)
			r.TTFTAvgMS = divOrZero(ttftSum, ttftCount)
			r.TPSAvgMilli = divOrZero(tpsSum, r.Requests)
			r.CacheHitRateBP = cacheHitRateBP(r)
			out.Rows = append(out.Rows, r)
			accumulate(&out.Total, r, latencySum, ttftSum, tpsSum, ttftCount)
		}
		return rows.Err()
	})
	if err != nil {
		return DimensionReport{}, err
	}
	finalizeTotal(&out.Total)
	out.Total.Value = "__total__"
	return out, nil
}

// accumulate 把一行并入合计行，均值字段在最后统一重算。
func accumulate(total *DimensionRow, r DimensionRow, latencySum, ttftSum, tpsSum, ttftCount int64) {
	total.Requests += r.Requests
	total.Failures += r.Failures
	total.InputTokens += r.InputTokens
	total.OutputTokens += r.OutputTokens
	total.ReasoningTokens += r.ReasoningTokens
	total.CachedTokens += r.CachedTokens
	total.CacheReadTokens += r.CacheReadTokens
	total.CacheCreationTokens += r.CacheCreationTokens
	total.TotalTokens += r.TotalTokens
	total.CostMicroUSD = total.CostMicroUSD.AddSat(r.CostMicroUSD)
	// 合计行的均值用「累加的 sum / 累加的 count」，而非各行均值的平均。
	total.LatencyAvgMS += latencySum
	total.TTFTAvgMS += ttftSum
	total.TPSAvgMilli += tpsSum
	total.CacheHitRateBP += ttftCount
}

// finalizeTotal 把 accumulate 暂存的 sum/count 换算为均值。
func finalizeTotal(total *DimensionRow) {
	latencySum, ttftSum, tpsSum, ttftCount := total.LatencyAvgMS, total.TTFTAvgMS, total.TPSAvgMilli, total.CacheHitRateBP
	total.LatencyAvgMS = divOrZero(latencySum, total.Requests)
	total.TTFTAvgMS = divOrZero(ttftSum, ttftCount)
	total.TPSAvgMilli = divOrZero(tpsSum, total.Requests)
	total.CacheHitRateBP = cacheHitRateBP(*total)
}

// cacheHitRateBP 计算缓存命中率（万分比）：缓存读取 / (输入 + 缓存读取)。
func cacheHitRateBP(r DimensionRow) int64 {
	denom := r.InputTokens + r.CacheReadTokens
	if denom <= 0 {
		return 0
	}
	return r.CacheReadTokens * 10000 / denom
}

func divOrZero(sum, count int64) int64 {
	if count <= 0 {
		return 0
	}
	return sum / count
}

// rollupFilter 生成针对 usage_rollups 的 WHERE 子句。
func rollupFilter(f UsageFilter) (string, []any) {
	var where []string
	var args []any
	if !f.From.IsZero() {
		where = append(where, "bucket_minute >= ?")
		args = append(args, f.From.UTC().Unix()/60)
	}
	if !f.To.IsZero() {
		where = append(where, "bucket_minute < ?")
		args = append(args, f.To.UTC().Unix()/60)
	}
	for _, item := range []struct{ column, value string }{
		{"key_id", f.KeyID}, {"caller_id", f.CallerID}, {"model", f.Model},
		{"provider", f.Provider}, {"result", f.Result},
	} {
		if item.value != "" {
			where = append(where, item.column+" = ?")
			args = append(args, item.value)
		}
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// KeySummary 是单个 Key 的用量与额度摘要，供「用量/按 Key 汇总」使用。
type KeySummary struct {
	KID         string `json:"kid"`
	Label       string `json:"label"`
	Principal   string `json:"principal"`
	CallerID    string `json:"caller_id"`
	Fingerprint string `json:"fingerprint"`
	Enabled     bool   `json:"enabled"`
	Revoked     bool   `json:"revoked"`

	Requests     int64       `json:"requests"`
	Failures     int64       `json:"failures"`
	TotalTokens  int64       `json:"total_tokens"`
	CostMicroUSD money.Micro `json:"cost_micro_usd"`

	QuotaMicroUSD   *money.Micro `json:"quota_micro_usd,omitempty"`
	SpentMicroUSD   money.Micro  `json:"spent_micro_usd"`
	DailyMicroUSD   *money.Micro `json:"daily_micro_usd,omitempty"`
	DailySpent      money.Micro  `json:"daily_spent_micro_usd"`
	WeeklyMicroUSD  *money.Micro `json:"weekly_micro_usd,omitempty"`
	WeeklySpent     money.Micro  `json:"weekly_spent_micro_usd"`
	MonthlyMicroUSD *money.Micro `json:"monthly_micro_usd,omitempty"`
	MonthlySpent    money.Micro  `json:"monthly_spent_micro_usd"`

	HeldMicroUSD money.Micro `json:"held_micro_usd"`
	Concurrent   int64       `json:"concurrent"`
	LastUsedAt   *time.Time  `json:"last_used_at,omitempty"`
}

// SummaryReport 是 usage/summary 的返回体：按 Key 与按模型各一份。
type SummaryReport struct {
	ByKey   []KeySummary   `json:"by_key"`
	ByModel []DimensionRow `json:"by_model"`
	Overall UsageSummary   `json:"overall"`
}

// UsageSummaryByKey 汇总每个 Key 的用量与额度状态。
func (s *Service) UsageSummaryByKey(ctx context.Context, f UsageFilter, now time.Time) ([]KeySummary, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	keys, _, err := s.st.ListKeys(ctx, store.KeyFilter{CallerID: f.CallerID})
	if err != nil {
		return nil, err
	}
	usage, err := s.GroupByDimension(ctx, f, "key_id", 0)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]DimensionRow, len(usage.Rows))
	for _, r := range usage.Rows {
		byKey[r.Value] = r
	}

	held := make(map[string]struct {
		amount money.Micro
		count  int64
	})
	err = s.st.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT key_id, COALESCE(SUM(held_micro_usd),0), COUNT(*)
			 FROM reservations WHERE status = 'held' GROUP BY key_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kid string
			var amount, count int64
			if err := rows.Scan(&kid, &amount, &count); err != nil {
				return err
			}
			held[kid] = struct {
				amount money.Micro
				count  int64
			}{money.Micro(amount), count}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	cy := store.CyclesFor(now)
	out := make([]KeySummary, 0, len(keys))
	for i := range keys {
		k := keys[i]
		u := byKey[k.KID]
		h := held[k.KID]
		out = append(out, KeySummary{
			KID: k.KID, Label: k.Label, Principal: k.Principal, CallerID: k.CallerID,
			Fingerprint: k.Fingerprint, Enabled: k.Enabled, Revoked: k.Revoked(),
			Requests: u.Requests, Failures: u.Failures,
			TotalTokens: u.TotalTokens, CostMicroUSD: u.CostMicroUSD,
			QuotaMicroUSD: k.QuotaMicroUSD, SpentMicroUSD: k.SpentMicroUSD,
			DailyMicroUSD: k.DailyMicroUSD, DailySpent: cycleSpent(k.DailyCycleKey, cy.Daily, k.DailySpentMicroUSD),
			WeeklyMicroUSD: k.WeeklyMicroUSD, WeeklySpent: cycleSpent(k.WeeklyCycleKey, cy.Weekly, k.WeeklySpentMicroUSD),
			MonthlyMicroUSD: k.MonthlyMicroUSD, MonthlySpent: cycleSpent(k.MonthlyCycleKey, cy.Monthly, k.MonthlySpentMicroUSD),
			HeldMicroUSD: h.amount, Concurrent: h.count, LastUsedAt: k.LastUsedAt,
		})
	}
	return out, nil
}

// cycleSpent 在存储周期与当前周期不一致时视为 0（周期已滚动）。
func cycleSpent(stored, current string, v money.Micro) money.Micro {
	if stored != current {
		return 0
	}
	return v
}

// Summary 组装 usage/summary 的完整返回体。
func (s *Service) Summary(ctx context.Context, f UsageFilter, now time.Time) (SummaryReport, error) {
	overall, err := s.UsageSummary(ctx, f)
	if err != nil {
		return SummaryReport{}, err
	}
	byKey, err := s.UsageSummaryByKey(ctx, f, now)
	if err != nil {
		return SummaryReport{}, err
	}
	byModel, err := s.GroupByDimension(ctx, f, "model", 200)
	if err != nil {
		return SummaryReport{}, err
	}
	return SummaryReport{ByKey: byKey, ByModel: byModel.Rows, Overall: overall}, nil
}
