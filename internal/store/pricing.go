package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

// pricingColumns 是 pricing_rules 表的完整列清单。
const pricingColumns = `id, match_kind, pattern, priority, enabled,
	price_input, price_output, price_reasoning, price_cached,
	price_cache_read, price_cache_creation,
	accounting_mode, billing_mode, per_image_micro_usd,
	source, models_dev_id, created_at, updated_at`

// scanPricingRule 从一行结果扫描 PricingRule。
func scanPricingRule(sc interface{ Scan(...any) error }) (PricingRule, error) {
	var r PricingRule
	var enabled int
	var created, updated int64
	var in, out, reason, cached, cacheRead, cacheCreate, perImage int64
	err := sc.Scan(
		&r.ID, &r.MatchKind, &r.Pattern, &r.Priority, &enabled,
		&in, &out, &reason, &cached, &cacheRead, &cacheCreate,
		&r.AccountingMode, &r.BillingMode, &perImage,
		&r.Source, &r.ModelsDevID, &created, &updated,
	)
	if err != nil {
		return PricingRule{}, err
	}
	r.Enabled = enabled != 0
	r.PriceInput = money.Price(in)
	r.PriceOutput = money.Price(out)
	r.PriceReasoning = money.Price(reason)
	r.PriceCached = money.Price(cached)
	r.PriceCacheRead = money.Price(cacheRead)
	r.PriceCacheCreation = money.Price(cacheCreate)
	r.PerImageMicroUSD = money.Micro(perImage)
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.UpdatedAt = time.UnixMilli(updated).UTC()
	return r, nil
}

// 计价模式取值。
const (
	// AccountingModeDefault 按 usageparse 归一化后的四类口径计价。
	AccountingModeDefault = "default"
	// AccountingModeInputInclusive 强制把上游输入视为已含缓存命中。
	AccountingModeInputInclusive = "input_inclusive"
	// AccountingModeInputExclusive 强制把上游输入视为不含缓存命中。
	AccountingModeInputExclusive = "input_exclusive"

	// BillingModeToken 按 token 计价。
	BillingModeToken = "token"
	// BillingModePerImage 按张计价（图像/视频生成）。
	BillingModePerImage = "per_image"
	// BillingModeFree 恒定免费。
	BillingModeFree = "free"
)

// ValidAccountingMode 校验 accounting_mode。
func ValidAccountingMode(m string) error {
	switch m {
	case AccountingModeDefault, AccountingModeInputInclusive, AccountingModeInputExclusive:
		return nil
	default:
		return fmt.Errorf("accounting_mode 须为 default/input_inclusive/input_exclusive，得到 %q", m)
	}
}

// ValidBillingMode 校验 billing_mode。
func ValidBillingMode(m string) error {
	switch m {
	case BillingModeToken, BillingModePerImage, BillingModeFree:
		return nil
	default:
		return fmt.Errorf("billing_mode 须为 token/per_image/free，得到 %q", m)
	}
}

// UpsertPricingRule 新增或更新一条计价规则。
// (match_kind, pattern) 唯一，重复写入即为更新。
func (s *Store) UpsertPricingRule(ctx context.Context, r PricingRule) (PricingRule, error) {
	r.MatchKind = strings.ToLower(strings.TrimSpace(r.MatchKind))
	r.Pattern = strings.TrimSpace(r.Pattern)
	if r.AccountingMode == "" {
		r.AccountingMode = AccountingModeDefault
	}
	if r.BillingMode == "" {
		r.BillingMode = BillingModeToken
	}
	if r.Source == "" {
		r.Source = PricingSourceManual
	}
	if err := r.Validate(); err != nil {
		return PricingRule{}, err
	}
	if err := ValidAccountingMode(r.AccountingMode); err != nil {
		return PricingRule{}, err
	}
	if err := ValidBillingMode(r.BillingMode); err != nil {
		return PricingRule{}, err
	}

	now := nowMillis()
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO pricing_rules (
				match_kind, pattern, priority, enabled,
				price_input, price_output, price_reasoning, price_cached,
				price_cache_read, price_cache_creation,
				accounting_mode, billing_mode, per_image_micro_usd,
				source, models_dev_id, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(match_kind, pattern) DO UPDATE SET
				priority             = excluded.priority,
				enabled              = excluded.enabled,
				price_input          = excluded.price_input,
				price_output         = excluded.price_output,
				price_reasoning      = excluded.price_reasoning,
				price_cached         = excluded.price_cached,
				price_cache_read     = excluded.price_cache_read,
				price_cache_creation = excluded.price_cache_creation,
				accounting_mode      = excluded.accounting_mode,
				billing_mode         = excluded.billing_mode,
				per_image_micro_usd  = excluded.per_image_micro_usd,
				source               = excluded.source,
				models_dev_id        = excluded.models_dev_id,
				updated_at           = excluded.updated_at`,
			r.MatchKind, r.Pattern, r.Priority, boolInt(r.Enabled),
			int64(r.PriceInput), int64(r.PriceOutput), int64(r.PriceReasoning), int64(r.PriceCached),
			int64(r.PriceCacheRead), int64(r.PriceCacheCreation),
			r.AccountingMode, r.BillingMode, int64(r.PerImageMicroUSD),
			r.Source, r.ModelsDevID, now, now)
		if err != nil {
			return fmt.Errorf("写入计价规则 %s:%s 失败: %w", r.MatchKind, r.Pattern, err)
		}
		return nil
	})
	if err != nil {
		return PricingRule{}, err
	}
	return s.GetPricingRuleByPattern(ctx, r.MatchKind, r.Pattern)
}

// GetPricingRule 按 ID 读取规则。
func (s *Store) GetPricingRule(ctx context.Context, id int64) (PricingRule, error) {
	var r PricingRule
	err := s.Read(ctx, func(q Querier) error {
		var e error
		r, e = scanPricingRule(q.QueryRowContext(ctx,
			`SELECT `+pricingColumns+` FROM pricing_rules WHERE id = ?`, id))
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PricingRule{}, fmt.Errorf("%w: 计价规则 #%d", ErrNotFound, id)
	}
	if err != nil {
		return PricingRule{}, fmt.Errorf("读取计价规则 #%d 失败: %w", id, err)
	}
	return r, nil
}

// GetPricingRuleByPattern 按 (match_kind, pattern) 读取规则。
func (s *Store) GetPricingRuleByPattern(ctx context.Context, matchKind, pattern string) (PricingRule, error) {
	var r PricingRule
	err := s.Read(ctx, func(q Querier) error {
		var e error
		r, e = scanPricingRule(q.QueryRowContext(ctx,
			`SELECT `+pricingColumns+` FROM pricing_rules WHERE match_kind = ? AND pattern = ?`,
			matchKind, pattern))
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PricingRule{}, fmt.Errorf("%w: 计价规则 %s:%s", ErrNotFound, matchKind, pattern)
	}
	if err != nil {
		return PricingRule{}, fmt.Errorf("读取计价规则 %s:%s 失败: %w", matchKind, pattern, err)
	}
	return r, nil
}

// ListPricingRules 列出全部计价规则，按匹配优先级排序（高优先级在前）。
func (s *Store) ListPricingRules(ctx context.Context, onlyEnabled bool) ([]PricingRule, error) {
	sqlText := `SELECT ` + pricingColumns + ` FROM pricing_rules`
	if onlyEnabled {
		sqlText += ` WHERE enabled = 1`
	}
	// 排序即匹配顺序：priority 降序，同优先级下 id 升序（先建先胜），结果稳定。
	sqlText += ` ORDER BY priority DESC, id ASC`
	var out []PricingRule
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, sqlText)
		if err != nil {
			return fmt.Errorf("列出计价规则失败: %w", err)
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			r, err := scanPricingRule(rows)
			if err != nil {
				return fmt.Errorf("扫描计价规则失败: %w", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("遍历计价规则失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePricingRule 按 ID 删除规则。
//
// 兜底规则（glob:*，最低优先级）不允许删除：它是 unknown_policy=allow
// 能正常工作的前提，删掉会让所有未配价模型突然变成「未知模型」。
func (s *Store) DeletePricingRule(ctx context.Context, id int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		var r PricingRule
		row := tx.QueryRowContext(ctx, `SELECT `+pricingColumns+` FROM pricing_rules WHERE id = ?`, id)
		r, err := scanPricingRule(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 计价规则 #%d", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("读取计价规则 #%d 失败: %w", id, err)
		}
		if r.IsFallback() {
			return fmt.Errorf("兜底计价规则（%s:%s）不可删除，如需改价请直接更新它",
				r.MatchKind, r.Pattern)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pricing_rules WHERE id = ?`, id); err != nil {
			return fmt.Errorf("删除计价规则 #%d 失败: %w", id, err)
		}
		return nil
	})
}

// ResetPricingRules 清空全部非兜底计价规则，恢复到仅剩全模型免费兜底规则的初始状态。
// 返回删除的规则数。兜底规则（glob:*）与单删一致地保留。
func (s *Store) ResetPricingRules(ctx context.Context) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM pricing_rules WHERE NOT (match_kind = 'glob' AND pattern = '*')`)
		if err != nil {
			return fmt.Errorf("清空计价规则失败: %w", err)
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// SetPricingRuleEnabled 启用/停用规则。
func (s *Store) SetPricingRuleEnabled(ctx context.Context, id int64, enabled bool) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE pricing_rules SET enabled = ?, updated_at = ? WHERE id = ?`,
			boolInt(enabled), nowMillis(), id)
		if err != nil {
			return fmt.Errorf("更新计价规则 #%d 失败: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: 计价规则 #%d", ErrNotFound, id)
		}
		return nil
	})
}

// ReplaceModelsDevRules 用一批 models.dev 同步结果替换既有 models_dev 来源规则。
//
// 语义：只影响 source='models_dev' 的规则，手工规则（manual）一律保留不动，
// 这样管理员的手工覆盖不会被同步冲掉。同一 (match_kind, pattern) 若已是手工规则，
// 则跳过该条同步项并计入 skipped。
func (s *Store) ReplaceModelsDevRules(ctx context.Context, rules []PricingRule, now time.Time) (applied, skipped, removed int, err error) {
	ts := now.UTC().UnixMilli()

	err = s.Write(ctx, func(tx *sql.Tx) error {
		// 先记录本次同步覆盖的键集合。
		incoming := make(map[string]bool, len(rules))
		for _, r := range rules {
			incoming[r.MatchKind+"\x00"+r.Pattern] = true
		}

		// 读出现存手工规则的键，用于避免覆盖手工配置。
		manual := make(map[string]bool)
		rows, qerr := tx.QueryContext(ctx,
			`SELECT match_kind, pattern FROM pricing_rules WHERE source = ?`, PricingSourceManual)
		if qerr != nil {
			return fmt.Errorf("读取手工计价规则失败: %w", qerr)
		}
		for rows.Next() {
			var mk, pat string
			if serr := rows.Scan(&mk, &pat); serr != nil {
				rows.Close()
				return fmt.Errorf("扫描手工计价规则失败: %w", serr)
			}
			manual[mk+"\x00"+pat] = true
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return fmt.Errorf("遍历手工计价规则失败: %w", rerr)
		}

		stmt, perr := tx.PrepareContext(ctx,
			`INSERT INTO pricing_rules (
				match_kind, pattern, priority, enabled,
				price_input, price_output, price_reasoning, price_cached,
				price_cache_read, price_cache_creation,
				accounting_mode, billing_mode, per_image_micro_usd,
				source, models_dev_id, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(match_kind, pattern) DO UPDATE SET
				priority             = excluded.priority,
				enabled              = excluded.enabled,
				price_input          = excluded.price_input,
				price_output         = excluded.price_output,
				price_reasoning      = excluded.price_reasoning,
				price_cached         = excluded.price_cached,
				price_cache_read     = excluded.price_cache_read,
				price_cache_creation = excluded.price_cache_creation,
				accounting_mode      = excluded.accounting_mode,
				billing_mode         = excluded.billing_mode,
				per_image_micro_usd  = excluded.per_image_micro_usd,
				models_dev_id        = excluded.models_dev_id,
				updated_at           = excluded.updated_at`)
		if perr != nil {
			return fmt.Errorf("准备同步语句失败: %w", perr)
		}
		defer stmt.Close()

		for _, r := range rules {
			key := r.MatchKind + "\x00" + r.Pattern
			if manual[key] {
				skipped++
				continue
			}
			if r.AccountingMode == "" {
				r.AccountingMode = AccountingModeDefault
			}
			if r.BillingMode == "" {
				r.BillingMode = BillingModeToken
			}
			r.Source = PricingSourceModelsDev
			if verr := r.Validate(); verr != nil {
				return fmt.Errorf("同步项 %s:%s 非法: %w", r.MatchKind, r.Pattern, verr)
			}
			if _, eerr := stmt.ExecContext(ctx,
				r.MatchKind, r.Pattern, r.Priority, boolInt(r.Enabled),
				int64(r.PriceInput), int64(r.PriceOutput), int64(r.PriceReasoning), int64(r.PriceCached),
				int64(r.PriceCacheRead), int64(r.PriceCacheCreation),
				r.AccountingMode, r.BillingMode, int64(r.PerImageMicroUSD),
				PricingSourceModelsDev, r.ModelsDevID, ts, ts); eerr != nil {
				return fmt.Errorf("写入同步项 %s:%s 失败: %w", r.MatchKind, r.Pattern, eerr)
			}
			applied++
		}

		// 清理本次同步未涵盖的旧 models_dev 规则（上游已下线的模型）。
		delRows, qerr := tx.QueryContext(ctx,
			`SELECT id, match_kind, pattern FROM pricing_rules WHERE source = ?`, PricingSourceModelsDev)
		if qerr != nil {
			return fmt.Errorf("读取旧同步规则失败: %w", qerr)
		}
		var staleIDs []int64
		for delRows.Next() {
			var id int64
			var mk, pat string
			if serr := delRows.Scan(&id, &mk, &pat); serr != nil {
				delRows.Close()
				return fmt.Errorf("扫描旧同步规则失败: %w", serr)
			}
			if !incoming[mk+"\x00"+pat] {
				staleIDs = append(staleIDs, id)
			}
		}
		delRows.Close()
		if rerr := delRows.Err(); rerr != nil {
			return fmt.Errorf("遍历旧同步规则失败: %w", rerr)
		}
		for _, id := range staleIDs {
			if _, derr := tx.ExecContext(ctx, `DELETE FROM pricing_rules WHERE id = ?`, id); derr != nil {
				return fmt.Errorf("删除过期同步规则 #%d 失败: %w", id, derr)
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return applied, skipped, removed, nil
}

// SortRulesForMatching 把规则按匹配顺序排序（优先级降序，同级按 id 升序）。
// 供服务层在内存中缓存规则表后自行排序使用。
func SortRulesForMatching(rules []PricingRule) {
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority > rules[j].Priority
		}
		return rules[i].ID < rules[j].ID
	})
}
