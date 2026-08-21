package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AuthQuotaSnapshot 是一份 OAuth 认证额度快照。
//
// Snapshot 里只允许出现宿主已清洗过的展示信息：绝不含上游 API Key
// 明文或密文，也不含任何可用于重建凭据的字段。
type AuthQuotaSnapshot struct {
	Provider  string         `json:"provider"`
	AuthID    string         `json:"auth_id"`
	Status    string         `json:"status"`
	FetchedAt time.Time      `json:"fetched_at"`
	Snapshot  map[string]any `json:"snapshot,omitempty"`
}

// AuthQuotaBaseline 是某个认证账号在一个额度窗口内的用量基线，
// 用于把「宿主观测到的累计值」换算成「本周期内的增量」以做预测。
type AuthQuotaBaseline struct {
	Provider  string    `json:"provider"`
	AuthID    string    `json:"auth_id"`
	WindowID  string    `json:"window_id"`
	CycleKey  string    `json:"cycle_key"`
	Observed  int64     `json:"observed"`
	Baseline  int64     `json:"baseline"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Delta 返回本周期内的增量（observed - baseline），下界为 0。
func (b AuthQuotaBaseline) Delta() int64 {
	if b.Observed <= b.Baseline {
		return 0
	}
	return b.Observed - b.Baseline
}

// UpsertAuthQuotaSnapshot 写入或替换一份认证额度快照。
func (s *Store) UpsertAuthQuotaSnapshot(ctx context.Context, snap AuthQuotaSnapshot) error {
	if strings.TrimSpace(snap.Provider) == "" || strings.TrimSpace(snap.AuthID) == "" {
		return errors.New("provider 与 auth_id 不能为空")
	}
	payload := "{}"
	if snap.Snapshot != nil {
		b, err := json.Marshal(snap.Snapshot)
		if err != nil {
			return fmt.Errorf("序列化认证额度快照失败: %w", err)
		}
		payload = string(b)
	}
	at := snap.FetchedAt
	if at.IsZero() {
		at = time.Now()
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO auth_quota_snapshots (provider, auth_id, snapshot_json, fetched_at, status)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(provider, auth_id) DO UPDATE SET
			   snapshot_json = excluded.snapshot_json,
			   fetched_at    = excluded.fetched_at,
			   status        = excluded.status`,
			snap.Provider, snap.AuthID, payload, at.UTC().UnixMilli(), snap.Status)
		if err != nil {
			return fmt.Errorf("写入认证额度快照失败: %w", err)
		}
		return nil
	})
}

// ListAuthQuotaSnapshots 列出全部认证额度快照，按 provider/auth_id 排序。
func (s *Store) ListAuthQuotaSnapshots(ctx context.Context) ([]AuthQuotaSnapshot, error) {
	var out []AuthQuotaSnapshot
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT provider, auth_id, snapshot_json, fetched_at, status
			 FROM auth_quota_snapshots ORDER BY provider, auth_id`)
		if err != nil {
			return fmt.Errorf("列出认证额度快照失败: %w", err)
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var snap AuthQuotaSnapshot
			var payload string
			var fetched int64
			if err := rows.Scan(&snap.Provider, &snap.AuthID, &payload, &fetched, &snap.Status); err != nil {
				return fmt.Errorf("扫描认证额度快照失败: %w", err)
			}
			snap.FetchedAt = time.UnixMilli(fetched).UTC()
			if strings.TrimSpace(payload) != "" && payload != "{}" {
				// 快照内容由宿主提供，解析失败不应让整个接口失败。
				_ = json.Unmarshal([]byte(payload), &snap.Snapshot)
			}
			out = append(out, snap)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("遍历认证额度快照失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteAuthQuotaSnapshot 删除一份快照（认证账号下线时调用）。
func (s *Store) DeleteAuthQuotaSnapshot(ctx context.Context, provider, authID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM auth_quota_snapshots WHERE provider = ? AND auth_id = ?`,
			provider, authID); err != nil {
			return fmt.Errorf("删除认证额度快照失败: %w", err)
		}
		return nil
	})
}

// ObserveAuthQuotaWindow 记录一次窗口观测值。
//
// 语义：同一 cycle_key 内第一次观测确立 baseline；此后只更新 observed。
// observed 回退（宿主侧计数器重置）时把 baseline 一起下调，避免 Delta 变负。
func (s *Store) ObserveAuthQuotaWindow(ctx context.Context, b AuthQuotaBaseline) (AuthQuotaBaseline, error) {
	if strings.TrimSpace(b.Provider) == "" || strings.TrimSpace(b.AuthID) == "" {
		return AuthQuotaBaseline{}, errors.New("provider 与 auth_id 不能为空")
	}
	if strings.TrimSpace(b.WindowID) == "" || strings.TrimSpace(b.CycleKey) == "" {
		return AuthQuotaBaseline{}, errors.New("window_id 与 cycle_key 不能为空")
	}
	if b.Observed < 0 {
		return AuthQuotaBaseline{}, fmt.Errorf("observed 不能为负，得到 %d", b.Observed)
	}
	at := b.UpdatedAt
	if at.IsZero() {
		at = time.Now()
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO auth_quota_window_baselines
			   (provider, auth_id, window_id, cycle_key, observed, baseline, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(provider, auth_id, window_id, cycle_key) DO UPDATE SET
			   observed   = excluded.observed,
			   baseline   = MIN(auth_quota_window_baselines.baseline, excluded.observed),
			   updated_at = excluded.updated_at`,
			b.Provider, b.AuthID, b.WindowID, b.CycleKey, b.Observed, b.Observed, at.UTC().UnixMilli())
		if err != nil {
			return fmt.Errorf("记录认证额度窗口观测失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return AuthQuotaBaseline{}, err
	}
	return s.GetAuthQuotaWindow(ctx, b.Provider, b.AuthID, b.WindowID, b.CycleKey)
}

// GetAuthQuotaWindow 读取一条窗口基线。
func (s *Store) GetAuthQuotaWindow(ctx context.Context, provider, authID, windowID, cycleKey string) (AuthQuotaBaseline, error) {
	var out AuthQuotaBaseline
	var updated int64
	err := s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx,
			`SELECT provider, auth_id, window_id, cycle_key, observed, baseline, updated_at
			 FROM auth_quota_window_baselines
			 WHERE provider = ? AND auth_id = ? AND window_id = ? AND cycle_key = ?`,
			provider, authID, windowID, cycleKey).
			Scan(&out.Provider, &out.AuthID, &out.WindowID, &out.CycleKey, &out.Observed, &out.Baseline, &updated)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return AuthQuotaBaseline{}, fmt.Errorf("%w: 认证额度窗口 %s/%s/%s", ErrNotFound, provider, authID, windowID)
	}
	if err != nil {
		return AuthQuotaBaseline{}, fmt.Errorf("读取认证额度窗口失败: %w", err)
	}
	out.UpdatedAt = time.UnixMilli(updated).UTC()
	return out, nil
}

// ListAuthQuotaWindows 列出窗口基线；provider/authID 为空表示不过滤。
func (s *Store) ListAuthQuotaWindows(ctx context.Context, provider, authID string) ([]AuthQuotaBaseline, error) {
	var where []string
	var args []any
	if provider != "" {
		where = append(where, `provider = ?`)
		args = append(args, provider)
	}
	if authID != "" {
		where = append(where, `auth_id = ?`)
		args = append(args, authID)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	var out []AuthQuotaBaseline
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT provider, auth_id, window_id, cycle_key, observed, baseline, updated_at
			 FROM auth_quota_window_baselines`+clause+
				` ORDER BY provider, auth_id, window_id, cycle_key`, args...)
		if err != nil {
			return fmt.Errorf("列出认证额度窗口失败: %w", err)
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var b AuthQuotaBaseline
			var updated int64
			if err := rows.Scan(&b.Provider, &b.AuthID, &b.WindowID, &b.CycleKey,
				&b.Observed, &b.Baseline, &updated); err != nil {
				return fmt.Errorf("扫描认证额度窗口失败: %w", err)
			}
			b.UpdatedAt = time.UnixMilli(updated).UTC()
			out = append(out, b)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("遍历认证额度窗口失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
