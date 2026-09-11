package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestListRecentReservations 锁住「最近预占回顾」查询：settled 携带实结金额、
// released 实结为 0（未走到结算即释放）、按完结时刻倒序、不混入在途行、
// 时间列 UnixMilli 口径与 AgeMS 全程耗时。
func TestListRecentReservations(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "recent")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	kid := "kidrecent1"
	if _, err := s.InsertKey(ctx, InsertKeyParams{
		KID: kid, KeyHash: []byte("h"), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: CallerScopeCaller, CallerID: DefaultCallerID,
	}); err != nil {
		t.Fatal(err)
	}

	// 在途行：不应出现在回顾里。
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "rec-held", KeyID: kid, Model: "m-held",
		HeldMicroUSD: 500, ReservedTokens: 100, Now: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// 已结算：完结于 now-1min。
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "rec-settled", KeyID: kid, Model: "m-a",
		HeldMicroUSD: 700, ReservedTokens: 300, Now: now.Add(-2 * time.Minute), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleReservation(ctx, "rec-settled", 120, 260, now.Add(-time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	// 已释放（未结算）：完结于 now-30s。
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "rec-released", KeyID: kid, Model: "m-b",
		HeldMicroUSD: 900, ReservedTokens: 400, Now: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseReservation(ctx, "rec-released", now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListRecentReservations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条已完结预占，得到 %d", len(got))
	}
	// 倒序：released 完结更晚，排首。
	if got[0].ID != "rec-released" || got[1].ID != "rec-settled" {
		t.Fatalf("完结时刻倒序错误: %s, %s", got[0].ID, got[1].ID)
	}
	rel, set := got[0], got[1]
	if rel.Status != "released" || rel.SettledMicroUSD != 0 || rel.HeldMicroUSD != 900 || rel.SettledTokens != 0 {
		t.Fatalf("released 行字段异常: %+v", rel)
	}
	if set.Status != "settled" || set.SettledMicroUSD != 120 || set.HeldMicroUSD != 700 || set.ReservedTokens != 300 || set.SettledTokens != 260 {
		t.Fatalf("settled 行字段异常: %+v", set)
	}
	if set.AgeMS != int64(time.Minute/time.Millisecond) {
		t.Fatalf("settled 全程耗时应为 1 分钟（创建到完结），得到 %dms", set.AgeMS)
	}
	if set.FinishedAt.IsZero() || set.CreatedAt.IsZero() {
		t.Fatalf("时间字段不应为零值: %+v", set)
	}
}

// TestReservationAccuracy 锁住预占精度聚合：只统计 settled 且两侧 token
// 均为正的行，P50/P95 分位数落点正确，输出按样本数降序。
func TestReservationAccuracy(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "acc")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	kid := "kidacc1"
	if _, err := s.InsertKey(ctx, InsertKeyParams{
		KID: kid, KeyHash: []byte("h"), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: CallerScopeCaller, CallerID: DefaultCallerID,
	}); err != nil {
		t.Fatal(err)
	}

	settle := func(id, model string, reserved, tokens int64, at time.Time) {
		t.Helper()
		if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
			ID: id, KeyID: kid, Model: model,
			HeldMicroUSD: 100, ReservedTokens: reserved, Now: at, ExpiresAt: at.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SettleReservation(ctx, id, 50, tokens, at.Add(time.Minute), nil); err != nil {
			t.Fatal(err)
		}
	}
	// m-a 三条样本：占比 70% / 100% / 130% → P50=100%、P95=130%。
	settle("acc-a1", "m-a", 1000, 700, now.Add(-5*time.Minute))
	settle("acc-a2", "m-a", 1000, 1000, now.Add(-4*time.Minute))
	settle("acc-a3", "m-a", 1000, 1300, now.Add(-3*time.Minute))
	// m-b 单条：P50=P95。
	settle("acc-b1", "m-b", 500, 450, now.Add(-2*time.Minute))
	// 干扰行：released、零 token settled、在途——都不应计入。
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "acc-rel", KeyID: kid, Model: "m-a",
		HeldMicroUSD: 100, ReservedTokens: 100, Now: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseReservation(ctx, "acc-rel", now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "acc-zero", KeyID: kid, Model: "m-a",
		HeldMicroUSD: 100, ReservedTokens: 100, Now: now.Add(-40 * time.Second), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleReservation(ctx, "acc-zero", 0, 0, now.Add(-20*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "acc-held", KeyID: kid, Model: "m-a",
		HeldMicroUSD: 100, ReservedTokens: 100, Now: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReservationAccuracy(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应只有 2 个模型（干扰行全排除），得到 %d: %+v", len(got), got)
	}
	if got[0].Model != "m-a" || got[0].Samples != 3 {
		t.Fatalf("样本多的模型应排首: %+v", got[0])
	}
	if got[0].P50RatioMilli != 1000 || got[0].P95RatioMilli != 1300 {
		t.Fatalf("m-a 分位数错误: P50=%d P95=%d（期望 1000/1300）", got[0].P50RatioMilli, got[0].P95RatioMilli)
	}
	if got[1].Model != "m-b" || got[1].Samples != 1 || got[1].P50RatioMilli != 900 || got[1].P95RatioMilli != 900 {
		t.Fatalf("m-b 单样本行异常: %+v", got[1])
	}
}
