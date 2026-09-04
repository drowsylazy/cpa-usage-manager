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

// keyColumns 是 plugin_keys 表的完整列清单。
const keyColumns = `kid, key_hash, encrypted_material, pepper_id, fingerprint, principal,
	caller_scope, caller_id, label, enabled, revoked_at, expires_at,
	quota_micro_usd, daily_micro_usd, weekly_micro_usd, monthly_micro_usd,
	token_limit, daily_token_limit, weekly_token_limit, monthly_token_limit,
	max_concurrent_requests, allowed_models_json,
	spent_micro_usd, daily_cycle_key, daily_spent_micro_usd,
	weekly_cycle_key, weekly_spent_micro_usd, monthly_cycle_key, monthly_spent_micro_usd,
	tokens_used, daily_tokens_used, weekly_tokens_used, monthly_tokens_used,
	daily_requests_limit, monthly_requests_limit, daily_requests_used, monthly_requests_used,
	created_at, updated_at, last_used_at`

// scanKey 从一行结果扫描 PluginKey。
func scanKey(sc interface{ Scan(...any) error }) (PluginKey, error) {
	var k PluginKey
	var (
		enabled                          int
		revokedAt, expiresAt, lastUsedAt *int64
		quota, daily, weekly, monthly    *int64
		tokLimit, tokDaily               *int64
		tokWeekly, tokMonthly            *int64
		modelsJSON                       string
		spent, dailySpent                int64
		weeklySpent, monthlySpent        int64
		tokUsed, tokDailyUsed            int64
		tokWeeklyUsed, tokMonthlyUsed    int64
		reqDayLimit, reqMonthLimit       *int64
		reqDayUsed, reqMonthUsed         int64
		created, updated                 int64
	)
	err := sc.Scan(
		&k.KID, &k.KeyHash, &k.EncryptedMaterial, &k.PepperID, &k.Fingerprint, &k.Principal,
		&k.CallerScope, &k.CallerID, &k.Label, &enabled, &revokedAt, &expiresAt,
		&quota, &daily, &weekly, &monthly,
		&tokLimit, &tokDaily, &tokWeekly, &tokMonthly,
		&k.MaxConcurrentRequests, &modelsJSON,
		&spent, &k.DailyCycleKey, &dailySpent,
		&k.WeeklyCycleKey, &weeklySpent, &k.MonthlyCycleKey, &monthlySpent,
		&tokUsed, &tokDailyUsed, &tokWeeklyUsed, &tokMonthlyUsed,
		&reqDayLimit, &reqMonthLimit, &reqDayUsed, &reqMonthUsed,
		&created, &updated, &lastUsedAt,
	)
	if err != nil {
		return PluginKey{}, err
	}
	k.Enabled = enabled != 0
	k.RevokedAt = timePtr(revokedAt)
	k.ExpiresAt = timePtr(expiresAt)
	k.LastUsedAt = timePtr(lastUsedAt)
	k.QuotaMicroUSD = moneyPtr(quota)
	k.DailyMicroUSD = moneyPtr(daily)
	k.WeeklyMicroUSD = moneyPtr(weekly)
	k.MonthlyMicroUSD = moneyPtr(monthly)
	k.TokenLimit = int64Ptr(tokLimit)
	k.DailyTokenLimit = int64Ptr(tokDaily)
	k.WeeklyTokenLimit = int64Ptr(tokWeekly)
	k.MonthlyTokenLimit = int64Ptr(tokMonthly)
	k.SpentMicroUSD = money.Micro(spent)
	k.DailySpentMicroUSD = money.Micro(dailySpent)
	k.WeeklySpentMicroUSD = money.Micro(weeklySpent)
	k.MonthlySpentMicroUSD = money.Micro(monthlySpent)
	k.TokensUsed = tokUsed
	k.DailyTokensUsed = tokDailyUsed
	k.WeeklyTokensUsed = tokWeeklyUsed
	k.MonthlyTokensUsed = tokMonthlyUsed
	k.DailyRequestsLimit = int64Ptr(reqDayLimit)
	k.MonthlyRequestsLimit = int64Ptr(reqMonthLimit)
	k.DailyRequestsUsed = reqDayUsed
	k.MonthlyRequestsUsed = reqMonthUsed
	k.CreatedAt = time.UnixMilli(created).UTC()
	k.UpdatedAt = time.UnixMilli(updated).UTC()

	models, err := decodeModels(modelsJSON)
	if err != nil {
		return PluginKey{}, err
	}
	k.AllowedModels = models
	return k, nil
}

// InsertKeyParams 是插入一枚新 Key 所需的参数。
// 明文由 service 层生成，store 只接收派生出的安全材料。
type InsertKeyParams struct {
	KID               string
	KeyHash           []byte
	EncryptedMaterial []byte
	PepperID          string
	Fingerprint       string

	Principal   string
	CallerScope string
	CallerID    string
	Label       string

	ExpiresAt *time.Time

	QuotaMicroUSD   *money.Micro
	DailyMicroUSD   *money.Micro
	WeeklyMicroUSD  *money.Micro
	MonthlyMicroUSD *money.Micro

	TokenLimit        *int64
	DailyTokenLimit   *int64
	WeeklyTokenLimit  *int64
	MonthlyTokenLimit *int64

	DailyRequestsLimit   *int64
	MonthlyRequestsLimit *int64

	MaxConcurrentRequests int
	AllowedModels         []string
}

// Validate 校验插入参数。
func (p *InsertKeyParams) Validate() error {
	if strings.TrimSpace(p.KID) == "" {
		return errors.New("kid 不能为空")
	}
	if len(p.KeyHash) == 0 {
		return errors.New("key_hash 不能为空")
	}
	if strings.TrimSpace(p.PepperID) == "" {
		return errors.New("pepper_id 不能为空")
	}
	switch p.CallerScope {
	case CallerScopeCaller, CallerScopeKey:
	default:
		return fmt.Errorf("caller_scope 须为 %s 或 %s，得到 %q", CallerScopeCaller, CallerScopeKey, p.CallerScope)
	}
	if err := ValidCallerID(p.CallerID); err != nil {
		return err
	}
	if p.MaxConcurrentRequests < 0 {
		return fmt.Errorf("max_concurrent_requests 不能为负，得到 %d", p.MaxConcurrentRequests)
	}
	for name, q := range map[string]*money.Micro{
		"quota_micro_usd":   p.QuotaMicroUSD,
		"daily_micro_usd":   p.DailyMicroUSD,
		"weekly_micro_usd":  p.WeeklyMicroUSD,
		"monthly_micro_usd": p.MonthlyMicroUSD,
	} {
		if q != nil && *q < 0 {
			return fmt.Errorf("%s 不能为负，得到 %d", name, *q)
		}
	}
	for name, t := range map[string]*int64{
		"token_limit":            p.TokenLimit,
		"daily_token_limit":      p.DailyTokenLimit,
		"weekly_token_limit":     p.WeeklyTokenLimit,
		"monthly_token_limit":    p.MonthlyTokenLimit,
		"daily_requests_limit":   p.DailyRequestsLimit,
		"monthly_requests_limit": p.MonthlyRequestsLimit,
	} {
		if t != nil && *t < 0 {
			return fmt.Errorf("%s 不能为负，得到 %d", name, *t)
		}
	}
	return nil
}

// InsertKey 写入一枚新 Key。
func (s *Store) InsertKey(ctx context.Context, p InsertKeyParams) (PluginKey, error) {
	if err := p.Validate(); err != nil {
		return PluginKey{}, err
	}
	modelsJSON, err := encodeModels(p.AllowedModels)
	if err != nil {
		return PluginKey{}, err
	}
	now := nowMillis()

	err = s.Write(ctx, func(tx *sql.Tx) error {
		// caller 必须已存在：外键约束会拦截，但显式检查能给出更清楚的错误。
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM callers WHERE id = ?`, p.CallerID).Scan(&exists); err != nil {
			return fmt.Errorf("校验 caller 失败: %w", err)
		}
		if exists == 0 {
			return fmt.Errorf("%w: caller %q 不存在", ErrNotFound, p.CallerID)
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO plugin_keys (
				kid, key_hash, encrypted_material, pepper_id, fingerprint, principal,
				caller_scope, caller_id, label, enabled, revoked_at, expires_at,
				quota_micro_usd, daily_micro_usd, weekly_micro_usd, monthly_micro_usd,
				token_limit, daily_token_limit, weekly_token_limit, monthly_token_limit,
				daily_requests_limit, monthly_requests_limit,
				max_concurrent_requests, allowed_models_json,
				spent_micro_usd, daily_cycle_key, daily_spent_micro_usd,
				weekly_cycle_key, weekly_spent_micro_usd,
				monthly_cycle_key, monthly_spent_micro_usd,
				tokens_used, daily_tokens_used, weekly_tokens_used, monthly_tokens_used,
				daily_requests_used, monthly_requests_used,
				created_at, updated_at, last_used_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			          ?, ?, 0, '', 0, '', 0, '', 0, 0, 0, 0, 0, 0, 0, ?, ?, NULL)`,
			p.KID, p.KeyHash, p.EncryptedMaterial, p.PepperID, p.Fingerprint, p.Principal,
			p.CallerScope, p.CallerID, p.Label, millisPtr(p.ExpiresAt),
			microPtr(p.QuotaMicroUSD), microPtr(p.DailyMicroUSD),
			microPtr(p.WeeklyMicroUSD), microPtr(p.MonthlyMicroUSD),
			countPtr(p.TokenLimit), countPtr(p.DailyTokenLimit),
			countPtr(p.WeeklyTokenLimit), countPtr(p.MonthlyTokenLimit),
			countPtr(p.DailyRequestsLimit), countPtr(p.MonthlyRequestsLimit),
			p.MaxConcurrentRequests, modelsJSON, now, now)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: Key %q 已存在", ErrConflict, p.KID)
		}
		if err != nil {
			return fmt.Errorf("写入 Key 失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return PluginKey{}, err
	}
	return s.GetKey(ctx, p.KID)
}

// GetKey 按 kid 读取 Key（含安全材料，仅限服务端内部使用）。
func (s *Store) GetKey(ctx context.Context, kid string) (PluginKey, error) {
	var k PluginKey
	err := s.Read(ctx, func(q Querier) error {
		var e error
		k, e = scanKey(q.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM plugin_keys WHERE kid = ?`, kid))
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PluginKey{}, fmt.Errorf("%w: Key %q", ErrNotFound, kid)
	}
	if err != nil {
		return PluginKey{}, fmt.Errorf("读取 Key %q 失败: %w", kid, err)
	}
	return k, nil
}

// FindKeyByCallerScope 按 caller_scope 读取一个启用中的 Key（caller 归属模式）。
// 只取 Usable 语义的 Key（启用、未撤销、未过期）：该查询是 model.route 元数据
// 兜底鉴权的依据，已撤销/禁用/过期的 Key 不能因排序最新而被采信、遮蔽同
// scope 下较旧的有效 Key。
func (s *Store) FindKeyByCallerScope(ctx context.Context, scope string) (PluginKey, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return PluginKey{}, fmt.Errorf("%w: 缺少 caller_scope", ErrNotFound)
	}
	var k PluginKey
	err := s.Read(ctx, func(q Querier) error {
		var e error
		k, e = scanKey(q.QueryRowContext(ctx,
			`SELECT `+keyColumns+` FROM plugin_keys
			 WHERE caller_scope = ? AND enabled = 1 AND revoked_at IS NULL
			   AND (expires_at IS NULL OR expires_at > ?)
			 ORDER BY created_at DESC, kid LIMIT 1`, scope, nowMillis()))
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PluginKey{}, fmt.Errorf("%w: caller_scope %q", ErrNotFound, scope)
	}
	if err != nil {
		return PluginKey{}, fmt.Errorf("读取 caller_scope %q 的 Key 失败: %w", scope, err)
	}
	return k, nil
}

// KeyFilter 是列出 Key 的筛选条件。
type KeyFilter struct {
	CallerID string
	// OnlyActive 为 true 时只返回启用且未撤销的 Key（旧口径，等价 Status=="active"）。
	OnlyActive bool
	// Status 按 keyStatus 语义过滤：active/disabled/revoked/expired；空为不过滤。
	Status string
	// Search 对 kid / label / principal 做子串匹配。
	Search string
	Limit  int
	Offset int
}

// keyFilterWhere 组装 caller/search/status 三类过滤条件。
// nowMS 仅被 active/expired 分支使用，调用方统一取同一时刻保证两处一致。
func keyFilterWhere(f KeyFilter, nowMS int64) ([]string, []any) {
	var where []string
	var args []any
	if f.CallerID != "" {
		where = append(where, `caller_id = ?`)
		args = append(args, f.CallerID)
	}
	if f.OnlyActive {
		// 旧口径：只看 enabled/revoked，不看 expires——通知扫描靠它发现
		// 「已过期但形式上启用」的 Key 来发告警，不能并入下面的 active。
		where = append(where, `enabled = 1 AND revoked_at IS NULL`)
	}
	switch f.Status {
	case "active":
		where = append(where, `enabled = 1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`)
		args = append(args, nowMS)
	case "disabled":
		where = append(where, `enabled = 0 AND revoked_at IS NULL`)
	case "revoked":
		where = append(where, `revoked_at IS NOT NULL`)
	case "expired":
		where = append(where, `enabled = 1 AND revoked_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ?`)
		args = append(args, nowMS)
	}
	if t := strings.TrimSpace(f.Search); t != "" {
		where = append(where, `(kid LIKE ? OR label LIKE ? OR principal LIKE ?)`)
		pat := "%" + t + "%"
		args = append(args, pat, pat, pat)
	}
	return where, args
}

// ListKeys 按条件列出 Key，返回结果与匹配总数。
func (s *Store) ListKeys(ctx context.Context, f KeyFilter) ([]PluginKey, int64, error) {
	nowMS := time.Now().UTC().UnixMilli()
	where, args := keyFilterWhere(f, nowMS)
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	var out []PluginKey
	err := s.Read(ctx, func(qr Querier) error {
		if err := qr.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM plugin_keys`+clause, args...).Scan(&total); err != nil {
			return fmt.Errorf("统计 Key 数量失败: %w", err)
		}
		q := `SELECT ` + keyColumns + ` FROM plugin_keys` + clause + ` ORDER BY created_at DESC, kid`
		if f.Limit > 0 {
			q += fmt.Sprintf(" LIMIT %d OFFSET %d", f.Limit, maxInt(0, f.Offset))
		}
		rows, err := qr.QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("列出 Key 失败: %w", err)
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			k, err := scanKey(rows)
			if err != nil {
				return fmt.Errorf("扫描 Key 失败: %w", err)
			}
			out = append(out, k)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("遍历 Key 失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// CountKeysByStatus 按四种 keyStatus 统计 Key 数量（只受 caller/search 过滤，
// 不受 status/paging 影响），供面板在服务端分页后仍能展示全量状态徽标。
func (s *Store) CountKeysByStatus(ctx context.Context, f KeyFilter) (map[string]int64, error) {
	nowMS := time.Now().UTC().UnixMilli()
	where, args := keyFilterWhere(KeyFilter{CallerID: f.CallerID, Search: f.Search}, nowMS)
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	q := `SELECT
		COALESCE(SUM(CASE WHEN enabled = 1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN enabled = 0 AND revoked_at IS NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN revoked_at IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN enabled = 1 AND revoked_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ? THEN 1 ELSE 0 END),0)
		FROM plugin_keys` + clause
	var active, disabled, revoked, expired int64
	err := s.Read(ctx, func(qr Querier) error {
		return qr.QueryRowContext(ctx, q, append([]any{nowMS, nowMS}, args...)...).
			Scan(&active, &disabled, &revoked, &expired)
	})
	if err != nil {
		return nil, fmt.Errorf("统计 Key 状态失败: %w", err)
	}
	return map[string]int64{"active": active, "disabled": disabled, "revoked": revoked, "expired": expired}, nil
}

// KeyUpdate 是对 Key 的部分更新。nil 字段表示不改动。
type KeyUpdate struct {
	Label       *string
	Principal   *string
	Enabled     *bool
	ExpiresAt   **time.Time // 双层指针：外层 nil=不改，内层 nil=清空
	CallerID    *string
	CallerScope *string

	QuotaMicroUSD   **money.Micro
	DailyMicroUSD   **money.Micro
	WeeklyMicroUSD  **money.Micro
	MonthlyMicroUSD **money.Micro

	TokenLimit        **int64
	DailyTokenLimit   **int64
	WeeklyTokenLimit  **int64
	MonthlyTokenLimit **int64

	DailyRequestsLimit   **int64
	MonthlyRequestsLimit **int64

	MaxConcurrentRequests *int
	AllowedModels         *[]string
}

// UpdateKey 部分更新 Key 的策略字段。安全材料不可通过此方法改动。
func (s *Store) UpdateKey(ctx context.Context, kid string, u KeyUpdate) (PluginKey, error) {
	var sets []string
	var args []any

	add := func(expr string, val any) {
		sets = append(sets, expr)
		args = append(args, val)
	}
	if u.Label != nil {
		add(`label = ?`, strings.TrimSpace(*u.Label))
	}
	if u.Principal != nil {
		add(`principal = ?`, strings.TrimSpace(*u.Principal))
	}
	if u.Enabled != nil {
		add(`enabled = ?`, boolInt(*u.Enabled))
	}
	if u.ExpiresAt != nil {
		add(`expires_at = ?`, millisPtr(*u.ExpiresAt))
	}
	if u.CallerID != nil {
		if err := ValidCallerID(*u.CallerID); err != nil {
			return PluginKey{}, err
		}
		add(`caller_id = ?`, *u.CallerID)
	}
	if u.CallerScope != nil {
		switch *u.CallerScope {
		case CallerScopeCaller, CallerScopeKey:
		default:
			return PluginKey{}, fmt.Errorf("caller_scope 须为 %s 或 %s，得到 %q",
				CallerScopeCaller, CallerScopeKey, *u.CallerScope)
		}
		add(`caller_scope = ?`, *u.CallerScope)
	}
	quotaFields := []struct {
		name string
		col  string
		val  **money.Micro
	}{
		{"quota_micro_usd", `quota_micro_usd`, u.QuotaMicroUSD},
		{"daily_micro_usd", `daily_micro_usd`, u.DailyMicroUSD},
		{"weekly_micro_usd", `weekly_micro_usd`, u.WeeklyMicroUSD},
		{"monthly_micro_usd", `monthly_micro_usd`, u.MonthlyMicroUSD},
	}
	for _, qf := range quotaFields {
		if qf.val == nil {
			continue
		}
		if *qf.val != nil && **qf.val < 0 {
			return PluginKey{}, fmt.Errorf("%s 不能为负，得到 %d", qf.name, **qf.val)
		}
		add(qf.col+` = ?`, microPtr(*qf.val))
	}
	tokenFields := []struct {
		name string
		col  string
		val  **int64
	}{
		{"token_limit", `token_limit`, u.TokenLimit},
		{"daily_token_limit", `daily_token_limit`, u.DailyTokenLimit},
		{"weekly_token_limit", `weekly_token_limit`, u.WeeklyTokenLimit},
		{"monthly_token_limit", `monthly_token_limit`, u.MonthlyTokenLimit},
	}
	for _, tf := range tokenFields {
		if tf.val == nil {
			continue
		}
		if *tf.val != nil && **tf.val < 0 {
			return PluginKey{}, fmt.Errorf("%s 不能为负，得到 %d", tf.name, **tf.val)
		}
		add(tf.col+` = ?`, countPtr(*tf.val))
	}
	requestFields := []struct {
		name string
		col  string
		val  **int64
	}{
		{"daily_requests_limit", `daily_requests_limit`, u.DailyRequestsLimit},
		{"monthly_requests_limit", `monthly_requests_limit`, u.MonthlyRequestsLimit},
	}
	for _, rf := range requestFields {
		if rf.val == nil {
			continue
		}
		if *rf.val != nil && **rf.val < 0 {
			return PluginKey{}, fmt.Errorf("%s 不能为负，得到 %d", rf.name, **rf.val)
		}
		add(rf.col+` = ?`, countPtr(*rf.val))
	}
	if u.MaxConcurrentRequests != nil {
		if *u.MaxConcurrentRequests < 0 {
			return PluginKey{}, fmt.Errorf("max_concurrent_requests 不能为负，得到 %d", *u.MaxConcurrentRequests)
		}
		add(`max_concurrent_requests = ?`, *u.MaxConcurrentRequests)
	}
	if u.AllowedModels != nil {
		j, err := encodeModels(*u.AllowedModels)
		if err != nil {
			return PluginKey{}, err
		}
		add(`allowed_models_json = ?`, j)
	}

	if len(sets) == 0 {
		return s.GetKey(ctx, kid)
	}
	sets = append(sets, `updated_at = ?`)
	args = append(args, nowMillis(), kid)

	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE plugin_keys SET `+strings.Join(sets, ", ")+` WHERE kid = ?`, args...)
		if err != nil {
			return fmt.Errorf("更新 Key %q 失败: %w", kid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: Key %q", ErrNotFound, kid)
		}
		return nil
	})
	if err != nil {
		return PluginKey{}, err
	}
	return s.GetKey(ctx, kid)
}

// RevokeKey 撤销 Key（不可逆，保留历史记录）。
func (s *Store) RevokeKey(ctx context.Context, kid string) error {
	now := nowMillis()
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE plugin_keys SET revoked_at = ?, enabled = 0, updated_at = ?
			 WHERE kid = ? AND revoked_at IS NULL`,
			now, now, kid)
		if err != nil {
			return fmt.Errorf("撤销 Key %q 失败: %w", kid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// 区分「不存在」与「已撤销」。
			var revoked *int64
			err := tx.QueryRowContext(ctx, `SELECT revoked_at FROM plugin_keys WHERE kid = ?`, kid).Scan(&revoked)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: Key %q", ErrNotFound, kid)
			}
			if err != nil {
				return fmt.Errorf("读取 Key %q 状态失败: %w", kid, err)
			}
			// 已撤销：幂等成功。
		}
		return nil
	})
}

// DeleteKey 永久删除 Key 记录。历史 requests/audit 保留（其 key_id 变为悬挂引用）。
func (s *Store) DeleteKey(ctx context.Context, kid string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM plugin_keys WHERE kid = ?`, kid)
		if err != nil {
			return fmt.Errorf("删除 Key %q 失败: %w", kid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: Key %q", ErrNotFound, kid)
		}
		return nil
	})
}

// RotateKeyMaterial 原子替换 Key 的安全材料（轮换）。
// 旧明文随即失效，因为校验依据的 key_hash 已更新。
func (s *Store) RotateKeyMaterial(ctx context.Context, kid string, hash, encrypted []byte, pepperID, fingerprint string) error {
	if len(hash) == 0 || strings.TrimSpace(pepperID) == "" {
		return errors.New("轮换需要新的 key_hash 与 pepper_id")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE plugin_keys
			 SET key_hash = ?, encrypted_material = ?, pepper_id = ?, fingerprint = ?, updated_at = ?
			 WHERE kid = ? AND revoked_at IS NULL`,
			hash, encrypted, pepperID, fingerprint, nowMillis(), kid)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: 新 key_hash 与既有 Key 冲突", ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("轮换 Key %q 失败: %w", kid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: Key %q（或已撤销）", ErrNotFound, kid)
		}
		return nil
	})
}

// TouchKeysLastUsed 在单个写事务里批量更新多个 Key 的最近使用时间。
// 鉴权热路径经服务层挂起表聚合后调用：每心跳周期最多一个写事务，
// 替代此前每次鉴权一个。失败不影响调用方语义，由上层忽略。
func (s *Store) TouchKeysLastUsed(ctx context.Context, touches map[string]int64) error {
	if len(touches) == 0 {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		kids := make([]string, 0, len(touches))
		var cases strings.Builder
		args := make([]any, 0, len(touches)*2)
		for kid, ms := range touches {
			kids = append(kids, kid)
			cases.WriteString(` WHEN kid = ? THEN ?`)
			args = append(args, kid, ms)
		}
		for _, kid := range kids {
			args = append(args, kid)
		}
		q := `UPDATE plugin_keys SET last_used_at = CASE` + cases.String() +
			` ELSE last_used_at END WHERE kid IN (` +
			strings.Repeat("?,", len(kids)-1) + `?)`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("批量更新 Key 使用时间失败: %w", err)
		}
		return nil
	})
}

// CountKeysByPepper 统计各 pepper 代际下的 Key 数量，用于轮换进度展示。
func (s *Store) CountKeysByPepper(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64)
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT pepper_id, COUNT(*) FROM plugin_keys GROUP BY pepper_id ORDER BY pepper_id`)
		if err != nil {
			return fmt.Errorf("统计 pepper 分布失败: %w", err)
		}
		defer rows.Close()
		clear(out)
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				return fmt.Errorf("扫描 pepper 分布失败: %w", err)
			}
			out[id] = n
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("遍历 pepper 分布失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
