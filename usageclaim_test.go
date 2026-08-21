//go:build cgo

package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// 认领登记表是「请求重复统计」的正解：宿主 usage.handle 不带请求 ID，
// 只有进程内配对才能保证同一次上游调用只落一行。这里覆盖它的关键分支。

func TestNormalizeModelKey(t *testing.T) {
	cases := map[string]string{
		"  GPT-5 ":                  "gpt-5",
		"azure/gpt-5":               "gpt-5",
		"openrouter/anthropic/opus": "opus",
		"gpt-5":                     "gpt-5",
		"":                          "",
		"trailing/":                 "trailing/", // 尾随斜杠不剥离，避免得到空键
	}
	for in, want := range cases {
		if got := normalizeModelKey(in); got != want {
			t.Fatalf("normalizeModelKey(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestUsageClaimRendezvous(t *testing.T) {
	resetUsageClaims()
	t.Cleanup(resetUsageClaims)

	// 执行器登记「渠道/模型」别名，宿主上报裸模型名，两者靠归一化后的裸名配对。
	claim := registerUsageClaim("kid1", "azure/gpt-5", "gpt-5")
	if !claim.registered {
		t.Fatal("有模型名时应入册")
	}

	id, ok := claimHostUsage(rpcUsageRecord{Model: "gpt-5", APIKey: "cum-kid1-secret",
		Detail: rpcUsageDetail{TotalTokens: 1000}})
	if !ok {
		t.Fatal("宿主记录应被认领")
	}
	if id != "" {
		t.Fatalf("尚未结算时不应返回请求 ID，得到 %q", id)
	}

	// 同一认领不会被交付两次：第二条同模型记录必须回落到被动路径。
	if _, ok := claimHostUsage(rpcUsageRecord{Model: "gpt-5"}); ok {
		t.Fatal("已交付的认领不应再次命中")
	}

	rec, ok := claim.wait(0)
	if !ok || rec.Detail.TotalTokens != 1000 {
		t.Fatalf("认领方应读到宿主用量: ok=%v rec=%+v", ok, rec)
	}
}

// TestUsageClaimSettleBeforeAttach 覆盖「先结算、宿主回调后到」：
// attach 返回已落库的请求 ID，由 usage.handle 侧回填。
func TestUsageClaimSettleBeforeAttach(t *testing.T) {
	resetUsageClaims()
	t.Cleanup(resetUsageClaims)

	claim := registerUsageClaim("kid1", "gpt-5")
	if rec := claim.settled("req-1"); rec != nil {
		t.Fatalf("结算时还没有宿主用量，应返回 nil，得到 %+v", rec)
	}
	id, ok := claimHostUsage(rpcUsageRecord{Model: "gpt-5"})
	if !ok || id != "req-1" {
		t.Fatalf("晚到的回调应拿到已落库的请求 ID: ok=%v id=%q", ok, id)
	}
}

// TestUsageClaimAttachBeforeSettle 覆盖「宿主回调先到、后结算」：
// attach 侧拿不到请求 ID，改由 settled 把记录交还结算方。
func TestUsageClaimAttachBeforeSettle(t *testing.T) {
	resetUsageClaims()
	t.Cleanup(resetUsageClaims)

	claim := registerUsageClaim("kid1", "gpt-5")
	id, ok := claimHostUsage(rpcUsageRecord{Model: "gpt-5", Detail: rpcUsageDetail{TotalTokens: 7}})
	if !ok || id != "" {
		t.Fatalf("结算前交付应返回空 ID: ok=%v id=%q", ok, id)
	}
	rec := claim.settled("req-2")
	if rec == nil || rec.Detail.TotalTokens != 7 {
		t.Fatalf("结算方应接手已交付的宿主用量: %+v", rec)
	}
}

// TestUsageClaimBackfillExactlyOnce 并发跑 settled 与 attach，
// 断言「恰好一方」看到对方的数据——否则会回填两次或一次都不回填。
func TestUsageClaimBackfillExactlyOnce(t *testing.T) {
	for i := 0; i < 200; i++ {
		resetUsageClaims()
		claim := registerUsageClaim("kid1", "gpt-5")

		var wg sync.WaitGroup
		var settleSaw bool
		var attachID string
		var attachOK bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			settleSaw = claim.settled("req-x") != nil
		}()
		go func() {
			defer wg.Done()
			attachID, attachOK = claimHostUsage(rpcUsageRecord{Model: "gpt-5"})
		}()
		wg.Wait()

		if !attachOK {
			t.Fatalf("第 %d 轮：认领未命中", i)
		}
		attachSaw := attachID != ""
		if settleSaw == attachSaw {
			t.Fatalf("第 %d 轮：回填次数应恰好为 1，settled 看到=%v attach 看到=%v",
				i, settleSaw, attachSaw)
		}
	}
	resetUsageClaims()
}

func TestUsageClaimKIDPreferredOverFIFO(t *testing.T) {
	resetUsageClaims()
	t.Cleanup(resetUsageClaims)

	// kid 不含连字符（cum-<kid>-<secret> 以首个连字符分段）。
	first := registerUsageClaim("kida", "gpt-5")
	second := registerUsageClaim("kidb", "gpt-5")

	// kid 命中的认领优先于更早登记的那条。
	if _, ok := claimHostUsage(rpcUsageRecord{Model: "gpt-5", APIKey: "cum-kidb-secret"}); !ok {
		t.Fatal("应命中认领")
	}
	if _, ok := second.wait(0); !ok {
		t.Fatal("kid 匹配的认领应拿到记录")
	}
	if _, ok := first.wait(0); ok {
		t.Fatal("kid 不匹配的认领不应被交付")
	}

	// kid 无法解析时退回 FIFO：交给最早登记的那条。
	if _, ok := claimHostUsage(rpcUsageRecord{Model: "gpt-5", APIKey: "sk-upstream-credential"}); !ok {
		t.Fatal("kid 不可解析时也应命中认领")
	}
	if _, ok := first.wait(0); !ok {
		t.Fatal("FIFO 应交给最早登记的认领")
	}
}

func TestUsageClaimReleaseFallsBackToPassive(t *testing.T) {
	resetUsageClaims()
	t.Cleanup(resetUsageClaims)

	claim := registerUsageClaim("kid1", "gpt-5")
	claim.release(0)
	if _, ok := claimHostUsage(rpcUsageRecord{Model: "gpt-5"}); ok {
		t.Fatal("放弃认领后宿主回调必须回落到被动统计路径")
	}
}

func TestUsageClaimNoModelNotRegistered(t *testing.T) {
	resetUsageClaims()
	t.Cleanup(resetUsageClaims)

	claim := registerUsageClaim("kid1", "", "  ")
	if claim.registered {
		t.Fatal("没有可匹配模型名时不应入册")
	}
	if _, ok := claim.wait(time.Millisecond); ok {
		t.Fatal("未入册的认领不应返回数据")
	}
	if _, ok := claimHostUsage(rpcUsageRecord{Model: "gpt-5"}); ok {
		t.Fatal("登记表为空时不应命中")
	}
	// 未入册的认领上调用 release/settled 必须安全。
	claim.release(0)
	if rec := claim.settled("req-3"); rec != nil {
		t.Fatalf("未入册的认领 settled 应返回 nil，得到 %+v", rec)
	}
}

func TestUsageClaimWaitBlocksUntilDelivered(t *testing.T) {
	resetUsageClaims()
	t.Cleanup(resetUsageClaims)

	claim := registerUsageClaim("kid1", "gpt-5")
	go func() {
		time.Sleep(20 * time.Millisecond)
		claimHostUsage(rpcUsageRecord{Model: "gpt-5", Detail: rpcUsageDetail{TotalTokens: 42}})
	}()
	start := time.Now()
	rec, ok := claim.wait(2 * time.Second)
	if !ok || rec.Detail.TotalTokens != 42 {
		t.Fatalf("wait 应等到交付: ok=%v rec=%+v", ok, rec)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wait 不应等满超时: %s", elapsed)
	}
}

func TestUsageClaimWaitTimesOut(t *testing.T) {
	resetUsageClaims()
	t.Cleanup(resetUsageClaims)

	claim := registerUsageClaim("kid1", "gpt-5")
	if _, ok := claim.wait(20 * time.Millisecond); ok {
		t.Fatal("无交付时 wait 应超时返回 false")
	}
}

// TestApplyHostUsageOnlyFillsBlanks 断言宿主字段只补空值：
// provider/auth_type/tier 是分钟聚合的维度键，覆盖已有值会让请求行与聚合行错位。
func TestApplyHostUsageOnlyFillsBlanks(t *testing.T) {
	r := storeRequestForTest()
	r.Provider = "已有渠道"
	r.TTFTMS = 111
	applyHostUsageToRequest(r, rpcUsageRecord{
		Provider: "宿主渠道", AuthID: "auth-1", AuthType: "api_key",
		ServiceTier: "default", ReasoningEffort: "high", TTFT: 900 * time.Millisecond,
	})
	if r.Provider != "已有渠道" {
		t.Fatalf("已有 provider 不应被覆盖: %q", r.Provider)
	}
	if r.TTFTMS != 111 {
		t.Fatalf("已有首字延迟不应被覆盖: %d", r.TTFTMS)
	}
	if r.AuthID != "auth-1" || r.AuthType != "api_key" || r.Tier != "default" {
		t.Fatalf("空字段应被补齐: %+v", r)
	}
	if r.ThinkingIntensity != "high" {
		t.Fatalf("思考强度应被补齐: %q", r.ThinkingIntensity)
	}

	// nil 请求不得 panic（执行器早退路径可能传 nil）。
	applyHostUsageToRequest(nil, rpcUsageRecord{Provider: "x"})

	// 宿主字段带空白时须清洗后再写入。
	r2 := storeRequestForTest()
	applyHostUsageToRequest(r2, rpcUsageRecord{Provider: "  openai  "})
	if r2.Provider != "openai" || strings.TrimSpace(r2.Provider) != r2.Provider {
		t.Fatalf("provider 未清洗: %q", r2.Provider)
	}
}

func storeRequestForTest() *store.Request { return &store.Request{} }
