package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ModelRoute 是一条模型路由（集合别名）配置。Rule 列存路由规则脚本
// （语法见 internal/routelang），保存期已编译校验；store 不解释脚本。
type ModelRoute struct {
	ID              int64  `json:"id"`
	Alias           string `json:"alias"`
	Rule            string `json:"rule"`
	CooldownSeconds int64  `json:"cooldown_seconds"`
	// CooldownPolicy 决定候选目标全部冷却时的行为：
	// block（默认）= 拒绝请求（upstream_error）；
	// force = 忽略冷却按原链顺序照打（冷却只是进程内启发式，宁可赌一次）。
	CooldownPolicy string `json:"cooldown_policy"`
	PricingMode    string `json:"pricing_mode"` // target|alias
	Enabled        bool   `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Refs 是规则引用的目标模型名（编译期静态提取），由 service 层填充，
	// 供面板展示与递归检查；不落库。
	Refs []string `json:"refs,omitempty"`
}

const modelRouteColumns = `id,alias,rule,cooldown_seconds,cooldown_policy,pricing_mode,enabled,created_at,updated_at`

func scanModelRoute(sc interface{ Scan(...any) error }) (ModelRoute, error) {
	var r ModelRoute
	var enabled int
	var created, updated int64
	if err := sc.Scan(&r.ID, &r.Alias, &r.Rule, &r.CooldownSeconds, &r.CooldownPolicy, &r.PricingMode, &enabled, &created, &updated); err != nil {
		return ModelRoute{}, err
	}
	if r.CooldownPolicy == "" {
		r.CooldownPolicy = "block"
	}
	r.Enabled = enabled == 1
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.UpdatedAt = time.UnixMilli(updated).UTC()
	return r, nil
}

// ListModelRoutes 返回全部路由，按 id 升序。
func (s *Store) ListModelRoutes(ctx context.Context) ([]ModelRoute, error) {
	var out []ModelRoute
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+modelRouteColumns+` FROM model_routes ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanModelRoute(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("列出模型路由失败: %w", err)
	}
	return out, nil
}

// GetModelRoute 按 id 读取单条路由。
func (s *Store) GetModelRoute(ctx context.Context, id int64) (ModelRoute, error) {
	var r ModelRoute
	err := s.Read(ctx, func(q Querier) error {
		var err error
		r, err = scanModelRoute(q.QueryRowContext(ctx,
			`SELECT `+modelRouteColumns+` FROM model_routes WHERE id = ?`, id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ModelRoute{}, fmt.Errorf("%w: model_route %d", ErrNotFound, id)
	}
	if err != nil {
		return ModelRoute{}, fmt.Errorf("读取模型路由 %d 失败: %w", id, err)
	}
	return r, nil
}

// InsertModelRoute 新增路由，返回新 id。
func (s *Store) InsertModelRoute(ctx context.Context, r ModelRoute) (int64, error) {
	en := 0
	if r.Enabled {
		en = 1
	}
	ts := nowMillis()
	var id int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO model_routes (alias,rule,cooldown_seconds,cooldown_policy,pricing_mode,enabled,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
			r.Alias, r.Rule, r.CooldownSeconds, r.CooldownPolicy, r.PricingMode, en, ts, ts)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("新增模型路由失败: %w", err)
	}
	s.notifyRoutesChanged()
	return id, nil
}

// UpdateModelRoute 覆盖路由的可编辑字段。
func (s *Store) UpdateModelRoute(ctx context.Context, id int64, r ModelRoute) error {
	en := 0
	if r.Enabled {
		en = 1
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE model_routes SET alias=?, rule=?, cooldown_seconds=?, cooldown_policy=?, pricing_mode=?, enabled=?, updated_at=? WHERE id=?`,
			r.Alias, r.Rule, r.CooldownSeconds, r.CooldownPolicy, r.PricingMode, en, nowMillis(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: model_route %d", ErrNotFound, id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("更新模型路由 %d 失败: %w", id, err)
	}
	s.notifyRoutesChanged()
	return nil
}

// DeleteModelRoute 删除路由。
func (s *Store) DeleteModelRoute(ctx context.Context, id int64) error {
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM model_routes WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: model_route %d", ErrNotFound, id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("删除模型路由 %d 失败: %w", id, err)
	}
	s.notifyRoutesChanged()
	return nil
}

// SetModelRouteEnabled 只翻转启用开关（卡片快捷开关路径）。
func (s *Store) SetModelRouteEnabled(ctx context.Context, id int64, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE model_routes SET enabled=?, updated_at=? WHERE id=?`, en, nowMillis(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: model_route %d", ErrNotFound, id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("切换模型路由 %d 状态失败: %w", id, err)
	}
	s.notifyRoutesChanged()
	return nil
}

// 模型路由变更钩子 ---------------------------------------------------------

// SetRoutesChangedHandler 注册路由表变更回调（写事务提交成功后调用）。
// 服务层用它失效内存中的路由快照；回调必须快速返回、不得阻塞。
func (s *Store) SetRoutesChangedHandler(fn func()) {
	s.routesHookMu.Lock()
	s.onRoutesChanged = fn
	s.routesHookMu.Unlock()
}

func (s *Store) notifyRoutesChanged() {
	s.routesHookMu.Lock()
	fn := s.onRoutesChanged
	s.routesHookMu.Unlock()
	if fn != nil {
		fn()
	}
}
