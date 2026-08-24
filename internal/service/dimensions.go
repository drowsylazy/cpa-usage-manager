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
	// Count 是分组总数，不受 limit 截断影响；前端据此展示「共 N 项」。
	Count int64 `json:"count"`
}

// GroupByDimension 按给定维度聚合用量。
//
// 默认走分钟聚合表（usage_rollups），维度不在聚合表里时退回逐请求表。
// limit <= 0 时返回全部分组；limit > 0 时 SQL 层直接返回前 N 组（按
// 费用/Token/名称排序），合计查询把 Total 与分组数下推到 SQL——
// Total/Count 始终覆盖全部分组，而内存峰值保持 O(limit)，不随分组数膨胀。
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

	table, agg := "usage_rollups", rollupAggregates
	clause, args := rollupFilter(f)
	if !fromRollups {
		table, agg = "requests", requestAggregates
		clause, args = requestFilter(f)
	}

	rowQuery := `SELECT ` + column + `, ` + agg + `
		FROM ` + table + clause + ` GROUP BY 1` +
		` ORDER BY 11 DESC, 10 DESC, 1`
	if limit > 0 {
		rowQuery += fmt.Sprintf(" LIMIT %d", limit)
	}
	totalQuery := `SELECT ` + agg + `, COUNT(DISTINCT ` + column + `) FROM ` + table + clause

	out := DimensionReport{Dimension: dimension}
	err := s.st.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, rowQuery, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, sums, err := scanDimensionRow(rows)
			if err != nil {
				return err
			}
			out.Rows = append(out.Rows, r)
			accumulate(&out.Total, r, sums)
		}
		return rows.Err()
	})
	if err != nil {
		return DimensionReport{}, err
	}
	finalizeTotal(&out.Total)
	out.Total.Value = "__total__"
	out.Count = int64(len(out.Rows))

	if limit > 0 {
		// 合计下推：一条不分组聚合查出全量 Total 与分组数，
		// 避免为算合计而把全部分组装载进内存再排序截断。
		var t DimensionRow
		var cost, latencySum, ttftSum, tpsSum, ttftCount int64
		if err := s.st.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, totalQuery, args...).Scan(
				&t.Requests, &t.Failures,
				&t.InputTokens, &t.OutputTokens, &t.ReasoningTokens, &t.CachedTokens,
				&t.CacheReadTokens, &t.CacheCreationTokens, &t.TotalTokens, &cost,
				&latencySum, &ttftSum, &tpsSum, &ttftCount, &out.Count)
		}); err != nil {
			return DimensionReport{}, err
		}
		t.CostMicroUSD = money.Micro(cost)
		t.LatencyAvgMS = divOrZero(latencySum, t.Requests)
		t.TTFTAvgMS = divOrZero(ttftSum, ttftCount)
		t.TPSAvgMilli = divOrZero(tpsSum, t.Requests)
		t.CacheHitRateBP = cacheHitRateBP(t)
		t.Value = "__total__"
		out.Total = t
	}
	return out, nil
}

// rollupAggregates / requestAggregates 是两表的聚合列清单（14 列），
// 分组查询与合计查询共用同一份文本，保证两条查询口径严格一致。
const rollupAggregates = `COALESCE(SUM(req_count),0), COALESCE(SUM(fail_count),0),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro_usd),0),
			COALESCE(SUM(latency_sum),0), COALESCE(SUM(ttft_sum),0),
			COALESCE(SUM(tps_milli_sum),0), COALESCE(SUM(ttft_count),0)`

const requestAggregates = `COUNT(*), COALESCE(SUM(CASE WHEN result <> 'ok' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro_usd),0),
			COALESCE(SUM(latency_ms),0), COALESCE(SUM(ttft_ms),0),
			COALESCE(SUM(tps_milli),0), COALESCE(SUM(CASE WHEN ttft_ms > 0 THEN 1 ELSE 0 END),0)`

// dimRawSums 是一行分组的原始聚合值（均值换算前的 sum/count），
// 供合计行用「sum 累加 / count 累加」精确重算均值。
type dimRawSums struct {
	latencySum, ttftSum, tpsSum, ttftCount int64
}

// scanDimensionRow 扫描一行分组结果（维度值 + 14 个聚合列）。
func scanDimensionRow(sc interface{ Scan(...any) error }) (DimensionRow, dimRawSums, error) {
	var r DimensionRow
	var sums dimRawSums
	var cost int64
	if err := sc.Scan(&r.Value, &r.Requests, &r.Failures,
		&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens,
		&r.CacheReadTokens, &r.CacheCreationTokens, &r.TotalTokens, &cost,
		&sums.latencySum, &sums.ttftSum, &sums.tpsSum, &sums.ttftCount); err != nil {
		return DimensionRow{}, dimRawSums{}, err
	}
	r.CostMicroUSD = money.Micro(cost)
	r.LatencyAvgMS = divOrZero(sums.latencySum, r.Requests)
	r.TTFTAvgMS = divOrZero(sums.ttftSum, sums.ttftCount)
	r.TPSAvgMilli = divOrZero(sums.tpsSum, r.Requests)
	r.CacheHitRateBP = cacheHitRateBP(r)
	return r, sums, nil
}

// accumulate 把一行并入合计行，均值字段在最后统一重算。
func accumulate(total *DimensionRow, r DimensionRow, sums dimRawSums) {
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
	total.LatencyAvgMS += sums.latencySum
	total.TTFTAvgMS += sums.ttftSum
	total.TPSAvgMilli += sums.tpsSum
	total.CacheHitRateBP += sums.ttftCount
}

// finalizeTotal 把 accumulate 暂存的 sum/count 换算为均值。
func finalizeTotal(total *DimensionRow) {
	latencySum, ttftSum, tpsSum, ttftCount := total.LatencyAvgMS, total.TTFTAvgMS, total.TPSAvgMilli, total.CacheHitRateBP
	total.LatencyAvgMS = divOrZero(latencySum, total.Requests)
	total.TTFTAvgMS = divOrZero(ttftSum, ttftCount)
	total.TPSAvgMilli = divOrZero(tpsSum, total.Requests)
	total.CacheHitRateBP = cacheHitRateBP(*total)
}

// cacheHitRateBP 计算缓存命中率（万分比），与面板展示同口径：
// 命中 = max(缓存读+缓存写, cached)（兼容 Claude/OpenAI 两种上游口径），
// 分母 = 输入 + 缓存读 + 缓存写。
func cacheHitRateBP(r DimensionRow) int64 {
	denom := r.InputTokens + r.CacheReadTokens + r.CacheCreationTokens
	if denom <= 0 {
		return 0
	}
	hit := r.CacheReadTokens + r.CacheCreationTokens
	if r.CachedTokens > hit {
		hit = r.CachedTokens
	}
	return hit * 10000 / denom
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
