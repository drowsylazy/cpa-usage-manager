//go:build ignore

// seed.go 为 UI 评估注入拟真数据（仅开发用，不参与构建）。
//
// 用法：
//
//	go run scripts/seed.go            # 写入默认 data_dir（与 devserver 同库）
//	CPA_DEV_DATA_DIR=/tmp/x go run scripts/seed.go
//
// 固定随机种子，可重复运行；每次运行生成新的请求 ID，不会覆盖旧数据。
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

type modelSpec struct {
	name     string
	provider string
	source   string
	weight   int
	inPrice  money.Price // micro-USD / 百万 token
	outPrice money.Price
	cacheR   money.Price
	cacheW   money.Price
	cacheAmt float64 // 命中缓存的概率
}

// 12+ 个模型：覆盖多提供方、超长名（测省略号）、免费模型（测计价覆盖率 <100%）。
var models = []modelSpec{
	{"claude-sonnet-4", "anthropic", "claude-code", 26, 3_000_000, 15_000_000, 300_000, 3_750_000, .55},
	{"claude-opus-4", "anthropic", "claude-code", 8, 15_000_000, 75_000_000, 1_500_000, 18_750_000, .45},
	{"claude-haiku-3.5", "anthropic", "api", 10, 800_000, 4_000_000, 80_000, 1_000_000, .30},
	{"gpt-4o", "openai", "api", 16, 2_500_000, 10_000_000, 1_250_000, 0, .35},
	{"gpt-4o-mini", "openai", "api", 12, 150_000, 600_000, 75_000, 0, .20},
	{"o3-mini", "openai", "api", 5, 1_100_000, 4_400_000, 550_000, 0, .15},
	{"gemini-2.5-pro", "google", "gemini-cli", 11, 1_250_000, 10_000_000, 310_000, 0, .40},
	{"gemini-2.5-flash", "google", "gemini-cli", 9, 300_000, 2_500_000, 75_000, 0, .25},
	{"openrouter/qwen/qwen-2.5-72b-instruct-turbo", "openrouter", "openrouter", 6, 900_000, 900_000, 0, 0, 0},
	{"openrouter/deepseek/deepseek-r1-distill-llama-70b", "openrouter", "openrouter", 5, 750_000, 990_000, 0, 0, 0},
	{"openrouter/meta-llama/llama-3.3-70b-instruct", "openrouter", "openrouter", 4, 590_000, 790_000, 0, 0, 0},
	{"local/qwen3-30b-a3b-instruct-gguf-q4km", "local", "lmstudio", 3, 0, 0, 0, 0, 0}, // 未计价
	{"grok-2-vision", "xai", "api", 2, 2_000_000, 10_000_000, 0, 0, 0},                // 无规则 → 未计价
}

type keySpec struct {
	label   string
	quota   *money.Micro // nil = 不限
	daily   *money.Micro
	conc    int
	weight  int
	fill    float64 // 目标额度占用比（驱动仪表状态：idle/ok/warn/alarm）
	disable bool
	revoke  bool
}

func mic(usd float64) *money.Micro { m := money.Micro(usd * 1e6); return &m }

var keySpecs = []keySpec{
	{label: "本地开发", quota: mic(50), daily: mic(5), conc: 4, weight: 22, fill: .34},
	{label: "CI 流水线", quota: mic(200), daily: mic(20), conc: 12, weight: 26, fill: .82}, // warn
	{label: "移动端 App", quota: mic(120), conc: 8, weight: 18, fill: .97},                  // alarm
	{label: "数据分析脚本", quota: nil, conc: 0, weight: 14, fill: 0},                          // 不限
	{label: "", quota: mic(30), conc: 2, weight: 8, fill: .11},                          // 无标签
	{label: "长标签测试：市场部门季度报告自动生成流水线（负责人：李四）", quota: mic(80), conc: 3, weight: 6, fill: .58},
	{label: "已停用的旧密钥", quota: mic(25), conc: 1, weight: 3, fill: .44, disable: true},
	{label: "已撤销的泄露密钥", quota: mic(10), conc: 1, weight: 1, fill: .9, revoke: true},
}

func main() {
	c := config.Default()
	if v := os.Getenv("CPA_DEV_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if err := c.EnsureDataDir(); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(c.DataDir, c.DatabaseFile), OwnerID: "seed"})
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	ps, err := service.LoadPeppers(c, os.LookupEnv)
	if err != nil {
		log.Fatal(err)
	}
	svc := service.New(st, c, ps)
	rng := rand.New(rand.NewSource(0xC0FFEE))

	// ── 计价规则 ────────────────────────────────────────────────
	rules := 0
	for _, m := range models {
		if m.inPrice == 0 && m.outPrice == 0 {
			continue // 故意留空，制造「未命中计价」
		}
		if m.provider == "xai" {
			continue // 故意留空
		}
		if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
			MatchKind: store.MatchExact, Pattern: m.name, Priority: 100, Enabled: true,
			PriceInput: m.inPrice, PriceOutput: m.outPrice,
			PriceCacheRead: m.cacheR, PriceCacheCreation: m.cacheW,
			AccountingMode: "default", BillingMode: "token", Source: "manual",
		}); err != nil {
			log.Fatalf("计价规则 %s: %v", m.name, err)
		}
		rules++
	}
	// glob 规则：演示优先级与通配
	for _, g := range []struct {
		pat            string
		pri            int
		in, out        money.Price
		cacheR, cacheW money.Price
	}{
		{"claude-*", 50, 3_000_000, 15_000_000, 300_000, 3_750_000},
		{"gpt-*", 50, 2_500_000, 10_000_000, 1_250_000, 0},
		{"gemini-*", 50, 1_250_000, 10_000_000, 310_000, 0},
		{"openrouter/*", 40, 800_000, 900_000, 0, 0},
	} {
		if _, err := st.UpsertPricingRule(ctx, store.PricingRule{
			MatchKind: store.MatchGlob, Pattern: g.pat, Priority: g.pri, Enabled: true,
			PriceInput: g.in, PriceOutput: g.out,
			PriceCacheRead: g.cacheR, PriceCacheCreation: g.cacheW,
			AccountingMode: "default", BillingMode: "token", Source: "manual",
		}); err != nil {
			log.Fatalf("glob 规则 %s: %v", g.pat, err)
		}
		rules++
	}

	// ── 密钥 ────────────────────────────────────────────────────
	type liveKey struct {
		kid    string
		spec   keySpec
		weight int
	}
	var live []liveKey
	for _, ks := range keySpecs {
		req := service.IssueRequest{
			Label: ks.label, CallerID: store.DefaultCallerID,
			MaxConcurrentRequests: ks.conc, Actor: "seed",
		}
		if ks.quota != nil {
			q := *ks.quota
			req.QuotaMicroUSD = &q
		}
		if ks.daily != nil {
			d := *ks.daily
			req.DailyMicroUSD = &d
		}
		ik, err := svc.IssueKey(ctx, req)
		if err != nil {
			log.Fatalf("签发密钥 %q: %v", ks.label, err)
		}
		live = append(live, liveKey{kid: ik.KID, spec: ks, weight: ks.weight})
	}

	// ── 请求记录 ────────────────────────────────────────────────
	now := time.Now().UTC()
	// 过去 30 天；第 12 天整天留空（测趋势图零值缺口）。
	gapDay := 12
	totalW := 0
	for _, m := range models {
		totalW += m.weight
	}
	keyW := 0
	for _, k := range live {
		keyW += k.weight
	}
	pickModel := func() modelSpec {
		n := rng.Intn(totalW)
		for _, m := range models {
			if n -= m.weight; n < 0 {
				return m
			}
		}
		return models[0]
	}
	pickKey := func() (string, keySpec) {
		if rng.Float64() < .12 { // 被动统计记录：无 key
			return "", keySpec{}
		}
		n := rng.Intn(keyW)
		for _, k := range live {
			if n -= k.weight; n < 0 {
				return k.kid, k.spec
			}
		}
		return live[0].kid, live[0].spec
	}

	inserted, failures := 0, 0
	var costTotal money.Micro
	for d := 29; d >= 0; d-- {
		if d == gapDay {
			continue
		}
		// 近几天更密集
		base := 40 + (30-d)*6
		count := base/2 + rng.Intn(base)
		for i := 0; i < count; i++ {
			// 昼夜分布：白天权重高
			hour := diurnalHour(rng)
			ts := now.AddDate(0, 0, -d).
				Truncate(24 * time.Hour).
				Add(time.Duration(hour)*time.Hour +
					time.Duration(rng.Intn(60))*time.Minute +
					time.Duration(rng.Intn(60))*time.Second)
			if ts.After(now) {
				continue
			}
			m := pickModel()
			kid, _ := pickKey()

			in := int64(400 + rng.Intn(24000))
			if rng.Float64() < .15 {
				in = int64(30000 + rng.Intn(120000)) // 长上下文
			}
			out := int64(60 + rng.Intn(2400))
			var cacheR, cacheW, cached int64
			if m.cacheAmt > 0 && rng.Float64() < m.cacheAmt {
				if m.provider == "anthropic" {
					cacheR = int64(float64(in) * (0.3 + rng.Float64()*0.6))
					if rng.Float64() < .25 {
						cacheW = int64(float64(in) * (0.1 + rng.Float64()*0.3))
					}
				} else {
					cached = int64(float64(in) * (0.2 + rng.Float64()*0.5)) // 含于输入
				}
			}
			reasoning := int64(0)
			if m.name == "o3-mini" || m.name == "gemini-2.5-pro" {
				if rng.Float64() < .5 {
					reasoning = int64(200 + rng.Intn(3000))
				}
			}

			result := store.ResultOK
			isFail := rng.Float64() < .07
			if isFail {
				result = "error"
				out, cacheR, cacheW, cached, reasoning = 0, 0, 0, 0, 0
				failures++
			}

			ttft := int64(180 + rng.Intn(2600))
			gen := int64(300 + rng.Intn(9000))
			latency := ttft + gen
			var tpsMilli int64
			if out > 0 && gen > 0 {
				tps := float64(out) / (float64(gen) / 1000)
				if tps > 3000 {
					tps = 200 + rng.Float64()*600 // 保持在可信上限内
				}
				tpsMilli = int64(tps * 1000)
			}

			total := in + out + cacheR + cacheW
			cost := priceOf(m, in-cached, out, cacheR, cacheW)
			priced := m.inPrice != 0 || m.outPrice != 0
			if isFail {
				cost = 0
			}
			costTotal += cost

			r := store.Request{
				ID:       fmt.Sprintf("seed-%d-%d-%d", now.UnixNano(), d, i),
				TS:       ts,
				KeyID:    kid,
				CallerID: store.DefaultCallerID,
				Model:    m.name, Provider: m.provider, Source: m.source,
				AuthType: authTypeOf(m.provider),
				AuthID:   m.provider + "-acct-" + fmt.Sprint(1+rng.Intn(2)),
				Tier:     tierOf(rng),
				Result:   result,
				InputTokens: in, OutputTokens: out, ReasoningTokens: reasoning,
				CachedTokens: cached, CacheReadTokens: cacheR, CacheCreationTokens: cacheW,
				TotalTokens: total,
				LatencyMS:   latency, TTFTMS: ttft, GenerationMS: gen, TPSMilli: tpsMilli,
				CostMicroUSD: cost, Priced: priced,
			}
			r.AuthLabel = r.AuthID
			if err := st.RecordUsage(ctx, r); err != nil {
				log.Fatalf("写入请求: %v", err)
			}
			inserted++
		}
	}

	// ── 密钥状态：禁用 / 撤销 ────────────────────────────────────
	for _, k := range live {
		if k.spec.revoke {
			if err := svc.RevokeKey(ctx, k.kid, "seed"); err != nil {
				log.Printf("撤销 %s 失败（忽略）：%v", k.kid, err)
			}
		} else if k.spec.disable {
			f := false
			if _, err := svc.UpdateKey(ctx, k.kid, store.KeyUpdate{Enabled: &f}, "seed"); err != nil {
				log.Printf("停用 %s 失败（忽略）：%v", k.kid, err)
			}
		}
	}

	fmt.Printf("已注入：%d 枚密钥 · %d 条计价规则 · %d 条请求（含 %d 条失败）· 合计 $%.2f\n",
		len(live), rules, inserted, failures, float64(costTotal)/1e6)
	fmt.Printf("模型 %d 个 · 时间跨度 30 天（第 %d 天留空以测零值缺口）\n", len(models), gapDay)
	fmt.Println("dev server: go run scripts/devserver.go  →  http://127.0.0.1:18080/console")
}

// diurnalHour 返回带昼夜分布的小时（白天概率高）。
func diurnalHour(rng *rand.Rand) int {
	w := [24]int{1, 1, 1, 1, 1, 2, 3, 5, 8, 11, 13, 13, 11, 12, 13, 13, 12, 10, 8, 6, 5, 4, 3, 2}
	sum := 0
	for _, v := range w {
		sum += v
	}
	n := rng.Intn(sum)
	for h, v := range w {
		if n -= v; n < 0 {
			return h
		}
	}
	return 12
}

func authTypeOf(provider string) string {
	switch provider {
	case "anthropic", "google":
		return "oauth"
	case "local":
		return "none"
	default:
		return "api_key"
	}
}

func tierOf(rng *rand.Rand) string {
	if rng.Float64() < .3 {
		return "standard"
	}
	return ""
}

// priceOf 按四类分别向上取整后相加（与 money 口径一致）。
func priceOf(m modelSpec, in, out, cacheR, cacheW int64) money.Micro {
	ceilDiv := func(tokens int64, price money.Price) money.Micro {
		if tokens <= 0 || price == 0 {
			return 0
		}
		num := tokens * int64(price)
		return money.Micro((num + 1e6 - 1) / 1e6)
	}
	if in < 0 {
		in = 0
	}
	return ceilDiv(in, m.inPrice) + ceilDiv(out, m.outPrice) +
		ceilDiv(cacheR, m.cacheR) + ceilDiv(cacheW, m.cacheW)
}
