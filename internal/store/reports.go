package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ReportConfig 是一条定期报告配置。Sections 与 EndpointIDs 以 JSON 落库，
// 结构随版本演进，store 层不解释其内容。
type ReportConfig struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`

	Frequency   string `json:"frequency"` // daily | weekly | monthly
	TimeOfDay   string `json:"time_of_day"`
	Weekday     int    `json:"weekday"`  // 1=周一 .. 7=周日（周报生效）
	Monthday    int    `json:"monthday"` // 1..28（月报生效）
	TZOffsetMin int    `json:"tz_offset_min"`

	Sections    json.RawMessage `json:"sections"`
	EndpointIDs []int64         `json:"endpoint_ids"`

	LastPeriod string `json:"last_period"`

	LastSentAt *time.Time `json:"last_sent_at,omitempty"`
	LastError  string     `json:"last_error"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const reportColumns = `id,name,enabled,frequency,time_of_day,weekday,monthday,tz_offset_min,sections_json,endpoint_ids_json,last_period,last_sent_at,last_error,created_at,updated_at`

func scanReport(sc interface{ Scan(...any) error }) (ReportConfig, error) {
	var r ReportConfig
	var enabled int
	var sections, endpoints string
	var lastSent *int64
	var created, updated int64
	if err := sc.Scan(&r.ID, &r.Name, &enabled, &r.Frequency, &r.TimeOfDay, &r.Weekday, &r.Monthday,
		&r.TZOffsetMin, &sections, &endpoints, &r.LastPeriod, &lastSent, &r.LastError, &created, &updated); err != nil {
		return ReportConfig{}, err
	}
	r.Enabled = enabled == 1
	r.Sections = json.RawMessage(sections)
	_ = json.Unmarshal([]byte(endpoints), &r.EndpointIDs)
	r.LastSentAt = timePtr(lastSent)
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.UpdatedAt = time.UnixMilli(updated).UTC()
	return r, nil
}

// ListReportConfigs 返回全部报告配置，按 id 升序。
func (s *Store) ListReportConfigs(ctx context.Context) ([]ReportConfig, error) {
	var out []ReportConfig
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+reportColumns+` FROM report_configs ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanReport(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("列出报告配置失败: %w", err)
	}
	return out, nil
}

// GetReportConfig 按 id 读取单条配置。
func (s *Store) GetReportConfig(ctx context.Context, id int64) (ReportConfig, error) {
	var r ReportConfig
	err := s.Read(ctx, func(q Querier) error {
		var err error
		r, err = scanReport(q.QueryRowContext(ctx,
			`SELECT `+reportColumns+` FROM report_configs WHERE id = ?`, id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ReportConfig{}, fmt.Errorf("%w: report_config %d", ErrNotFound, id)
	}
	if err != nil {
		return ReportConfig{}, fmt.Errorf("读取报告配置 %d 失败: %w", id, err)
	}
	return r, nil
}

// reportEditParams 是新增与更新共享的 9 个可编辑字段。
func reportEditParams(r ReportConfig) []any {
	en := 0
	if r.Enabled {
		en = 1
	}
	endpoints, _ := json.Marshal(r.EndpointIDs)
	sections := "{}"
	if len(r.Sections) > 0 {
		sections = string(r.Sections)
	}
	return []any{r.Name, en, r.Frequency, r.TimeOfDay, r.Weekday, r.Monthday, r.TZOffsetMin,
		sections, string(endpoints)}
}

// InsertReportConfig 新增配置，返回新 id。
func (s *Store) InsertReportConfig(ctx context.Context, r ReportConfig) (int64, error) {
	ts := nowMillis()
	var id int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO report_configs (name,enabled,frequency,time_of_day,weekday,monthday,tz_offset_min,sections_json,endpoint_ids_json,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			append(reportEditParams(r), ts, ts)...)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("新增报告配置失败: %w", err)
	}
	return id, nil
}

// UpdateReportConfig 覆盖可编辑字段；last_period/发送状态不受影响。
func (s *Store) UpdateReportConfig(ctx context.Context, r ReportConfig) error {
	ts := nowMillis()
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE report_configs SET name=?,enabled=?,frequency=?,time_of_day=?,weekday=?,monthday=?,tz_offset_min=?,sections_json=?,endpoint_ids_json=?,updated_at=? WHERE id=?`,
			append(reportEditParams(r), ts, r.ID)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: report_config %d", ErrNotFound, r.ID)
		}
		return nil
	})
}

// UpdateReportResult 记录一次发送：period 非空时刷新 last_period（失败也推进，
// 避免每分钟重试轰炸；面板展示 last_error，可用测试按钮手动重发）；
// errText 为空视为成功并清空 last_error。
func (s *Store) UpdateReportResult(ctx context.Context, id int64, period, errText string, sentAt time.Time) error {
	now := sentAt.UTC().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if period != "" {
			res, err = tx.ExecContext(ctx,
				`UPDATE report_configs SET last_period=?, last_sent_at=?, last_error=? WHERE id=?`,
				period, now, errText, id)
		} else {
			res, err = tx.ExecContext(ctx,
				`UPDATE report_configs SET last_sent_at=?, last_error=? WHERE id=?`, now, errText, id)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: report_config %d", ErrNotFound, id)
		}
		return nil
	})
}

// DeleteReportConfig 删除配置。
func (s *Store) DeleteReportConfig(ctx context.Context, id int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM report_configs WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: report_config %d", ErrNotFound, id)
		}
		return nil
	})
}
