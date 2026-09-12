package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// TestRunAutoRetention 钉住每日自动维护循环的服务层契约：删过保留期的行、
// 释放陈旧 held 预占、审计留痕；同时不做历史重复行对账（那是手动维护入口
// 的低频兜底，全年窗口自连接逐日跑代价不成比例）。
func TestRunAutoRetention(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := st.InsertKey(ctx, store.InsertKeyParams{
		KID: "kidret1", KeyHash: []byte("h"), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: store.CallerScopeCaller, CallerID: store.DefaultCallerID,
	}); err != nil {
		t.Fatal(err)
	}
	oldTS := now.AddDate(0, 0, -400).UnixMilli()
	newTS := now.Add(-time.Hour).UnixMilli()
	err := st.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO requests (id, ts, key_id, source, model) VALUES ('old', ?, 'k-old', 'executor', 'm'),
			 ('new', ?, 'k-new', 'executor', 'm')`, oldTS, newTS); err != nil {
			return err
		}
		// 历史重复对（执行器行 + 被动行，判据与 dedupePairSQL 同口径）：
		// RunAutoRetention 不做对账，两行都必须原样保留。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO requests (id, ts, key_id, source, model) VALUES ('dup-exec', ?, 'k-dup', 'executor', 'm'),
			 ('dup-passive', ?, '', 'passive', 'm')`, newTS, newTS); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	if _, _, err := st.HoldReservation(ctx, store.HoldReservationParams{
		ID: "stale-1", KeyID: "kidret1", Model: "m",
		HeldMicroUSD: 100, ReservedTokens: 10,
		Now: now.Add(-3 * time.Hour), ExpiresAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("准备陈旧预占失败: %v", err)
	}

	res, err := s.RunAutoRetention(ctx)
	if err != nil {
		t.Fatalf("RunAutoRetention 失败: %v", err)
	}
	if res.Requests != 1 {
		t.Fatalf("清理计数异常: %+v", res)
	}

	var oldN, dupN int
	var staleStatus string
	if err := st.Read(ctx, func(q store.Querier) error {
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE id = 'old'`).Scan(&oldN); err != nil {
			return err
		}
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE id IN ('dup-exec','dup-passive')`).Scan(&dupN); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, `SELECT status FROM reservations WHERE id = 'stale-1'`).Scan(&staleStatus)
	}); err != nil {
		t.Fatal(err)
	}
	if oldN != 0 {
		t.Fatalf("过保留期的行应被删除，仍剩 %d 行", oldN)
	}
	if dupN != 2 {
		t.Fatalf("自动清理不应做重复行对账，重复对应保留 2 行，剩 %d", dupN)
	}
	if staleStatus != "released" {
		t.Fatalf("陈旧预占应被释放，得到 %q", staleStatus)
	}

	events, err := st.ListAudit(ctx, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == "system.auto_retention" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("审计应留痕 system.auto_retention，实际: %+v", events)
	}
}
