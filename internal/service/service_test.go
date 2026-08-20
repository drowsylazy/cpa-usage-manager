package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
	"github.com/drowsylazy/cpa-usage-manager/internal/usageparse"
)

func testService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	c := config.Default()
	c.DataDir = t.TempDir()
	c.DatabaseFile = "test.db"
	if err := c.EnsureDataDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(c.DataDir, c.DatabaseFile), OwnerID: "service-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ps, err := LoadPeppers(c, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return New(st, c, ps), st
}

func TestIssueAuthenticateRotateReveal(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	q := money.Micro(10)
	issued, err := s.IssueKey(ctx, IssueRequest{CallerID: store.DefaultCallerID, QuotaMicroUSD: &q, AllowedModels: []string{"gpt-*"}, Actor: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if issued.Key == "" || issued.Record.KeyHash != nil && len(issued.Record.KeyHash) == 0 {
		t.Fatal("签发结果异常")
	}
	auth, err := s.Authenticate(ctx, issued.Key)
	if err != nil || auth.Record.KID != issued.KID {
		t.Fatalf("鉴权失败: %v", err)
	}
	if _, err := s.Authenticate(ctx, "cum-"+issued.KID+"-bad"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("错误密钥应失败: %v", err)
	}
	plain, err := s.RevealKey(ctx, issued.KID, "admin")
	if err != nil || plain != issued.Key {
		t.Fatalf("解密回显异常: %q %v", plain, err)
	}
	rotated, err := s.RotateKey(ctx, issued.KID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, issued.Key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("轮换后旧 Key 仍可用: %v", err)
	}
	if _, err := s.Authenticate(ctx, rotated.Key); err != nil {
		t.Fatalf("轮换后新 Key 不可用: %v", err)
	}
	events, err := st.ListAudit(ctx, 20, 0)
	if err != nil || len(events) < 3 {
		t.Fatalf("审计不足: %d %v", len(events), err)
	}
}

func TestReserveSettleAndQuotaBoundary(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	price := money.Price(1)
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "gpt-test", Priority: 10, Enabled: true, PriceInput: price, PriceOutput: price, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	quota := money.Micro(2)
	issued, err := s.IssueKey(ctx, IssueRequest{QuotaMicroUSD: &quota, AllowedModels: []string{"gpt-*"}})
	if err != nil {
		t.Fatal(err)
	}
	k := issued.KID
	res, err := s.Reserve(ctx, ReservationRequest{KeyID: k, Model: "gpt-test", EstimatedTokens: 1_000_000, IdempotencyKey: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	resAgain, err := s.Reserve(ctx, ReservationRequest{KeyID: k, Model: "gpt-test", EstimatedTokens: 1_000_000, IdempotencyKey: "req-1"})
	if err != nil || resAgain.ID != res.ID {
		t.Fatalf("幂等预占异常: %v", err)
	}
	usage := usageparse.Usage{InputTokens: 1, OutputTokens: 1, InputIncludesCache: true, ReasoningInOutput: true}
	req := &store.Request{ID: "request-1", KeyID: k, CallerID: store.DefaultCallerID, Model: "gpt-test", Result: store.ResultOK}
	if _, err := s.Settle(ctx, res.ID, usage, req); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRequest(ctx, req.ID)
	if err != nil || got.CostMicroUSD != 2 {
		t.Fatalf("usage 入库费用异常: %+v %v", got, err)
	}
	if _, err := s.Reserve(ctx, ReservationRequest{KeyID: k, Model: "gpt-test", EstimatedTokens: 1_000_000}); !errors.Is(err, store.ErrQuotaExceeded) && !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("超额预占应失败: %v", err)
	}
}

func TestMissingUsageReleaseAndModelLimit(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	issued, err := s.IssueKey(ctx, IssueRequest{AllowedModels: []string{"allowed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, ReservationRequest{KeyID: issued.KID, Model: "blocked", EstimatedTokens: 1}); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("模型限制未生效: %v", err)
	}
	res, err := s.Reserve(ctx, ReservationRequest{KeyID: issued.KID, Model: "allowed", EstimatedTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 默认策略为 settle_reserved：缺失 usage 仍结算预占。
	settled, err := s.Settle(ctx, res.ID, usageparse.Usage{}, nil)
	if err != nil || settled.Status != store.ReservationSettled {
		t.Fatalf("缺失 usage 结算异常: %+v %v", settled, err)
	}

	c := config.Default()
	c.DataDir = t.TempDir()
	c.Quota.Settlement.MissingUsage = config.MissingUsageRelease
	ps, err := LoadPeppers(c, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(c.DataDir, "release.db"), OwnerID: "release-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s2 := New(st, c, ps)
	i2, err := s2.IssueKey(ctx, IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s2.Reserve(ctx, ReservationRequest{KeyID: i2.KID, Model: "x", EstimatedTokens: 1, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	released, err := s2.Settle(ctx, r2.ID, usageparse.Usage{}, nil)
	if err != nil || released.Status != store.ReservationReleased {
		t.Fatalf("缺失 usage release 异常: %+v %v", released, err)
	}
}
