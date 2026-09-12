package service

import (
	"context"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// TestReservationAccuracyCached 钉住服务端 60s TTL 缓存：实时页 5s 轮询
// 本读数，缓存生效后底层数据变化不应立即反映在返回值里（扫描压力从每 5s
// 一次降到每分钟一次）。
func TestReservationAccuracyCached(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := st.InsertKey(ctx, store.InsertKeyParams{
		KID: "kidacc1", KeyHash: []byte("h"), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: store.CallerScopeCaller, CallerID: store.DefaultCallerID,
	}); err != nil {
		t.Fatal(err)
	}
	seedSettled := func(id, model string) {
		t.Helper()
		if _, _, err := st.HoldReservation(ctx, store.HoldReservationParams{
			ID: id, KeyID: "kidacc1", Model: model,
			HeldMicroUSD: 700, ReservedTokens: 300, Now: now.Add(-2 * time.Minute), ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SettleReservation(ctx, id, 120, 260, now.Add(-time.Minute), nil); err != nil {
			t.Fatal(err)
		}
	}
	seedSettled("acc-settled-1", "m-a")

	first, err := s.ReservationAccuracy(ctx, 100)
	if err != nil || len(first) != 1 || first[0].Model != "m-a" {
		t.Fatalf("首次读数应含 1 个模型: items=%+v err=%v", first, err)
	}
	// 缓存窗口内底层数据新增一个模型：返回值不变。
	seedSettled("acc-settled-2", "m-b")
	cached, err := s.ReservationAccuracy(ctx, 100)
	if err != nil || len(cached) != 1 || cached[0].Model != "m-a" {
		t.Fatalf("TTL 内应返回缓存: items=%+v err=%v", cached, err)
	}
}
