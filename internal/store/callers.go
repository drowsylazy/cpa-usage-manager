package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// callerColumns 是 callers 表的列清单，供扫描复用。
const callerColumns = `id, display_name, enabled, created_at, updated_at`

// scanCaller 从一行结果扫描 Caller。
func scanCaller(sc interface{ Scan(...any) error }) (Caller, error) {
	var c Caller
	var enabled int
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.DisplayName, &enabled, &created, &updated); err != nil {
		return Caller{}, err
	}
	c.Enabled = enabled != 0
	c.CreatedAt = time.UnixMilli(created).UTC()
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	return c, nil
}

// ValidCallerID 校验归属 ID：仅允许字母、数字、下划线、短横线与点，1..64 字符。
// 收紧字符集是为了让 ID 能安全地出现在 URL 与日志中。
func ValidCallerID(id string) error {
	if len(id) == 0 || len(id) > 64 {
		return fmt.Errorf("caller id 长度须在 1..64 之间，得到 %d", len(id))
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return fmt.Errorf("caller id 含非法字符 %q，仅允许字母数字与 _-.", r)
		}
	}
	return nil
}

// CreateCaller 创建归属。ID 已存在时返回 ErrConflict。
func (s *Store) CreateCaller(ctx context.Context, id, displayName string) (Caller, error) {
	id = strings.TrimSpace(id)
	if err := ValidCallerID(id); err != nil {
		return Caller{}, err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = id
	}
	now := nowMillis()
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO callers (id, display_name, enabled, created_at, updated_at)
			 VALUES (?, ?, 1, ?, ?)`,
			id, displayName, now, now)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: caller %q 已存在", ErrConflict, id)
		}
		if err != nil {
			return fmt.Errorf("创建 caller 失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return Caller{}, err
	}
	return Caller{
		ID:          id,
		DisplayName: displayName,
		Enabled:     true,
		CreatedAt:   time.UnixMilli(now).UTC(),
		UpdatedAt:   time.UnixMilli(now).UTC(),
	}, nil
}

// GetCaller 读取单个归属。
func (s *Store) GetCaller(ctx context.Context, id string) (Caller, error) {
	var c Caller
	err := s.Read(ctx, func(q Querier) error {
		var e error
		c, e = scanCaller(q.QueryRowContext(ctx, `SELECT `+callerColumns+` FROM callers WHERE id = ?`, id))
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Caller{}, fmt.Errorf("%w: caller %q", ErrNotFound, id)
	}
	if err != nil {
		return Caller{}, fmt.Errorf("读取 caller %q 失败: %w", id, err)
	}
	return c, nil
}

// ListCallers 列出全部归属，按 ID 排序。
func (s *Store) ListCallers(ctx context.Context) ([]Caller, error) {
	var out []Caller
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+callerColumns+` FROM callers ORDER BY id`)
		if err != nil {
			return fmt.Errorf("列出 callers 失败: %w", err)
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			c, err := scanCaller(rows)
			if err != nil {
				return fmt.Errorf("扫描 caller 失败: %w", err)
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("遍历 callers 失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetCallerEnabled 启用/停用归属。
func (s *Store) SetCallerEnabled(ctx context.Context, id string, enabled bool) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE callers SET enabled = ?, updated_at = ? WHERE id = ?`,
			boolInt(enabled), nowMillis(), id)
		if err != nil {
			return fmt.Errorf("更新 caller %q 失败: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: caller %q", ErrNotFound, id)
		}
		return nil
	})
}

// UpdateCallerDisplayName 修改归属展示名。
func (s *Store) UpdateCallerDisplayName(ctx context.Context, id, displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return fmt.Errorf("display_name 不能为空")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE callers SET display_name = ?, updated_at = ? WHERE id = ?`,
			displayName, nowMillis(), id)
		if err != nil {
			return fmt.Errorf("更新 caller %q 失败: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: caller %q", ErrNotFound, id)
		}
		return nil
	})
}

// boolInt 把布尔转为 SQLite 的 0/1。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
