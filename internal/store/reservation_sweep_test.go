package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// 陈旧预占内联清扫：崩溃/重启残留的 held 行必须能在下一次 Reserve 时被释放，
// 且活跃预占（心跳新鲜）绝不被误杀。SweepStaleBefore 零值保持旧行为。

func TestHoldReservationSweepsStaleBeforeCounting(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "sweep")
	ctx := context.Background()
	now := time.Now().UTC()

	kid := seedKeyForQuota(t, s)

	// 一条僵尸预占（心跳远早于阈值）+ 一条活跃预占（心跳新鲜）。
	stale := now.Add(-3 * time.Hour)
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "stale-1", KeyID: kid, Model: "m", HeldMicroUSD: 1_000_000, ReservedTokens: 10,
		ExpiresAt: stale.Add(time.Hour), Now: stale,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "live-1", KeyID: kid, Model: "m", HeldMicroUSD: 1_000_000, ReservedTokens: 10,
		ExpiresAt: now.Add(2 * time.Hour), Now: now,
	}); err != nil {
		t.Fatal(err)
	}

	sweepBefore := now.Add(-2 * time.Hour)
	res, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "new-1", KeyID: kid, Model: "m", HeldMicroUSD: 1_000_000, ReservedTokens: 10,
		ExpiresAt: now.Add(2 * time.Hour), Now: now, SweepStaleBefore: sweepBefore,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "new-1" {
		t.Fatalf("应写入新预占，得到 %q", res.ID)
	}

	var held int
	var released int
	if err := s.Read(ctx, func(q Querier) error {
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM reservations WHERE status='held'`).Scan(&held); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM reservations WHERE id='stale-1' AND status='released'`).Scan(&released)
	}); err != nil {
		t.Fatal(err)
	}
	if held != 2 { // live-1 + new-1
		t.Fatalf("held 行数 = %d, 期望 2（活跃行保留，仅僵尸被释放）", held)
	}
	if released != 1 {
		t.Fatalf("僵尸预占应被标记 released，released=%d", released)
	}
}

func TestHoldReservationZeroSweepKeepsLegacyBehavior(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "nosweep")
	ctx := context.Background()
	now := time.Now().UTC()
	kid := seedKeyForQuota(t, s)

	stale := now.Add(-3 * time.Hour)
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "stale-1", KeyID: kid, Model: "m", HeldMicroUSD: 1_000_000, ReservedTokens: 10,
		ExpiresAt: stale.Add(time.Hour), Now: stale,
	}); err != nil {
		t.Fatal(err)
	}
	// 不带 SweepStaleBefore：旧行为，僵尸行保留。
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "new-1", KeyID: kid, Model: "m", HeldMicroUSD: 1_000_000, ReservedTokens: 10,
		ExpiresAt: now.Add(2 * time.Hour), Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	var held int
	if err := s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM reservations WHERE status='held'`).Scan(&held)
	}); err != nil {
		t.Fatal(err)
	}
	if held != 2 {
		t.Fatalf("未启用清扫时 held 行数 = %d, 期望 2", held)
	}
}

func TestReleaseStaleReservationsReleasesOnlyStale(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "release-stale")
	ctx := context.Background()
	now := time.Now().UTC()
	kid := seedKeyForQuota(t, s)

	stale := now.Add(-3 * time.Hour)
	for _, tc := range []struct {
		id      string
		now     time.Time
		expires time.Time
	}{
		// 心跳早于阈值 → 按心跳条件释放。
		{"dead-heartbeat", stale, now.Add(2 * time.Hour)},
		// 已过期 → 按过期条件释放。
		{"dead-expiry", stale, stale},
		{"alive", now, now.Add(2 * time.Hour)},
	} {
		if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
			ID: tc.id, KeyID: kid, Model: "m", HeldMicroUSD: 1, ReservedTokens: 1,
			ExpiresAt: tc.expires, Now: tc.now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ReleaseStaleReservations(ctx, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应释放 2 条陈旧预占，得到 %d", n)
	}
	var aliveHeld int
	if err := s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM reservations WHERE id='alive' AND status='held'`).Scan(&aliveHeld)
	}); err != nil {
		t.Fatal(err)
	}
	if aliveHeld != 1 {
		t.Fatal("心跳新鲜的预占不应被释放")
	}
}

// seedKeyForQuota 建一条可用的插件 Key，供预占用例引用。
func seedKeyForQuota(t *testing.T, s *Store) string {
	t.Helper()
	kid := "kidsweep01"
	if _, err := s.InsertKey(context.Background(), InsertKeyParams{
		KID: kid, KeyHash: []byte("test-hash"), EncryptedMaterial: []byte("test-enc"), PepperID: "p1",
		CallerScope: CallerScopeCaller, CallerID: DefaultCallerID,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return kid
}

// 时间列以 UnixMilli 整数落库；ListHeldReservations 曾直接扫 *time.Time 触发
// "unsupported Scan, storing driver.Value type int64 into type *time.Time"，
// 面板「进行中请求」整面板 500。此测试钉住整数扫描口径。
func TestListHeldReservationsScansMillisTimestamps(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "held-list")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	kid := seedKeyForQuota(t, s)

	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "held-1", KeyID: kid, Model: "m", HeldMicroUSD: 2_500, ReservedTokens: 42,
		ExpiresAt: now.Add(2 * time.Hour), Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 一条已结算行：不应出现在 held 视图里。
	if _, err := s.SettleReservation(ctx, "held-1", 2_500, 42, now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "held-2", KeyID: kid, Model: "m2", HeldMicroUSD: 1_000, ReservedTokens: 7,
		ExpiresAt: now.Add(2 * time.Hour), Now: now,
	}); err != nil {
		t.Fatal(err)
	}

	items, err := s.ListHeldReservations(ctx, now.Add(-2*time.Hour), now.Add(time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("held 行数 = %d, 期望 1（仅未结算行）", len(items))
	}
	h := items[0]
	if h.ID != "held-2" || h.Model != "m2" || h.ReservedTokens != 7 || h.HeldMicroUSD != 1_000 {
		t.Fatalf("行内容不符: %+v", h)
	}
	if !h.CreatedAt.Equal(now) {
		t.Fatalf("created_at 应为 %v, 得到 %v", now, h.CreatedAt)
	}
	if h.AgeSec != 60 {
		t.Fatalf("age_sec = %d, 期望 60", h.AgeSec)
	}
	if h.StaleMark {
		t.Fatal("心跳新鲜的行不应标记 stale")
	}
}
