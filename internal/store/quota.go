package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// Audit 非空时在预占成功的同一写事务尾追加审计事件（省一次独立写事务）。
	// 审计失败只吞错不回滚预占。
	Audit *AuditEvent
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
	var callerID string
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
		scopeWhere, scopeArg := `key_id = ?`, any(p.KeyID)
		if k.CallerScope == CallerScopeCaller {
			scopeWhere, scopeArg = `caller_id = ?`, k.CallerID
		}
		dailyStart, weeklyStart, monthlyStart := CycleStart(p.Now)

		// 并发计数、held 金额四档、held token 四档与周期内 held 请求数合并为
		// 同一条聚合语句：多次独立扫描压缩成一次，缩短 writeMu 临界区。
		loadAgg := func() (concurrent int64, held [4]int64, heldTok [4]int64, heldReq [2]int64, err error) {
			q := `SELECT COUNT(*),
					COALESCE(SUM(held_micro_usd),0),
					COALESCE(SUM(CASE WHEN created_at >= ? THEN held_micro_usd ELSE 0 END),0),
					COALESCE(SUM(CASE WHEN created_at >= ? THEN held_micro_usd ELSE 0 END),0),
					COALESCE(SUM(CASE WHEN created_at >= ? THEN held_micro_usd ELSE 0 END),0),
					COALESCE(SUM(reserved_tokens),0),
					COALESCE(SUM(CASE WHEN created_at >= ? THEN reserved_tokens ELSE 0 END),0),
					COALESCE(SUM(CASE WHEN created_at >= ? THEN reserved_tokens ELSE 0 END),0),
					COALESCE(SUM(CASE WHEN created_at >= ? THEN reserved_tokens ELSE 0 END),0),
					COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),0),
					COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),0)
				FROM reservations WHERE status='held' AND ` + scopeWhere
			err = tx.QueryRowContext(ctx, q,
				dailyStart.UnixMilli(), weeklyStart.UnixMilli(), monthlyStart.UnixMilli(),
				dailyStart.UnixMilli(), weeklyStart.UnixMilli(), monthlyStart.UnixMilli(),
				dailyStart.UnixMilli(), monthlyStart.UnixMilli(),
				scopeArg).Scan(&concurrent, &held[0], &held[1], &held[2], &held[3],
				&heldTok[0], &heldTok[1], &heldTok[2], &heldTok[3],
				&heldReq[0], &heldReq[1])
			return
		}
		recheck := func() error {
			concurrent, held, heldTok, heldReq, err := loadAgg()
			if err != nil {
				return err
			}
			cy := CyclesFor(p.Now)
			settledTotal := int64(k.SpentMicroUSD)
			settledDaily := int64(spentForCycle(k.DailyCycleKey, cy.Daily, k.DailySpentMicroUSD))
			settledWeekly := int64(spentForCycle(k.WeeklyCycleKey, cy.Weekly, k.WeeklySpentMicroUSD))
			settledMonthly := int64(spentForCycle(k.MonthlyCycleKey, cy.Monthly, k.MonthlySpentMicroUSD))
			usedTokTotal := k.TokensUsed
			usedTokDaily := tokensForCycle(k.DailyCycleKey, cy.Daily, k.DailyTokensUsed)
			usedTokWeekly := tokensForCycle(k.WeeklyCycleKey, cy.Weekly, k.WeeklyTokensUsed)
			usedTokMonthly := tokensForCycle(k.MonthlyCycleKey, cy.Monthly, k.MonthlyTokensUsed)
			usedReqDaily := requestsForCycle(k.DailyCycleKey, cy.Daily, k.DailyRequestsUsed)
			usedReqMonthly := requestsForCycle(k.MonthlyCycleKey, cy.Monthly, k.MonthlyRequestsUsed)
			if k.CallerScope == CallerScopeCaller {
				// caller 归属：已结算口径取该归属全部 Key 的累计，
				// 金额/token/请求次数合并为一条查询（原为两条）。
				var sT, sD, sW, sM, uT, uD, uW, uM, rD, rM int64
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(spent_micro_usd),0), COALESCE(SUM(CASE WHEN daily_cycle_key=? THEN daily_spent_micro_usd ELSE 0 END),0), COALESCE(SUM(CASE WHEN weekly_cycle_key=? THEN weekly_spent_micro_usd ELSE 0 END),0), COALESCE(SUM(CASE WHEN monthly_cycle_key=? THEN monthly_spent_micro_usd ELSE 0 END),0), COALESCE(SUM(tokens_used),0), COALESCE(SUM(CASE WHEN daily_cycle_key=? THEN daily_tokens_used ELSE 0 END),0), COALESCE(SUM(CASE WHEN weekly_cycle_key=? THEN weekly_tokens_used ELSE 0 END),0), COALESCE(SUM(CASE WHEN monthly_cycle_key=? THEN monthly_tokens_used ELSE 0 END),0), COALESCE(SUM(CASE WHEN daily_cycle_key=? THEN daily_requests_used ELSE 0 END),0), COALESCE(SUM(CASE WHEN monthly_cycle_key=? THEN monthly_requests_used ELSE 0 END),0) FROM plugin_keys WHERE caller_id=?`,
					cy.Daily, cy.Weekly, cy.Monthly, cy.Daily, cy.Weekly, cy.Monthly, cy.Daily, cy.Monthly, k.CallerID).
					Scan(&sT, &sD, &sW, &sM, &uT, &uD, &uW, &uM, &rD, &rM); err != nil {
					return err
				}
				settledTotal, settledDaily, settledWeekly, settledMonthly = sT, sD, sW, sM
				usedTokTotal, usedTokDaily, usedTokWeekly, usedTokMonthly = uT, uD, uW, uM
				usedReqDaily, usedReqMonthly = rD, rM
			}
			amount := int64(p.HeldMicroUSD)
			checks := []struct {
				name  string
				limit *money.Micro
				used  int64
			}{{"total", k.QuotaMicroUSD, settledTotal + held[0]}, {"daily", k.DailyMicroUSD, settledDaily + held[1]}, {"weekly", k.WeeklyMicroUSD, settledWeekly + held[2]}, {"monthly", k.MonthlyMicroUSD, settledMonthly + held[3]}}
			for _, c := range checks {
				if c.limit != nil && (amount > int64(*c.limit) || c.used > int64(*c.limit)-amount) {
					return fmt.Errorf("%w: %s", ErrQuotaExceeded, c.name)
				}
			}

			// ---- Token 限额：与金额同构的四档检查 ----
			// 只有配了 token 限额的 Key 才启用判定（绝大多数 Key 只配金额）；
			// token 汇总已随金额在同一次扫描里取得，不再单独付费。
			if k.TokenLimit != nil || k.DailyTokenLimit != nil || k.WeeklyTokenLimit != nil || k.MonthlyTokenLimit != nil {
				tokAmount := p.ReservedTokens
				tokChecks := []struct {
					name  string
					limit *int64
					used  int64
				}{{"total_tokens", k.TokenLimit, usedTokTotal + heldTok[0]}, {"daily_tokens", k.DailyTokenLimit, usedTokDaily + heldTok[1]}, {"weekly_tokens", k.WeeklyTokenLimit, usedTokWeekly + heldTok[2]}, {"monthly_tokens", k.MonthlyTokenLimit, usedTokMonthly + heldTok[3]}}
				for _, c := range tokChecks {
					if c.limit != nil && (tokAmount > *c.limit || c.used > *c.limit-tokAmount) {
						return fmt.Errorf("%w: %s", ErrQuotaExceeded, c.name)
					}
				}
			}
			// ---- 请求次数限额：日/月两档，每笔请求固定 +1；与金额/Token
			// 并列生效（任一触顶即拒绝），未配置时不付任何额外代价。
			if k.DailyRequestsLimit != nil || k.MonthlyRequestsLimit != nil {
				reqAmount := int64(1)
				reqChecks := []struct {
					name  string
					limit *int64
					used  int64
				}{{"daily_requests", k.DailyRequestsLimit, usedReqDaily + heldReq[0]}, {"monthly_requests", k.MonthlyRequestsLimit, usedReqMonthly + heldReq[1]}}
				for _, c := range reqChecks {
					if c.limit != nil && (reqAmount > *c.limit || c.used > *c.limit-reqAmount) {
						return fmt.Errorf("%w: %s", ErrQuotaExceeded, c.name)
					}
				}
			}
			if k.MaxConcurrentRequests > 0 && concurrent >= int64(k.MaxConcurrentRequests) {
				return ErrConcurrencyExceeded
			}
			return nil
		}
		checkErr := recheck()
		if checkErr != nil && (errors.Is(checkErr, ErrQuotaExceeded) || errors.Is(checkErr, ErrConcurrencyExceeded)) {
			// 僵尸预占会虚占并发与额度导致误拒。常规清扫由服务层节流驱动
			//（SweepStaleBefore）；这里只在「即将拒绝」时按行自身 expires_at
			// 补救一次并复核——不引入额外策略参数，正常通过路径零开销。
			if _, e := tx.ExecContext(ctx,
				`UPDATE reservations SET status='released',released_at=?,heartbeat_at=? WHERE status='held' AND expires_at < ?`,
				p.Now.UTC().UnixMilli(), p.Now.UTC().UnixMilli(), p.Now.UTC().UnixMilli()); e != nil {
				return e
			}
			checkErr = recheck()
		}
		if checkErr != nil {
			return checkErr
		}
		reservationCaller := p.CallerID
		if k.CallerScope == CallerScopeCaller {
			reservationCaller = k.CallerID
		}
		callerID = reservationCaller
		_, err = s.execHotTx(ctx, tx, `INSERT INTO reservations (id,key_id,caller_id,model,idempotency_key,status,held_micro_usd,settled_micro_usd,reserved_tokens,created_at,expires_at,heartbeat_at) VALUES (?,?,?,?,?,'held',?,?,?, ?,?,?)`, p.ID, p.KeyID, reservationCaller, p.Model, nullIfEmpty(p.IdempotencyKey), int64(p.HeldMicroUSD), 0, p.ReservedTokens, p.Now.UTC().UnixMilli(), p.ExpiresAt.UTC().UnixMilli(), p.Now.UTC().UnixMilli())
		if isUniqueViolation(err) && p.IdempotencyKey != "" {
			return fmt.Errorf("预占幂等键冲突: %w", ErrConflict)
		}
		if err != nil {
			return err
		}
		if p.Audit != nil {
			// 审计失败不回滚预占：与旧「事务外吞错 AppendAudit」语义一致，
			// 资金扣占不能因留痕失败而丢失。
			_ = appendAuditTx(ctx, s, tx, *p.Audit)
		}
		return nil
	})
	if err != nil {
		return Reservation{}, false, fmt.Errorf("写入预占失败: %w", err)
	}
	if existing {
		return out, true, nil
	}
	// 预占行是本事务按入参刚写的，直接组装返回值：省掉提交后再发一次
	// GetReservation 读往返（鉴权热路径上每次预占都要付）。
	out = Reservation{
		ID:             p.ID,
		KeyID:          p.KeyID,
		CallerID:       callerID,
		Model:          p.Model,
		IdempotencyKey: p.IdempotencyKey,
		Status:         "held",
		HeldMicroUSD:   p.HeldMicroUSD,
		ReservedTokens: p.ReservedTokens,
		CreatedAt:      p.Now.UTC(),
		ExpiresAt:      p.ExpiresAt.UTC(),
		HeartbeatAt:    p.Now.UTC(),
	}
	return out, false, nil
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
// audits 非空时在同一写事务内追加审计事件——结算与留痕原子化，省去独立的一次写事务。
//
// billableTokens 是本次真实消耗的计费 token 合计（输入+输出+缓存读+缓存写，
// 由 usageparse.Billable().Sum() 得出，与 cost 同一口径）。它累加进 token 累计器，
// 与金额累计器在同一条 UPDATE 里完成，保证两种口径的周期归零点严格一致。
func (s *Store) SettleReservation(ctx context.Context, id string, cost money.Micro, billableTokens int64, now time.Time, request *Request, audits ...AuditEvent) (Reservation, error) {
	if cost < 0 {
		return Reservation{}, errors.New("结算金额不能为负")
	}
	if billableTokens < 0 {
		return Reservation{}, errors.New("结算 token 数不能为负")
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
		if _, err = s.execHotTx(ctx, tx, `UPDATE reservations SET status='settled',settled_micro_usd=?,settled_at=?,heartbeat_at=? WHERE id=? AND status='held'`, int64(cost), now.UTC().UnixMilli(), now.UTC().UnixMilli(), id); err != nil {
			return err
		}
		cy := CyclesFor(now)
		// 金额、token 与请求次数累计器在同一条 UPDATE 内推进：周期标识只判
		// 一次，三种口径的归零点因此严格一致（不会出现金额已跨期而请求数未跨期）。
		if _, err = s.execHotTx(ctx, tx, `UPDATE plugin_keys SET spent_micro_usd=spent_micro_usd+?, tokens_used=tokens_used+?, daily_cycle_key=CASE WHEN daily_cycle_key<>? THEN ? ELSE daily_cycle_key END, daily_spent_micro_usd=CASE WHEN daily_cycle_key<>? THEN ? ELSE daily_spent_micro_usd+? END, daily_tokens_used=CASE WHEN daily_cycle_key<>? THEN ? ELSE daily_tokens_used+? END, daily_requests_used=CASE WHEN daily_cycle_key<>? THEN 1 ELSE daily_requests_used+1 END, weekly_cycle_key=CASE WHEN weekly_cycle_key<>? THEN ? ELSE weekly_cycle_key END, weekly_spent_micro_usd=CASE WHEN weekly_cycle_key<>? THEN ? ELSE weekly_spent_micro_usd+? END, weekly_tokens_used=CASE WHEN weekly_cycle_key<>? THEN ? ELSE weekly_tokens_used+? END, monthly_cycle_key=CASE WHEN monthly_cycle_key<>? THEN ? ELSE monthly_cycle_key END, monthly_spent_micro_usd=CASE WHEN monthly_cycle_key<>? THEN ? ELSE monthly_spent_micro_usd+? END, monthly_tokens_used=CASE WHEN monthly_cycle_key<>? THEN ? ELSE monthly_tokens_used+? END, monthly_requests_used=CASE WHEN monthly_cycle_key<>? THEN 1 ELSE monthly_requests_used+1 END, updated_at=? WHERE kid=?`,
			int64(cost), billableTokens,
			cy.Daily, cy.Daily,
			cy.Daily, int64(cost), int64(cost),
			cy.Daily, billableTokens, billableTokens,
			cy.Daily,
			cy.Weekly, cy.Weekly,
			cy.Weekly, int64(cost), int64(cost),
			cy.Weekly, billableTokens, billableTokens,
			cy.Monthly, cy.Monthly,
			cy.Monthly, int64(cost), int64(cost),
			cy.Monthly, billableTokens, billableTokens,
			cy.Monthly,
			now.UTC().UnixMilli(), r.KeyID); err != nil {
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
			// 入库时防重：被动行可能抢先落库（认领未覆盖的迟到回调/跨进程兜底）。
			// 在同一事务内探测，命中则插入本行后立即合并掉被动行——
			// 单写者串行化保证探测结果在提交前不会被并发改写，
			// 外部看不到任何中间态，事后对账因此不再需要。
			// 结算侧传执行器侧 token 计数做相容性过滤（0 = 不约束）。
			twin, twinFound, perr := duplicateProbeTx(ctx, tx,
				modelCandidatesOf(request.Model), request.TS, request.LatencyMS,
				request.TotalTokens, request.InputTokens, false)
			if perr != nil {
				return perr
			}
			ierr := insertRequestTx(ctx, s, tx, *request)
			if ierr != nil && !errors.Is(ierr, errDuplicateRequest) {
				return ierr
			}
			if ierr == nil {
				if err := upsertRollupTx(ctx, s, tx, *request); err != nil {
					return err
				}
				if twinFound {
					keeper, lerr := loadRequestTx(ctx, tx, request.ID)
					if lerr != nil {
						return lerr
					}
					if err := mergeRequestPairTx(ctx, tx, s, keeper, twin); err != nil {
						return err
					}
				}
			}
		}
		out = r
		out.Status = ReservationSettled
		out.SettledMicroUSD = cost
		t := now.UTC()
		out.SettledAt = &t
		for _, e := range audits {
			if err := appendAuditTx(ctx, s, tx, e); err != nil {
				return err
			}
		}
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

// TouchReservations 批量续期在途预占的心跳。单个写事务覆盖全部活跃预占，
// 供集中式心跳协程每轮调用，避免每个流式请求各占一个定时器与一次写事务。
func (s *Store) TouchReservations(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+1)
	args = append(args, time.Now().UTC().UnixMilli())
	for _, id := range ids {
		args = append(args, id)
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE reservations SET heartbeat_at=? WHERE status='held' AND id IN (`+placeholders+`)`,
			args...)
		return err
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
	return s.Write(ctx, func(tx *sql.Tx) error { return appendAuditTx(ctx, s, tx, e) })
}
func appendAuditTx(ctx context.Context, s *Store, tx *sql.Tx, e AuditEvent) error {
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
	_, err = s.execHotTx(ctx, tx, `INSERT INTO audit_events(ts,actor,action,entity_type,entity_id,detail_json) VALUES(?,?,?,?,?,?)`, e.TS.UTC().UnixMilli(), e.Actor, e.Action, e.EntityType, e.EntityID, string(b))
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

// HeldReservation 是在途预占的展示行（GET /reservations/held）。
type HeldReservation struct {
	ID             string      `json:"id"`
	KeyID          string      `json:"key_id"`
	Model          string      `json:"model"`
	HeldMicroUSD   money.Micro `json:"held_micro_usd"`
	ReservedTokens int64       `json:"reserved_tokens"`
	CreatedAt      time.Time   `json:"created_at"`
	HeartbeatAt    time.Time   `json:"heartbeat_at"`
	AgeSec         int64       `json:"age_sec"`
	// StaleMark 标记心跳已超时但行仍在（stale 清扫的候选）。
	StaleMark bool `json:"stale"`
}

// RecentReservation 是最近已完结预占的回顾行（GET /reservations/recent）：
// 预占估算 vs 实际结算的对照，用于实时页「最近预占」面板。
type RecentReservation struct {
	ID               string      `json:"id"`
	KeyID            string      `json:"key_id"`
	Model            string      `json:"model"`
	Status           string      `json:"status"` // settled | released
	HeldMicroUSD     money.Micro `json:"held_micro_usd"`
	SettledMicroUSD  money.Micro `json:"settled_micro_usd"`
	ReservedTokens   int64       `json:"reserved_tokens"`
	CreatedAt        time.Time   `json:"created_at"`
	FinishedAt       time.Time   `json:"finished_at"`
	AgeMS            int64       `json:"age_ms"` // 预占创建到完结的全程耗时
}

// ListRecentReservations 返回最近 limit 条已完结（settled/released）预占，
// 按完结时刻倒序。released 涵盖三种收尾：执行器放弃/上游无响应释放、
// 心跳或过期清扫——都表示没有走到结算，settled_micro_usd 即为 0。
// 走 idx_reservations_settled 无法覆盖 released，故全表扫描 status 前缀
// 走 idx_reservations_key_status 的 status 列；已完结行随保留期被
// ApplyRetention 回收，量级有限。
func (s *Store) ListRecentReservations(ctx context.Context, limit int) ([]RecentReservation, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	out := make([]RecentReservation, 0)
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT id, key_id, model, status, held_micro_usd, settled_micro_usd, reserved_tokens,
			        created_at, COALESCE(settled_at, released_at)
			 FROM reservations
			 WHERE status IN ('settled','released')
			 ORDER BY COALESCE(settled_at, released_at) DESC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RecentReservation
			var held, settled, created, finished int64 // 时间列存 UnixMilli 整数，与 scanReservation 同口径
			if err := rows.Scan(&r.ID, &r.KeyID, &r.Model, &r.Status, &held, &settled,
				&r.ReservedTokens, &created, &finished); err != nil {
				return err
			}
			r.HeldMicroUSD = money.Micro(held)
			r.SettledMicroUSD = money.Micro(settled)
			r.CreatedAt = time.UnixMilli(created).UTC()
			r.FinishedAt = time.UnixMilli(finished).UTC()
			r.AgeMS = finished - created
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ListHeldReservations 返回全部在途（status='held'）预占，按创建时间倒序，
// 供系统页「进行中请求」面板。走 idx_reservations_key_status 的 status 前缀。
func (s *Store) ListHeldReservations(ctx context.Context, staleBefore time.Time, now time.Time, limit int) ([]HeldReservation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := make([]HeldReservation, 0) // 非 nil：JSON 序列化为 [] 而不是 null
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT id, key_id, model, held_micro_usd, reserved_tokens, created_at, heartbeat_at
			 FROM reservations WHERE status = 'held'
			 ORDER BY created_at DESC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h HeldReservation
			var held, created, heartbeat int64 // 时间列存 UnixMilli 整数，与 scanReservation 同口径
			if err := rows.Scan(&h.ID, &h.KeyID, &h.Model, &held, &h.ReservedTokens, &created, &heartbeat); err != nil {
				return err
			}
			h.HeldMicroUSD = money.Micro(held)
			h.CreatedAt = time.UnixMilli(created).UTC()
			h.HeartbeatAt = time.UnixMilli(heartbeat).UTC()
			h.AgeSec = int64(now.Sub(h.CreatedAt).Seconds())
			h.StaleMark = !h.HeartbeatAt.After(staleBefore)
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}
