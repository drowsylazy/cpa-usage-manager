package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NotifyEndpoint 是一条告警通知端点。URL 字段是解密后的 shoutrrr URL，
// 加解密只发生在 service 层；store 只见密文（URLEnc）。
type NotifyEndpoint struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
	URL   string `json:"url"`

	Enabled bool `json:"enabled"`

	LastSentAt *time.Time `json:"last_sent_at,omitempty"`
	LastOKAt   *time.Time `json:"last_ok_at,omitempty"`
	LastError  string     `json:"last_error"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// URLEnc 是加密后的 shoutrrr URL，不进任何 JSON 响应。
	URLEnc []byte `json:"-"`
}

const notifyEndpointColumns = `id,label,url_enc,enabled,last_sent_at,last_ok_at,last_error,created_at,updated_at`

func scanNotifyEndpoint(sc interface{ Scan(...any) error }) (NotifyEndpoint, error) {
	var e NotifyEndpoint
	var enabled int
	var lastSent, lastOK *int64
	var created, updated int64
	if err := sc.Scan(&e.ID, &e.Label, &e.URLEnc, &enabled, &lastSent, &lastOK, &e.LastError, &created, &updated); err != nil {
		return NotifyEndpoint{}, err
	}
	e.Enabled = enabled == 1
	e.LastSentAt = timePtr(lastSent)
	e.LastOKAt = timePtr(lastOK)
	e.CreatedAt = time.UnixMilli(created).UTC()
	e.UpdatedAt = time.UnixMilli(updated).UTC()
	return e, nil
}

// ListNotifyEndpoints 返回全部通知端点，按 id 升序。
func (s *Store) ListNotifyEndpoints(ctx context.Context) ([]NotifyEndpoint, error) {
	var out []NotifyEndpoint
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+notifyEndpointColumns+` FROM notify_endpoints ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanNotifyEndpoint(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("列出通知端点失败: %w", err)
	}
	return out, nil
}

// GetNotifyEndpoint 按 id 读取单条端点。
func (s *Store) GetNotifyEndpoint(ctx context.Context, id int64) (NotifyEndpoint, error) {
	var e NotifyEndpoint
	err := s.Read(ctx, func(q Querier) error {
		var err error
		e, err = scanNotifyEndpoint(q.QueryRowContext(ctx,
			`SELECT `+notifyEndpointColumns+` FROM notify_endpoints WHERE id = ?`, id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return NotifyEndpoint{}, fmt.Errorf("%w: notify_endpoint %d", ErrNotFound, id)
	}
	if err != nil {
		return NotifyEndpoint{}, fmt.Errorf("读取通知端点 %d 失败: %w", id, err)
	}
	return e, nil
}

// InsertNotifyEndpoint 新增端点，返回新 id。now 仅用于测试注入时间。
func (s *Store) InsertNotifyEndpoint(ctx context.Context, label string, urlEnc []byte, enabled bool, now time.Time) (int64, error) {
	en := 0
	if enabled {
		en = 1
	}
	ts := nowMillis()
	if !now.IsZero() {
		ts = now.UTC().UnixMilli()
	}
	var id int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO notify_endpoints (label,url_enc,enabled,created_at,updated_at) VALUES (?,?,?,?,?)`,
			label, urlEnc, en, ts, ts)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("新增通知端点失败: %w", err)
	}
	return id, nil
}

// UpdateNotifyEndpoint 覆盖端点的可编辑字段（label/url/enabled）。
func (s *Store) UpdateNotifyEndpoint(ctx context.Context, id int64, label string, urlEnc []byte, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE notify_endpoints SET label=?, url_enc=?, enabled=?, updated_at=? WHERE id=?`,
			label, urlEnc, en, nowMillis(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: notify_endpoint %d", ErrNotFound, id)
		}
		return nil
	})
}

// DeleteNotifyEndpoint 删除端点。
func (s *Store) DeleteNotifyEndpoint(ctx context.Context, id int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM notify_endpoints WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: notify_endpoint %d", ErrNotFound, id)
		}
		return nil
	})
}

// UpdateNotifyEndpointResult 记录一次发送结果：last_sent_at 总是更新；
// errText 为空视为成功并同步刷新 last_ok_at 与清空 last_error。
func (s *Store) UpdateNotifyEndpointResult(ctx context.Context, id int64, sentAt time.Time, errText string) error {
	now := sentAt.UTC().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if errText == "" {
			res, err = tx.ExecContext(ctx,
				`UPDATE notify_endpoints SET last_sent_at=?, last_ok_at=?, last_error='' WHERE id=?`,
				now, now, id)
		} else {
			res, err = tx.ExecContext(ctx,
				`UPDATE notify_endpoints SET last_sent_at=?, last_error=? WHERE id=?`,
				now, errText, id)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: notify_endpoint %d", ErrNotFound, id)
		}
		return nil
	})
}
