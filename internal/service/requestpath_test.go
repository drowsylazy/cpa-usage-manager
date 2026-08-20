package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func TestResolveIdentityBearer(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	issued, err := s.IssueKey(ctx, IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+issued.Key)
	rec, err := s.ResolveIdentity(ctx, h, nil)
	if err != nil {
		t.Fatalf("Bearer 身份解析失败: %v", err)
	}
	if rec.KID != issued.KID {
		t.Fatalf("KID 不匹配: %s != %s", rec.KID, issued.KID)
	}
}

func TestResolveIdentityBadBearer(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	h := http.Header{}
	h.Set("Authorization", "Bearer cum-xxxx-bad")
	if _, err := s.ResolveIdentity(ctx, h, nil); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("坏密钥应失败: %v", err)
	}
}

func TestResolveIdentityCallerScope(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	issued, err := s.IssueKey(ctx, IssueRequest{CallerScope: store.CallerScopeCaller, CallerID: store.DefaultCallerID})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.ResolveIdentity(ctx, nil, map[string]any{CallerScopeMetadataKey: store.CallerScopeCaller})
	if err != nil {
		t.Fatalf("caller_scope 兜底解析失败: %v", err)
	}
	if rec.KID != issued.KID {
		t.Fatalf("caller_scope KID 不匹配: %s != %s", rec.KID, issued.KID)
	}
}

func TestResolveIdentityMissing(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if _, err := s.ResolveIdentity(ctx, nil, nil); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("缺少身份应失败: %v", err)
	}
}

func TestBuildReservePlanToken(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "gpt-test", Priority: 10, Enabled: true, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gpt-test","max_tokens":2048}`)
	plan, err := s.BuildReservePlan(ctx, "gpt-test", body)
	if err != nil {
		t.Fatal(err)
	}
	if plan.BillingMode != store.BillingModeToken {
		t.Fatalf("billing_mode = %s", plan.BillingMode)
	}
	if plan.OutputEstimate != 2048 {
		t.Fatalf("output = %d, 期望 2048", plan.OutputEstimate)
	}
	if plan.InputEstimate <= 0 {
		t.Fatalf("input 估算异常: %d", plan.InputEstimate)
	}
	if plan.TokenEstimate != plan.InputEstimate+plan.OutputEstimate {
		t.Fatalf("token 估算异常: %d", plan.TokenEstimate)
	}
}

func TestBuildReservePlanFree(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "gpt-free", Priority: 10, Enabled: true, BillingMode: store.BillingModeFree, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.BuildReservePlan(ctx, "gpt-free", []byte(`{"model":"gpt-free"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.BillingMode != store.BillingModeFree || plan.TokenEstimate != 1 || plan.ImageCount != 0 {
		t.Fatalf("free 计划异常: %+v", plan)
	}
}

func TestBuildReservePlanPerImage(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "img-test", Priority: 10, Enabled: true, BillingMode: store.BillingModePerImage, PerImageMicroUSD: 100, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.BuildReservePlan(ctx, "img-test", []byte(`{"model":"img-test","n":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.BillingMode != store.BillingModePerImage || plan.ImageCount != 3 {
		t.Fatalf("per_image 计划异常: %+v", plan)
	}
	if plan.TokenEstimate != 0 {
		t.Fatalf("per_image 不应估算 token: %d", plan.TokenEstimate)
	}
}

func TestBuildReservePlanDisabledRuleFallsBack(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	if _, err := st.UpsertPricingRule(ctx, store.PricingRule{MatchKind: store.MatchExact, Pattern: "gpt-off", Priority: 10, Enabled: false, Source: store.PricingSourceManual}); err != nil {
		t.Fatal(err)
	}
	// matchPricing 只读启用规则：禁用的显式规则不生效，模型落到全模型免费兜底规则。
	plan, err := s.BuildReservePlan(ctx, "gpt-off", []byte(`{"model":"gpt-off"}`))
	if err != nil {
		t.Fatalf("禁用规则应被跳过: %v", err)
	}
	if plan.BillingMode != store.BillingModeToken || plan.TokenEstimate <= 0 {
		t.Fatalf("应落到兜底 token 规则: %+v", plan)
	}
}

func TestEstimateTokens(t *testing.T) {
	in, out := estimateTokens([]byte(`{"max_completion_tokens":512}`), 4096, 1_000_000)
	if out != 512 {
		t.Fatalf("max_completion_tokens 未生效: %d", out)
	}
	if in != int64(len(`{"max_completion_tokens":512}`))/2+1 {
		t.Fatalf("input 估算异常: %d", in)
	}
	in, out = estimateTokens([]byte(`{}`), 4096, 1_000_000)
	if out != 4096 {
		t.Fatalf("默认输出预占异常: %d", out)
	}
	in, out = estimateTokens([]byte(`{"max_tokens":1000000000}`), 4096, 10_000)
	if out != 10_000 {
		t.Fatalf("输出封顶异常: %d", out)
	}
}

func TestExtractImageCount(t *testing.T) {
	if got := extractImageCount([]byte(`{"n":5}`)); got != 5 {
		t.Fatalf("n=5 -> %d", got)
	}
	if got := extractImageCount([]byte(`{"n":0}`)); got != 1 {
		t.Fatalf("n=0 -> %d", got)
	}
	if got := extractImageCount([]byte(`not-json`)); got != 1 {
		t.Fatalf("非 JSON -> %d", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "  ", "a", "b"); got != "a" {
		t.Fatalf("FirstNonEmpty = %q", got)
	}
	if got := FirstNonEmpty("", " "); got != "" {
		t.Fatalf("空输入 -> %q", got)
	}
}

func TestParseKeyID(t *testing.T) {
	if kid, ok := ParseKeyID("cum-abcdefgh-0123456789012345678901234567890123456789012345678901234567890123"); !ok || kid != "abcdefgh" {
		t.Fatalf("ParseKeyID = %q, %v", kid, ok)
	}
	if _, ok := ParseKeyID("tk-abcdefgh-secret"); ok {
		t.Fatal("非 cum- 前缀不应解析成功")
	}
	if _, ok := ParseKeyID("cum-single"); ok {
		t.Fatal("缺 secret 段不应解析成功")
	}
	if _, ok := ParseKeyID("cum-kid-"); ok {
		t.Fatal("空 secret 不应解析成功")
	}
}
