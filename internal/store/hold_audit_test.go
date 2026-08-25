package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

// holdAuditTestStore 建库并预置一个无限额 Key，供 HoldReservation 直测。
func holdAuditTestStore(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "hold-audit")
	_, err := s.InsertKey(context.Background(), InsertKeyParams{
		KID: "k0001", KeyHash: []byte{1}, EncryptedMaterial: []byte{2}, PepperID: "p1",
		CallerScope: CallerScopeCaller, CallerID: DefaultCallerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestHoldReservationAuditInTransaction 验证审计并入预占写事务：
// 成功后 audit_events 有 quota.reserve 行；额度拒绝时没有；幂等重放不重复记。
func TestHoldReservationAuditInTransaction(t *testing.T) {
	s := holdAuditTestStore(t)
	ctx := context.Background()

	countReserveAudits := func() int64 {
		var n int64
		if err := s.Read(ctx, func(q Querier) error {
			return q.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM audit_events WHERE action='quota.reserve'`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	params := func() HoldReservationParams {
		return HoldReservationParams{
			ID: "r-1", KeyID: "k0001", CallerID: DefaultCallerID, Model: "m",
			HeldMicroUSD: money.Micro(100), ReservedTokens: 10,
			Audit: &AuditEvent{Action: "quota.reserve", EntityType: "reservation", EntityID: "r-1", Actor: "quota"},
		}
	}
	if _, _, err := s.HoldReservation(ctx, params()); err != nil {
		t.Fatal(err)
	}
	if n := countReserveAudits(); n != 1 {
		t.Fatalf("成功预占应留 1 条审计，实际 %d", n)
	}

	// 额度拒绝（金额为负在入口被拦，改用不存在 Key 触发失败路径）。
	bad := params()
	bad.ID, bad.KeyID = "r-2", "missing-key"
	bad.Audit.EntityID = "r-2"
	if _, _, err := s.HoldReservation(ctx, bad); err == nil {
		t.Fatal("不存在的 Key 应失败")
	}
	if n := countReserveAudits(); n != 1 {
		t.Fatalf("失败路径不应留审计，实际 %d", n)
	}

	// 幂等重放：返回原记录且不再追加审计。
	p := params()
	p.ID = "r-3"
	p.IdempotencyKey = "idem-1"
	if _, _, err := s.HoldReservation(ctx, p); err != nil {
		t.Fatal(err)
	}
	replay := params()
	replay.ID = "r-4"
	replay.IdempotencyKey = "idem-1"
	if res, existing, err := s.HoldReservation(ctx, replay); err != nil || !existing || res.ID != "r-3" {
		t.Fatalf("重放应命中原记录: existing=%v res=%+v err=%v", existing, res, err)
	}
	if n := countReserveAudits(); n != 2 {
		t.Fatalf("新插入才记审计（重放不计），实际 %d", n)
	}
}
