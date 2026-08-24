package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

type UsageFilter struct {
	From, To                                 time.Time
	KeyID, CallerID, Model, Provider, Result string
}

func requestFilter(f UsageFilter) (string, []any) {
	var where []string
	var args []any
	if !f.From.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.From.UTC().UnixMilli())
	}
	if !f.To.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, f.To.UTC().UnixMilli())
	}
	for _, item := range []struct{ column, value string }{
		{"key_id", f.KeyID}, {"caller_id", f.CallerID},
		{"provider", f.Provider}, {"result", f.Result},
	} {
		if item.value != "" {
			where = append(where, item.column+" = ?")
			args = append(args, item.value)
		}
	}
	if f.Model != "" {
		// 模型按「精确名或渠道/后缀」匹配：输入 ox-alpha 应能命中
		// openrouter/ox-alpha；完整名则精确命中。与库内判重口径一致。
		where = append(where, "(model = ? OR model LIKE ? ESCAPE '\\')")
		args = append(args, f.Model, "%/"+escapeLikeValue(f.Model))
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// escapeLikeValue 转义 LIKE 通配符，用户输入按字面匹配。
func escapeLikeValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

type UsageSummary struct {
	Requests            int64 `json:"requests"`
	Failures            int64 `json:"failures"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	CostMicroUSD        int64 `json:"cost_micro_usd"`
}

func (s *Service) UsageSummary(ctx context.Context, f UsageFilter) (UsageSummary, error) {
	clause, args := requestFilter(f)
	var out UsageSummary
	err := s.st.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN result <> 'ok' THEN 1 ELSE 0 END),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_micro_usd),0) FROM requests`+clause, args...).Scan(&out.Requests, &out.Failures, &out.InputTokens, &out.OutputTokens, &out.ReasoningTokens, &out.CachedTokens, &out.CacheReadTokens, &out.CacheCreationTokens, &out.TotalTokens, &out.CostMicroUSD)
	})
	return out, err
}

type RequestPage struct {
	Items []store.Request `json:"items"`
	Total int64           `json:"total"`
}

func (s *Service) ListRequests(ctx context.Context, f UsageFilter, limit, offset int, sortBy, order string) (RequestPage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	columns := map[string]string{"ts": "ts", "cost": "cost_micro_usd", "tokens": "total_tokens", "latency": "latency_ms", "model": "model"}
	column := columns[sortBy]
	if column == "" {
		column = "ts"
	}
	direction := "DESC"
	if strings.EqualFold(order, "asc") {
		direction = "ASC"
	}
	clause, args := requestFilter(f)
	var total int64
	if err := s.st.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`+clause, args...).Scan(&total)
	}); err != nil {
		return RequestPage{}, err
	}
	query := `SELECT id,ts,key_id,caller_id,model,provider,source,upstream_model,auth_id,auth_label,auth_type,tier,result,input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,latency_ms,ttft_ms,generation_ms,tps_milli,thinking_intensity,cost_micro_usd,priced,reservation_id FROM requests` + clause + ` ORDER BY ` + column + ` ` + direction + `, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, max(0, offset))
	var items []store.Request
	err := s.st.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r store.Request
			var ts, cost int64
			var priced int
			if err := rows.Scan(&r.ID, &ts, &r.KeyID, &r.CallerID, &r.Model, &r.Provider, &r.Source, &r.UpstreamModel, &r.AuthID, &r.AuthLabel, &r.AuthType, &r.Tier, &r.Result, &r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens, &r.TotalTokens, &r.LatencyMS, &r.TTFTMS, &r.GenerationMS, &r.TPSMilli, &r.ThinkingIntensity, &cost, &priced, &r.ReservationID); err != nil {
				return err
			}
			r.TS = time.UnixMilli(ts).UTC()
			r.CostMicroUSD = money.Micro(cost)
			r.Priced = priced != 0
			items = append(items, r)
		}
		return rows.Err()
	})
	return RequestPage{Items: items, Total: total}, err
}

type TrendPoint struct {
	Bucket              time.Time `json:"bucket"`
	Requests            int64     `json:"requests"`
	Failures            int64     `json:"failures"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CachedTokens        int64     `json:"cached_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	TotalTokens         int64     `json:"total_tokens"`
	CostMicroUSD        int64     `json:"cost_micro_usd"`
}

func (s *Service) Trends(ctx context.Context, f UsageFilter, grain string) ([]TrendPoint, error) {
	// 分钟/时/日 用整数取整即可；周固定以周一为界，月为日历月，
	// 二者都不能用「按秒整除」表达，交给 SQLite 的日期函数处理。
	seconds := int64(60)
	bucketExpr := ""
	switch grain {
	case "", "minute":
	case "hour":
		seconds = 3600
	case "day":
		seconds = 86400
	case "week":
		// ISO 周以周一为起点：先退到当周周一零点。
		bucketExpr = `CAST(strftime('%s', date(bucket_minute*60, 'unixepoch', 'weekday 1', '-7 days')) AS INTEGER)`
	case "month":
		bucketExpr = `CAST(strftime('%s', date(bucket_minute*60, 'unixepoch', 'start of month')) AS INTEGER)`
	default:
		return nil, fmt.Errorf("不支持的趋势粒度 %q", grain)
	}
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
	for _, item := range []struct{ column, value string }{{"key_id", f.KeyID}, {"caller_id", f.CallerID}, {"model", f.Model}, {"provider", f.Provider}, {"result", f.Result}} {
		if item.value != "" {
			where = append(where, item.column+" = ?")
			args = append(args, item.value)
		}
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	selectBucket := bucketExpr
	if selectBucket == "" {
		selectBucket = `((bucket_minute*60)/?)*?`
		// 两个占位符必须排在筛选参数之前。
		args = append([]any{seconds, seconds}, args...)
	}
	query := `SELECT ` + selectBucket +
		`, SUM(req_count),SUM(fail_count),SUM(input_tokens),SUM(output_tokens),SUM(cached_tokens),SUM(cache_read_tokens),SUM(cache_creation_tokens),SUM(total_tokens),SUM(cost_micro_usd)` +
		` FROM usage_rollups` + clause + ` GROUP BY 1 ORDER BY 1`
	var out []TrendPoint
	err := s.st.Read(ctx, func(q store.Querier) error {
		rows, e := q.QueryContext(ctx, query, args...)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var p TrendPoint
			var unix int64
			if e := rows.Scan(&unix, &p.Requests, &p.Failures, &p.InputTokens, &p.OutputTokens, &p.CachedTokens, &p.CacheReadTokens, &p.CacheCreationTokens, &p.TotalTokens, &p.CostMicroUSD); e != nil {
				return e
			}
			p.Bucket = time.Unix(unix, 0).UTC()
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

type CostCoverage struct {
	Requests       int64 `json:"requests"`
	PricedRequests int64 `json:"priced_requests"`
	CostMicroUSD   int64 `json:"cost_micro_usd"`
}

func (s *Service) Costs(ctx context.Context, f UsageFilter) (CostCoverage, error) {
	clause, args := requestFilter(f)
	var out CostCoverage
	err := s.st.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(priced),0),COALESCE(SUM(cost_micro_usd),0) FROM requests`+clause, args...).Scan(&out.Requests, &out.PricedRequests, &out.CostMicroUSD)
	})
	return out, err
}

type Balance struct {
	KeyID      string       `json:"key_id"`
	Total      *money.Micro `json:"total_remaining_micro_usd,omitempty"`
	Daily      *money.Micro `json:"daily_remaining_micro_usd,omitempty"`
	Weekly     *money.Micro `json:"weekly_remaining_micro_usd,omitempty"`
	Monthly    *money.Micro `json:"monthly_remaining_micro_usd,omitempty"`
	Held       money.Micro  `json:"held_micro_usd"`
	Concurrent int64        `json:"concurrent"`

	// Token 余量；nil 表示该档未设限。口径与费用一致（计费四类合计）。
	TotalTokens   *int64 `json:"total_remaining_tokens,omitempty"`
	DailyTokens   *int64 `json:"daily_remaining_tokens,omitempty"`
	WeeklyTokens  *int64 `json:"weekly_remaining_tokens,omitempty"`
	MonthlyTokens *int64 `json:"monthly_remaining_tokens,omitempty"`
	HeldTokens    int64  `json:"held_tokens"`
}

func (s *Service) Balance(ctx context.Context, kid string, now time.Time) (Balance, error) {
	k, err := s.st.GetKey(ctx, kid)
	if err != nil {
		return Balance{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cy := store.CyclesFor(now)
	var held int64
	var concurrent int64
	var heldTokens int64
	err = s.st.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, `SELECT COALESCE(SUM(held_micro_usd),0),COUNT(*),COALESCE(SUM(reserved_tokens),0) FROM reservations WHERE key_id=? AND status='held'`, kid).Scan(&held, &concurrent, &heldTokens)
	})
	if err != nil {
		return Balance{}, err
	}
	out := Balance{KeyID: kid, Held: money.Micro(held), Concurrent: concurrent, HeldTokens: heldTokens}
	remain := func(limit *money.Micro, spent money.Micro) *money.Micro {
		if limit == nil {
			return nil
		}
		v := *limit - spent
		return &v
	}
	cycle := func(stored, current string, v money.Micro) money.Micro {
		if stored != current {
			return 0
		}
		return v
	}
	out.Total = remain(k.QuotaMicroUSD, k.SpentMicroUSD+out.Held)
	out.Daily = remain(k.DailyMicroUSD, cycle(k.DailyCycleKey, cy.Daily, k.DailySpentMicroUSD)+out.Held)
	out.Weekly = remain(k.WeeklyMicroUSD, cycle(k.WeeklyCycleKey, cy.Weekly, k.WeeklySpentMicroUSD)+out.Held)
	out.Monthly = remain(k.MonthlyMicroUSD, cycle(k.MonthlyCycleKey, cy.Monthly, k.MonthlySpentMicroUSD)+out.Held)
	// Token 余量：与金额同构（已用 + 在途预占 一并扣除）
	remainTok := func(limit *int64, used int64) *int64 {
		if limit == nil {
			return nil
		}
		v := *limit - used
		return &v
	}
	cycleTok := func(stored, current string, v int64) int64 {
		if stored != current {
			return 0
		}
		return v
	}
	out.TotalTokens = remainTok(k.TokenLimit, k.TokensUsed+heldTokens)
	out.DailyTokens = remainTok(k.DailyTokenLimit, cycleTok(k.DailyCycleKey, cy.Daily, k.DailyTokensUsed)+heldTokens)
	out.WeeklyTokens = remainTok(k.WeeklyTokenLimit, cycleTok(k.WeeklyCycleKey, cy.Weekly, k.WeeklyTokensUsed)+heldTokens)
	out.MonthlyTokens = remainTok(k.MonthlyTokenLimit, cycleTok(k.MonthlyCycleKey, cy.Monthly, k.MonthlyTokensUsed)+heldTokens)
	return out, nil
}

// RouteRow 是一条「上游实际模型」的路由聚合行。
// UpstreamModel 为别名本身时表示该别名下完全没有显式真名可分摊。
type RouteRow struct {
	UpstreamModel string   `json:"upstream_model"`
	Requests      int64    `json:"requests"`
	TotalTokens   int64    `json:"total_tokens"`
	Models        []string `json:"models"` // 涉及的本地别名，供前端按别名筛选
}

// RouteReport 按上游实际模型聚合，用于暴露上游二次路由：
// 同一别名被拆到多个真名、或渠道返回了意料之外的模型时一目了然。
// 部分请求嗅探到真名、部分没捕获到的别名，未捕获请求按各真名行已捕获量
// 加权分摊（requests 按请求占比、token 按 token 占比），本地别名不再单独成行；
// Models 收集涉及的本地别名供前端筛选。
func (s *Service) RouteReport(ctx context.Context, f UsageFilter) ([]RouteRow, error) {
	clause, args := requestFilter(f)
	query := `SELECT COALESCE(NULLIF(upstream_model,''), model), COALESCE(upstream_model,''),
			COUNT(*), COALESCE(SUM(total_tokens),0),
			COALESCE(GROUP_CONCAT(DISTINCT model),'')
		FROM requests` + clause + `
		GROUP BY COALESCE(NULLIF(upstream_model,''), model)
		ORDER BY 3 DESC LIMIT 500`
	var aggs []struct {
		row RouteRow
		raw string // 原始 upstream_model；空 = 回退行（展示名实为别名）
	}
	err := s.st.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a struct {
				row RouteRow
				raw string
			}
			var models string
			if err := rows.Scan(&a.row.UpstreamModel, &a.raw, &a.row.Requests, &a.row.TotalTokens, &models); err != nil {
				return err
			}
			for _, m := range strings.Split(models, ",") {
				if m = strings.TrimSpace(m); m != "" {
					a.row.Models = append(a.row.Models, m)
				}
			}
			aggs = append(aggs, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("聚合上游路由失败: %w", err)
	}
	// 回退行（raw==''，展示名实为别名）的归并：把该别名下未捕获真名的请求
	// 按各显式真名行**已捕获**的量加权分摊（requests 按请求占比、token 按 token
	// 占比，最大余数法保整数守恒），别名不再单独成行；该别名完全没有显式真名时
	// 才保留为独立行。权重取原始快照，多个别名分摊到同一真名行时互不干扰。
	type agg struct {
		row RouteRow
		raw string // 原始 upstream_model；空 = 回退行
		req int64  // 已捕获原始值，作分摊权重
		tok int64
	}
	rows2 := make([]agg, len(aggs))
	for i, a := range aggs {
		rows2[i] = agg{row: a.row, raw: a.raw, req: a.row.Requests, tok: a.row.TotalTokens}
	}
	targets := make(map[string][]int) // 别名 → 显式真名行下标
	for i := range rows2 {
		if rows2[i].raw == "" {
			continue
		}
		for _, m := range rows2[i].row.Models {
			targets[m] = append(targets[m], i)
		}
	}
	drop := make(map[int]bool)
	for i := range rows2 {
		if rows2[i].raw != "" {
			continue
		}
		dst := targets[rows2[i].row.UpstreamModel]
		if len(dst) == 0 {
			continue
		}
		wReq := make([]int64, len(dst))
		wTok := make([]int64, len(dst))
		for k, j := range dst {
			wReq[k] = rows2[j].req
			wTok[k] = rows2[j].tok
		}
		for k, j := range dst {
			rows2[j].row.Requests += shareInt(rows2[i].row.Requests, wReq, k)
			rows2[j].row.TotalTokens += shareInt(rows2[i].row.TotalTokens, wTok, k)
		}
		drop[i] = true
	}
	out := make([]RouteRow, 0, len(rows2))
	for i := range rows2 {
		if !drop[i] {
			out = append(out, rows2[i].row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out, nil
}

// shareInt 把 total 按 weights 加权分成 len(weights) 份，返回第 k 份。
// 最大余数法：各份之和恒等于 total（权重和为 0 时全部给第 0 份）。
func shareInt(total int64, weights []int64, k int) int64 {
	var wsum int64
	for _, w := range weights {
		if w < 0 {
			w = 0
		}
		wsum += w
	}
	if wsum <= 0 || total <= 0 {
		if k == 0 {
			return total
		}
		return 0
	}
	shares := make([]int64, len(weights))
	var acc int64
	for i, w := range weights {
		if w < 0 {
			w = 0
		}
		shares[i] = total * w / wsum
		acc += shares[i]
	}
	rem := total - acc
	if rem > 0 {
		order := make([]int, len(weights))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return weights[order[a]] > weights[order[b]] })
		for i := 0; rem > 0 && i < len(order); i++ {
			shares[order[i]]++
			rem--
		}
	}
	return shares[k]
}
