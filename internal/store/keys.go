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
	max_concurrent_requests, allowed_models_json,
	spent_micro_usd, daily_cycle_key, daily_spent_micro_usd,
	weekly_cycle_key, weekly_spent_micro_usd, monthly_cycle_key, monthly_spent_micro_usd,
	created_at, updated_at, last_used_at`

// scanKey 从一行结果扫描 PluginKey。
func scanKey(sc interface{ Scan(...any) error }) (PluginKey, error) {
	var k PluginKey
	var (
		enabled                          int
		revokedAt, expiresAt, lastUsedAt *int64
		quota, daily, weekly, monthly    *int64
		modelsJSON                       string
		spent, dailySpent                int64
		weeklySpent, monthlySpent        int64
		created, updated                 int64
	)
	err := sc.Scan(
		&k.KID, &k.KeyHash, &k.EncryptedMaterial, &k.PepperID, &k.Fingerprint, &k.Principal,
		&k.CallerScope, &k.CallerID, &k.Label, &enabled, &revokedAt, &expiresAt,
		&quota, &daily, &weekly, &monthly,
		&k.MaxConcurrentRequests, &modelsJSON,
		&spent, &k.DailyCycleKey, &dailySpent,
		&k.WeeklyCycleKey, &weeklySpent, &k.MonthlyCycleKey, &monthlySpent,
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
	k.SpentMicroUSD = money.Micro(spent)
	k.DailySpentMicroUSD = money.Micro(dailySpent)
	k.WeeklySpentMicroUSD = money.Micro(weeklySpent)
	k.MonthlySpentMicroUSD = money.Micro(monthlySpent)
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
				max_concurrent_requests, allowed_models_json,
				spent_micro_usd, daily_cycle_key, daily_spent_micro_usd,
				weekly_cycle_key, weekly_spent_micro_usd,
				monthly_cycle_key, monthly_spent_micro_usd,
				created_at, updated_at, last_used_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, NULL, ?, ?, ?, ?, ?, ?, ?,
			          0, '', 0, '', 0, '', 0, ?, ?, NULL)`,
			p.KID, p.KeyHash, p.EncryptedMaterial, p.PepperID, p.Fingerprint, p.Principal,
			p.CallerScope, p.CallerID, p.Label, millisPtr(p.ExpiresAt),
			microPtr(p.QuotaMicroUSD), microPtr(p.DailyMicroUSD),
			microPtr(p.WeeklyMicroUSD), microPtr(p.MonthlyMicroUSD),
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
	row := s.readDB.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM plugin_keys WHERE kid = ?`, kid)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PluginKey{}, fmt.Errorf("%w: Key %q", ErrNotFound, kid)
	}
	if err != nil {
		return PluginKey{}, fmt.Errorf("读取 Key %q 失败: %w", kid, err)
	}
	return k, nil
}

// KeyFilter 是列出 Key 的筛选条件。
type KeyFilter struct {
	CallerID string
	// OnlyActive 为 true 时只返回启用且未撤销的 Key。
	OnlyActive bool
	// Search 对 kid / label / principal 做子串匹配。
	Search string
	Limit  int
	Offset int
}

// ListKeys 按条件列出 Key，返回结果与匹配总数。
func (s *Store) ListKeys(ctx context.Context, f KeyFilter) ([]PluginKey, int64, error) {
	var where []string
	var args []any
	if f.CallerID != "" {
		where = append(where, `caller_id = ?`)
		args = append(args, f.CallerID)
	}
	if f.OnlyActive {
		where = append(where, `enabled = 1 AND revoked_at IS NULL`)
	}
	if t := strings.TrimSpace(f.Search); t != "" {
		where = append(where, `(kid LIKE ? OR label LIKE ? OR principal LIKE ?)`)
		pat := "%" + t + "%"
		args = append(args, pat, pat, pat)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plugin_keys`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计 Key 数量失败: %w", err)
	}

	q := `SELECT ` + keyColumns + ` FROM plugin_keys` + clause + ` ORDER BY created_at DESC, kid`
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d OFFSET %d", f.Limit, maxInt(0, f.Offset))
	}
	rows, err := s.readDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("列出 Key 失败: %w", err)
	}
	defer rows.Close()
	var out []PluginKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("扫描 Key 失败: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历 Key 失败: %w", err)
	}
	return out, total, nil
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

// TouchKeyLastUsed 更新 Key 的最近使用时间。
// 这是鉴权热路径上的写操作，失败不应影响鉴权结果，由调用方决定是否忽略错误。
func (s *Store) TouchKeyLastUsed(ctx context.Context, kid string, at time.Time) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE plugin_keys SET last_used_at = ? WHERE kid = ?`,
			at.UTC().UnixMilli(), kid)
		if err != nil {
			return fmt.Errorf("更新 Key %q 使用时间失败: %w", kid, err)
		}
		return nil
	})
}

// CountKeysByPepper 统计各 pepper 代际下的 Key 数量，用于轮换进度展示。
func (s *Store) CountKeysByPepper(ctx context.Context) (map[string]int64, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT pepper_id, COUNT(*) FROM plugin_keys GROUP BY pepper_id ORDER BY pepper_id`)
	if err != nil {
		return nil, fmt.Errorf("统计 pepper 分布失败: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("扫描 pepper 分布失败: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 pepper 分布失败: %w", err)
	}
	return out, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
