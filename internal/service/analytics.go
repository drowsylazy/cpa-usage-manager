package service

import (
	"context"
	"fmt"
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
	query := `SELECT id,ts,key_id,caller_id,model,provider,source,auth_id,auth_label,auth_type,tier,result,input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,latency_ms,ttft_ms,generation_ms,tps_milli,thinking_intensity,cost_micro_usd,priced,reservation_id FROM requests` + clause + ` ORDER BY ` + column + ` ` + direction + `, id DESC LIMIT ? OFFSET ?`
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
			if err := rows.Scan(&r.ID, &ts, &r.KeyID, &r.CallerID, &r.Model, &r.Provider, &r.Source, &r.AuthID, &r.AuthLabel, &r.AuthType, &r.Tier, &r.Result, &r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CachedTokens, &r.CacheReadTokens, &r.CacheCreationTokens, &r.TotalTokens, &r.LatencyMS, &r.TTFTMS, &r.GenerationMS, &r.TPSMilli, &r.ThinkingIntensity, &cost, &priced, &r.ReservationID); err != nil {
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
	err = s.st.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, `SELECT COALESCE(SUM(held_micro_usd),0),COUNT(*) FROM reservations WHERE key_id=? AND status='held'`, kid).Scan(&held, &concurrent)
	})
	if err != nil {
		return Balance{}, err
	}
	out := Balance{KeyID: kid, Held: money.Micro(held), Concurrent: concurrent}
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
	return out, nil
}
