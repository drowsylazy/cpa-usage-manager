package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

// HoldReservation 原子检查并写入一条在途预占。额度检查由 service 计算上限后传入。
// SweepStaleBefore 非零时，事务内先释放心跳/过期早于该时刻的陈旧预占，
// 使并发与额度统计基于干净数据；零值跳过清扫。
type HoldReservationParams struct {
	ID, KeyID, CallerID, Model, IdempotencyKey string
	HeldMicroUSD                               money.Micro
	ReservedTokens                             int64
	ExpiresAt, Now                             time.Time
	SweepStaleBefore                           time.Time
}

// HoldReservation 写入 held 预占；idempotency_key 已存在时返回原记录且不重复扣占。
func (s *Store) HoldReservation(ctx context.Context, p HoldReservationParams) (Reservation, bool, error) {
	if p.ID == "" || p.KeyID == "" {
		return Reservation{}, false, errors.New("预占缺少 id 或 key_id")
	}
	if p.HeldMicroUSD < 0 || p.ReservedTokens < 0 {
		return Reservation{}, false, errors.New("预占金额和 token 不能为负")
	}
	if p.Now.IsZero() {
		p.Now = time.Now().UTC()
	}
	if p.ExpiresAt.IsZero() {
		p.ExpiresAt = p.Now.Add(2 * time.Hour)
	}
	var out Reservation
	existing := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if !p.SweepStaleBefore.IsZero() {
			// 先清僵尸预占再做额度统计：崩溃/重启残留的 held 行若不清，
			// 会永久占用该 Key 的并发名额与周期额度。
			if _, err := tx.ExecContext(ctx,
				`UPDATE reservations SET status='released',released_at=? WHERE status='held' AND (heartbeat_at < ? OR expires_at < ?)`,
				p.Now.UTC().UnixMilli(), p.SweepStaleBefore.UTC().UnixMilli(), p.SweepStaleBefore.UTC().UnixMilli()); err != nil {
				return fmt.Errorf("清扫陈旧预占失败: %w", err)
			}
		}
		if p.IdempotencyKey != "" {
			row := tx.QueryRowContext(ctx, `SELECT id,key_id,caller_id,model,idempotency_key,status,held_micro_usd,settled_micro_usd,reserved_tokens,created_at,expires_at,heartbeat_at,settled_at,released_at FROM reservations WHERE idempotency_key = ?`, p.IdempotencyKey)
			r, err := scanReservation(row)
			if err == nil {
				if r.KeyID != p.KeyID {
					return fmt.Errorf("%w: 幂等键已属于其他 Key", ErrConflict)
				}
				out = r
				existing = true
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var k PluginKey
		row := tx.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM plugin_keys WHERE kid = ?`, p.KeyID)
		var err error
		k, err = scanKey(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: Key %q", ErrNotFound, p.KeyID)
		}
		if err != nil {
			return err
		}
		if !k.Usable(p.Now) {
			return fmt.Errorf("%w: Key 不可用", ErrQuotaExceeded)
		}
		concurrentWhere, concurrentArg := `key_id = ?`, any(p.KeyID)
		if k.CallerScope == CallerScopeCaller {
			concurrentWhere, concurrentArg = `caller_id = ?`, k.CallerID
		}
		var concurrent int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reservations WHERE status = 'held' AND `+concurrentWhere, concurrentArg).Scan(&concurrent); err != nil {
			return err
		}
		if k.MaxConcurrentRequests > 0 && concurrent >= int64(k.MaxConcurrentRequests) {
			return ErrConcurrencyExceeded
		}
		dailyStart, weeklyStart, monthlyStart := CycleStart(p.Now)
		where, scopeArg := `key_id = ?`, any(p.KeyID)
		if k.CallerScope == CallerScopeCaller {
			where, scopeArg = `caller_id = ?`, k.CallerID
		}
		var heldTotal, heldDaily, heldWeekly, heldMonthly int64
		query := `SELECT COALESCE(SUM(held_micro_usd),0), COALESCE(SUM(CASE WHEN created_at >= ? THEN held_micro_usd ELSE 0 END),0), COALESCE(SUM(CASE WHEN created_at >= ? THEN held_micro_usd ELSE 0 END),0), COALESCE(SUM(CASE WHEN created_at >= ? THEN held_micro_usd ELSE 0 END),0) FROM reservations WHERE status='held' AND ` + where
		if err := tx.QueryRowContext(ctx, query, dailyStart.UnixMilli(), weeklyStart.UnixMilli(), monthlyStart.UnixMilli(), scopeArg).Scan(&heldTotal, &heldDaily, &heldWeekly, &heldMonthly); err != nil {
			return err
		}
		settledTotal, settledDaily, settledWeekly, settledMonthly := int64(k.SpentMicroUSD), int64(spentForCycle(k.DailyCycleKey, CyclesFor(p.Now).Daily, k.DailySpentMicroUSD)), int64(spentForCycle(k.WeeklyCycleKey, CyclesFor(p.Now).Weekly, k.WeeklySpentMicroUSD)), int64(spentForCycle(k.MonthlyCycleKey, CyclesFor(p.Now).Monthly, k.MonthlySpentMicroUSD))
		if k.CallerScope == CallerScopeCaller {
			cy := CyclesFor(p.Now)
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(spent_micro_usd),0), COALESCE(SUM(CASE WHEN daily_cycle_key=? THEN daily_spent_micro_usd ELSE 0 END),0), COALESCE(SUM(CASE WHEN weekly_cycle_key=? THEN weekly_spent_micro_usd ELSE 0 END),0), COALESCE(SUM(CASE WHEN monthly_cycle_key=? THEN monthly_spent_micro_usd ELSE 0 END),0) FROM plugin_keys WHERE caller_id=?`, cy.Daily, cy.Weekly, cy.Monthly, k.CallerID).Scan(&settledTotal, &settledDaily, &settledWeekly, &settledMonthly); err != nil {
				return err
			}
		}
		amount := int64(p.HeldMicroUSD)
		checks := []struct {
			name  string
			limit *money.Micro
			used  int64
		}{{"total", k.QuotaMicroUSD, settledTotal + heldTotal}, {"daily", k.DailyMicroUSD, settledDaily + heldDaily}, {"weekly", k.WeeklyMicroUSD, settledWeekly + heldWeekly}, {"monthly", k.MonthlyMicroUSD, settledMonthly + heldMonthly}}
		for _, c := range checks {
			if c.limit != nil && (amount > int64(*c.limit) || c.used > int64(*c.limit)-amount) {
				return fmt.Errorf("%w: %s", ErrQuotaExceeded, c.name)
			}
		}
		reservationCaller := p.CallerID
		if k.CallerScope == CallerScopeCaller {
			reservationCaller = k.CallerID
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO reservations (id,key_id,caller_id,model,idempotency_key,status,held_micro_usd,settled_micro_usd,reserved_tokens,created_at,expires_at,heartbeat_at) VALUES (?,?,?,?,?,'held',?,?,?, ?,?,?)`, p.ID, p.KeyID, reservationCaller, p.Model, nullIfEmpty(p.IdempotencyKey), int64(p.HeldMicroUSD), 0, p.ReservedTokens, p.Now.UTC().UnixMilli(), p.ExpiresAt.UTC().UnixMilli(), p.Now.UTC().UnixMilli())
		if isUniqueViolation(err) && p.IdempotencyKey != "" {
			return fmt.Errorf("预占幂等键冲突: %w", ErrConflict)
		}
		return err
	})
	if err != nil {
		return Reservation{}, false, fmt.Errorf("写入预占失败: %w", err)
	}
	if existing {
		return out, true, nil
	}
	r, err := s.GetReservation(ctx, p.ID)
	return r, false, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanReservation(sc interface{ Scan(...any) error }) (Reservation, error) {
	var r Reservation
	var created, expires, heartbeat int64
	var settled, released *int64
	var idem sql.NullString
	if err := sc.Scan(&r.ID, &r.KeyID, &r.CallerID, &r.Model, &idem, &r.Status, &r.HeldMicroUSD, &r.SettledMicroUSD, &r.ReservedTokens, &created, &expires, &heartbeat, &settled, &released); err != nil {
		return Reservation{}, err
	}
	r.IdempotencyKey = idem.String
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.ExpiresAt = time.UnixMilli(expires).UTC()
	r.HeartbeatAt = time.UnixMilli(heartbeat).UTC()
	r.SettledAt = timePtr(settled)
	r.ReleasedAt = timePtr(released)
	return r, nil
}

func (s *Store) GetReservation(ctx context.Context, id string) (Reservation, error) {
	var r Reservation
	err := s.Read(ctx, func(q Querier) error {
		var e error
		r, e = scanReservation(q.QueryRowContext(ctx, `SELECT id,key_id,caller_id,model,idempotency_key,status,held_micro_usd,settled_micro_usd,reserved_tokens,created_at,expires_at,heartbeat_at,settled_at,released_at FROM reservations WHERE id = ?`, id))
		return e
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, fmt.Errorf("%w: reservation %q", ErrNotFound, id)
	}
	return r, err
}

// SettleReservation 原子结束预占并更新 Key 累计器；request 非 nil 时同事务写入请求与分钟聚合。
func (s *Store) SettleReservation(ctx context.Context, id string, cost money.Micro, now time.Time, request *Request) (Reservation, error) {
	if cost < 0 {
		return Reservation{}, errors.New("结算金额不能为负")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var out Reservation
	err := s.Write(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT id,key_id,caller_id,model,idempotency_key,status,held_micro_usd,settled_micro_usd,reserved_tokens,created_at,expires_at,heartbeat_at,settled_at,released_at FROM reservations WHERE id = ?`, id)
		r, err := scanReservation(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: reservation %q", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if r.Status != ReservationHeld {
			out = r
			return nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE reservations SET status='settled',settled_micro_usd=?,settled_at=?,heartbeat_at=? WHERE id=? AND status='held'`, int64(cost), now.UTC().UnixMilli(), now.UTC().UnixMilli(), id); err != nil {
			return err
		}
		cy := CyclesFor(now)
		if _, err = tx.ExecContext(ctx, `UPDATE plugin_keys SET spent_micro_usd=spent_micro_usd+?, daily_cycle_key=CASE WHEN daily_cycle_key<>? THEN ? ELSE daily_cycle_key END, daily_spent_micro_usd=CASE WHEN daily_cycle_key<>? THEN ? ELSE daily_spent_micro_usd+? END, weekly_cycle_key=CASE WHEN weekly_cycle_key<>? THEN ? ELSE weekly_cycle_key END, weekly_spent_micro_usd=CASE WHEN weekly_cycle_key<>? THEN ? ELSE weekly_spent_micro_usd+? END, monthly_cycle_key=CASE WHEN monthly_cycle_key<>? THEN ? ELSE monthly_cycle_key END, monthly_spent_micro_usd=CASE WHEN monthly_cycle_key<>? THEN ? ELSE monthly_spent_micro_usd+? END, updated_at=? WHERE kid=?`, int64(cost), cy.Daily, cy.Daily, cy.Daily, int64(cost), int64(cost), cy.Weekly, cy.Weekly, cy.Weekly, int64(cost), int64(cost), cy.Monthly, cy.Monthly, cy.Monthly, int64(cost), int64(cost), now.UTC().UnixMilli(), r.KeyID); err != nil {
			return err
		}
		if request != nil {
			request.ReservationID = id
			if request.CostMicroUSD == 0 {
				request.CostMicroUSD = cost
			}
			if request.TS.IsZero() {
				request.TS = now
			}
			ierr := insertRequestTx(ctx, tx, *request)
			if ierr != nil && !errors.Is(ierr, errDuplicateRequest) {
				return ierr
			}
			if ierr == nil {
				if err := upsertRollupTx(ctx, tx, *request); err != nil {
					return err
				}
			}
		}
		out = r
		out.Status = ReservationSettled
		out.SettledMicroUSD = cost
		t := now.UTC()
		out.SettledAt = &t
		return nil
	})
	return out, err
}

func (s *Store) ReleaseReservation(ctx context.Context, id string, now time.Time) (Reservation, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var out Reservation
	err := s.Write(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT id,key_id,caller_id,model,idempotency_key,status,held_micro_usd,settled_micro_usd,reserved_tokens,created_at,expires_at,heartbeat_at,settled_at,released_at FROM reservations WHERE id=?`, id)
		r, err := scanReservation(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: reservation %q", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if r.Status == ReservationHeld {
			if _, err = tx.ExecContext(ctx, `UPDATE reservations SET status='released',released_at=?,heartbeat_at=? WHERE id=? AND status='held'`, now.UTC().UnixMilli(), now.UTC().UnixMilli(), id); err != nil {
				return err
			}
			r.Status = ReservationReleased
			t := now.UTC()
			r.ReleasedAt = &t
		}
		out = r
		return nil
	})
	return out, err
}

func (s *Store) HeartbeatReservation(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE reservations SET heartbeat_at=? WHERE id=? AND status='held'`, at.UTC().UnixMilli(), id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("%w: held reservation %q", ErrNotFound, id)
		}
		return nil
	})
}

// ReleaseStaleReservations 释放心跳或过期时间已到的在途预占。
func (s *Store) ReleaseStaleReservations(ctx context.Context, before time.Time) (int64, error) {
	return s.releaseStale(ctx, before)
}
func (s *Store) releaseStale(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `UPDATE reservations SET status='released',released_at=? WHERE status='held' AND (heartbeat_at < ? OR expires_at < ?)`, time.Now().UTC().UnixMilli(), before.UTC().UnixMilli(), before.UTC().UnixMilli())
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		return nil
	})
	return n, err
}

// AppendAudit 在同一写事务中追加审计事件。
func (s *Store) AppendAudit(ctx context.Context, e AuditEvent) error {
	return s.Write(ctx, func(tx *sql.Tx) error { return appendAuditTx(ctx, tx, e) })
}
func appendAuditTx(ctx context.Context, tx *sql.Tx, e AuditEvent) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	if e.Action == "" {
		return errors.New("审计 action 不能为空")
	}
	b, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		b = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(ts,actor,action,entity_type,entity_id,detail_json) VALUES(?,?,?,?,?,?)`, e.TS.UTC().UnixMilli(), e.Actor, e.Action, e.EntityType, e.EntityID, string(b))
	return err
}

// ListAudit 返回最近审计事件。
func (s *Store) ListAudit(ctx context.Context, limit, offset int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []AuditEvent
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT id,ts,actor,action,entity_type,entity_id,detail_json FROM audit_events ORDER BY ts DESC,id DESC LIMIT ? OFFSET ?`, limit, maxInt(0, offset))
		if err != nil {
			return err
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var e AuditEvent
			var ts int64
			var raw string
			if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.EntityType, &e.EntityID, &raw); err != nil {
				return err
			}
			e.TS = time.UnixMilli(ts).UTC()
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &e.Detail)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
