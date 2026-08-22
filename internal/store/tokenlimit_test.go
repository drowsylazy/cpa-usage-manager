package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
)

// Token 限额与金额限额并列生效：任一触顶即拒绝。
//
// 回归点：token 限额是 v0.3.4 新增的第二把闸。金额限额控制成本，token 限额控制
// 用量 —— 混合模型下价差可达 50 倍（haiku vs opus），只靠金额无法精确约束用量。
// 以下测试锁住三件事：预占期就能拦住（不是等结算后才发现超了）、结算按真实
// token 累加而非估算值、跨周期自动归零。

func insertKeyWithTokenLimits(t *testing.T, s *Store, kid string, total, daily *int64) {
	t.Helper()
	if _, err := s.InsertKey(context.Background(), InsertKeyParams{
		KID: kid, KeyHash: []byte("h-" + kid), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: CallerScopeKey, CallerID: DefaultCallerID,
		TokenLimit: total, DailyTokenLimit: daily,
	}); err != nil {
		t.Fatalf("seed key %s: %v", kid, err)
	}
}

func i64(v int64) *int64 { return &v }

// microOf 构造可空金额上限。
func microOf(v int64) *money.Micro { m := money.Micro(v); return &m }

func TestTokenLimitBlocksReservationAtHoldTime(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "toklimit")
	ctx := context.Background()
	now := time.Now().UTC()

	// 总量上限 1000 token，无金额上限：证明 token 是独立的一把闸。
	insertKeyWithTokenLimits(t, s, "kidtok0001", i64(1000), nil)

	// 800 token 的预占应通过。
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "r1", KeyID: "kidtok0001", Model: "m", HeldMicroUSD: 0, ReservedTokens: 800,
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); err != nil {
		t.Fatalf("800 token 预占应通过: %v", err)
	}

	// 再来 300 token（累计 1100 > 1000）必须被拒 —— 在**预占期**就拦住，
	// 而不是放过去等结算才发现。
	_, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "r2", KeyID: "kidtok0001", Model: "m", HeldMicroUSD: 0, ReservedTokens: 300,
		ExpiresAt: now.Add(time.Hour), Now: now,
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("超出 token 总量应返回 ErrQuotaExceeded，得到 %v", err)
	}

	// 单次请求本身就超上限，也必须拒（amount > limit 分支）。
	_, _, err = s.HoldReservation(ctx, HoldReservationParams{
		ID: "r3", KeyID: "kidtok0001", Model: "m", HeldMicroUSD: 0, ReservedTokens: 5000,
		ExpiresAt: now.Add(time.Hour), Now: now,
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("单次超上限应返回 ErrQuotaExceeded，得到 %v", err)
	}
}

func TestTokenLimitCountsInFlightHolds(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "tokheld")
	ctx := context.Background()
	now := time.Now().UTC()
	insertKeyWithTokenLimits(t, s, "kidtok0002", i64(1000), nil)

	// 两笔在途预占各 400，累计 800；第三笔 300 会到 1100，必须被拒。
	// 这条锁住「在途预占计入 token 用量」——否则并发请求能一起冲破上限。
	for _, id := range []string{"h1", "h2"} {
		if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
			ID: id, KeyID: "kidtok0002", Model: "m", ReservedTokens: 400,
			ExpiresAt: now.Add(time.Hour), Now: now,
		}); err != nil {
			t.Fatalf("预占 %s: %v", id, err)
		}
	}
	_, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "h3", KeyID: "kidtok0002", Model: "m", ReservedTokens: 300,
		ExpiresAt: now.Add(time.Hour), Now: now,
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("在途预占应计入 token 用量，得到 %v", err)
	}
}

func TestSettleAccumulatesRealTokensNotEstimate(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "toksettle")
	ctx := context.Background()
	now := time.Now().UTC()
	insertKeyWithTokenLimits(t, s, "kidtok0003", i64(10000), i64(5000))

	// 估算 900，真实只用了 100：累计器必须记真实值。
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "s1", KeyID: "kidtok0003", Model: "m", ReservedTokens: 900,
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleReservation(ctx, "s1", 0, 100, now, nil); err != nil {
		t.Fatalf("结算: %v", err)
	}
	k, err := s.GetKey(ctx, "kidtok0003")
	if err != nil {
		t.Fatal(err)
	}
	if k.TokensUsed != 100 {
		t.Errorf("tokens_used 应为真实值 100，得到 %d", k.TokensUsed)
	}
	if k.DailyTokensUsed != 100 {
		t.Errorf("daily_tokens_used 应为 100，得到 %d", k.DailyTokensUsed)
	}
	// 预占已释放，在途归零：剩余额度应回到 10000-100。
	b := balanceTokens(t, s, "kidtok0003", now)
	if b != 9900 {
		t.Errorf("总 token 余量应为 9900，得到 %d", b)
	}
}

// balanceTokens 直接算「总量上限 - 已用 - 在途」，避免依赖 service 层。
func balanceTokens(t *testing.T, s *Store, kid string, now time.Time) int64 {
	t.Helper()
	k, err := s.GetKey(context.Background(), kid)
	if err != nil {
		t.Fatal(err)
	}
	var held int64
	if err := s.Read(context.Background(), func(q Querier) error {
		return q.QueryRowContext(context.Background(),
			`SELECT COALESCE(SUM(reserved_tokens),0) FROM reservations WHERE key_id=? AND status='held'`, kid).Scan(&held)
	}); err != nil {
		t.Fatal(err)
	}
	if k.TokenLimit == nil {
		return -1
	}
	return *k.TokenLimit - k.TokensUsed - held
}

func TestTokenCycleCountersRollOver(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "tokcycle")
	ctx := context.Background()

	// 日上限 1000。先在「昨天」用掉 900。
	insertKeyWithTokenLimits(t, s, "kidtok0004", nil, i64(1000))
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "c1", KeyID: "kidtok0004", Model: "m", ReservedTokens: 900,
		ExpiresAt: yesterday.Add(time.Hour), Now: yesterday,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleReservation(ctx, "c1", 0, 900, yesterday, nil); err != nil {
		t.Fatal(err)
	}

	// 今天再要 900：昨天的日累计已跨期作废，应当通过。
	// 这条锁住 token 累计器复用金额那套 cycle_key 归零机制确实生效。
	now := time.Now().UTC()
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "c2", KeyID: "kidtok0004", Model: "m", ReservedTokens: 900,
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); err != nil {
		t.Fatalf("跨日后日 token 累计应归零，却被拒: %v", err)
	}
}

func TestMoneyAndTokenLimitsAreIndependent(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "tokmoney")
	ctx := context.Background()
	now := time.Now().UTC()

	// 金额充裕（1 USD）但 token 上限极小（10）：token 那把闸必须独立生效。
	quota := microOf(1_000_000)
	if _, err := s.InsertKey(ctx, InsertKeyParams{
		KID: "kidtok0005", KeyHash: []byte("h5"), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: CallerScopeKey, CallerID: DefaultCallerID,
		QuotaMicroUSD: quota, TokenLimit: i64(10),
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "m1", KeyID: "kidtok0005", Model: "m", HeldMicroUSD: 1, ReservedTokens: 100,
		ExpiresAt: now.Add(time.Hour), Now: now,
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("金额够但 token 超限时应被拒，得到 %v", err)
	}

	// 反向：token 充裕但金额超限，同样应被拒（原有行为不能被破坏）。
	if _, err := s.InsertKey(ctx, InsertKeyParams{
		KID: "kidtok0006", KeyHash: []byte("h6"), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: CallerScopeKey, CallerID: DefaultCallerID,
		QuotaMicroUSD: microOf(10), TokenLimit: i64(1_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.HoldReservation(ctx, HoldReservationParams{
		ID: "m2", KeyID: "kidtok0006", Model: "m", HeldMicroUSD: 500, ReservedTokens: 5,
		ExpiresAt: now.Add(time.Hour), Now: now,
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("token 够但金额超限时应被拒，得到 %v", err)
	}
}

func TestNoTokenLimitMeansUnlimited(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "toknolimit")
	ctx := context.Background()
	now := time.Now().UTC()

	// 完全不配 token 限额（全 nil）：任意大的预占都不该被 token 闸拦住。
	// 这条守住向下兼容 —— 迁移后既有 Key 的 token 列全为 NULL。
	insertKeyWithTokenLimits(t, s, "kidtok0007", nil, nil)
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "u1", KeyID: "kidtok0007", Model: "m", ReservedTokens: 999_999_999,
		ExpiresAt: now.Add(time.Hour), Now: now,
	}); err != nil {
		t.Fatalf("未配 token 限额应视为不限，却被拒: %v", err)
	}
}
