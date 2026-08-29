package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// 请求次数限额（日/月）与金额/Token 并列生效：任一触顶即拒绝。
//
// 回归点：请求次数限额是 schema v9 新增的第三把闸。金额/Token 控制成本与用量，
// 请求次数控制「脚本失控刷接口」——金额和 Token 都可能反应太钝。测试锁住：
// 预占期拦截、在途预占计入、结算 +1、跨周期归零、未配即不限。

func insertKeyWithRequestLimits(t *testing.T, s *Store, kid string, daily, monthly *int64) {
	t.Helper()
	if _, err := s.InsertKey(context.Background(), InsertKeyParams{
		KID: kid, KeyHash: []byte("h-" + kid), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: CallerScopeKey, CallerID: DefaultCallerID,
		DailyRequestsLimit: daily, MonthlyRequestsLimit: monthly,
	}); err != nil {
		t.Fatalf("seed key %s: %v", kid, err)
	}
}

func TestRequestLimitBlocksReservationAtHoldTime(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "reqlimit")
	ctx := context.Background()
	now := time.Now().UTC()

	insertKeyWithRequestLimits(t, s, "kidreq0001", i64(2), nil)
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "r1", KeyID: "kidreq0001", Model: "m",
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); err != nil {
		t.Fatalf("第 1 笔应通过: %v", err)
	}
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "r2", KeyID: "kidreq0001", Model: "m",
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); err != nil {
		t.Fatalf("第 2 笔应通过: %v", err)
	}
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "r3", KeyID: "kidreq0001", Model: "m",
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("第 3 笔应因日请求次数触顶被拒，得到 %v", err)
	}
}

func TestRequestLimitSettleCountsOne(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "reqsettle")
	ctx := context.Background()
	now := time.Now().UTC()

	insertKeyWithRequestLimits(t, s, "kidreq0002", i64(1), nil)
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "s1", KeyID: "kidreq0002", Model: "m",
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleReservation(ctx, "s1", 0, 0, now, nil); err != nil {
		t.Fatal(err)
	}
	k, err := s.GetKey(ctx, "kidreq0002")
	if err != nil {
		t.Fatal(err)
	}
	if k.DailyRequestsUsed != 1 {
		t.Fatalf("结算后 daily_requests_used 应为 1，得到 %d", k.DailyRequestsUsed)
	}
	// 已用 1/上限 1：下一笔必拒。
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "s2", KeyID: "kidreq0002", Model: "m",
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("已用满后应拒绝新请求，得到 %v", err)
	}
}

func TestRequestCycleCountersRollOver(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "reqcycle")
	ctx := context.Background()

	insertKeyWithRequestLimits(t, s, "kidreq0003", i64(1), nil)
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "c1", KeyID: "kidreq0003", Model: "m",
		ExpiresAt: yesterday.Add(time.Hour), Now: yesterday,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleReservation(ctx, "c1", 0, 0, yesterday, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "c2", KeyID: "kidreq0003", Model: "m",
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); err != nil {
		t.Fatalf("跨日后日请求累计应归零，却被拒: %v", err)
	}
}

func TestNoRequestLimitMeansUnlimited(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "reqnolimit")
	ctx := context.Background()
	now := time.Now().UTC()

	insertKeyWithRequestLimits(t, s, "kidreq0004", nil, nil)
	for i, id := range []string{"u1", "u2", "u3"} {
		if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
			ID: id, KeyID: "kidreq0004", Model: "m",
			ExpiresAt: now.Add(time.Hour), Now: now,
		}); err != nil {
			t.Fatalf("未配请求次数限额应视为不限（第 %d 笔被拒）: %v", i+1, err)
		}
	}
}

func TestZeroRequestLimitMeansDisabled(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "reqzero")
	ctx := context.Background()
	now := time.Now().UTC()

	// 0=禁用（真实限额，一笔都过不去），与金额/Token 口径一致。
	insertKeyWithRequestLimits(t, s, "kidreq0005", i64(0), nil)
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "z1", KeyID: "kidreq0005", Model: "m",
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("0 上限应全部拒付，得到 %v", err)
	}
}
