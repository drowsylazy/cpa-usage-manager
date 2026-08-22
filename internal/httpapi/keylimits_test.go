package httpapi

import (
	"strings"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// 限额更新的入参口径回归。
//
// 两个在 v0.3.4 之前一直存在的缺陷：
//  1. 金额限额编辑从未成功过 —— parseKeyUpdate 用 money.ParseUSD(jsonStr(v))
//     把 *_micro_usd 当美元字符串解析，而签发路径与面板都发裸整数 micro-USD。
//     jsonStr 对 JSON 数字返回空串，ParseUSD 随即报「金额格式非法」→ 400。
//  2. token 限额同理（新字段，一并按裸数字解）。
//
// 这两条锁住「签发与更新对同一字段名使用同一口径」，否则 quota_micro_usd
// 在两条路径上含义会差 1e6 倍。

func issueTestKey(t *testing.T, a *API, body string) string {
	t.Helper()
	w := do(t, a, "POST", base+"/keys/issue", body)
	if w.Code != 200 {
		t.Fatalf("issue 失败 code=%d body=%s", w.Code, w.Body.String())
	}
	var issued struct {
		KID string `json:"kid"`
	}
	decodeJSON(t, w, &issued)
	if issued.KID == "" {
		t.Fatal("issue 未返回 kid")
	}
	return issued.KID
}

func TestKeyUpdateAcceptsBareNumericLimits(t *testing.T) {
	a := newTestAPI(t)
	kid := issueTestKey(t, a, `{"label":"lim","caller_id":"default","actor":"t"}`)

	// 金额与 token 限额都以裸 JSON 数字提交（面板的实际发送形式）。
	w := do(t, a, "POST", base+"/keys/update",
		`{"kid":"`+kid+`","quota_micro_usd":7000000,"daily_micro_usd":2500000,`+
			`"token_limit":1500000,"daily_token_limit":250000,"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("裸数字限额应被接受，得到 code=%d body=%s", w.Code, w.Body.String())
	}
	var k store.PluginKey
	decodeJSON(t, w, &k)
	if k.QuotaMicroUSD == nil || *k.QuotaMicroUSD != 7_000_000 {
		t.Errorf("quota_micro_usd 应为 7000000 micro-USD，得到 %v", k.QuotaMicroUSD)
	}
	if k.DailyMicroUSD == nil || *k.DailyMicroUSD != 2_500_000 {
		t.Errorf("daily_micro_usd 应为 2500000，得到 %v", k.DailyMicroUSD)
	}
	if k.TokenLimit == nil || *k.TokenLimit != 1_500_000 {
		t.Errorf("token_limit 应为 1500000，得到 %v", k.TokenLimit)
	}
	if k.DailyTokenLimit == nil || *k.DailyTokenLimit != 250_000 {
		t.Errorf("daily_token_limit 应为 250000，得到 %v", k.DailyTokenLimit)
	}
}

func TestKeyUpdateAcceptsQuotedNumericLimits(t *testing.T) {
	a := newTestAPI(t)
	kid := issueTestKey(t, a, `{"label":"lim2","caller_id":"default","actor":"t"}`)

	// 带引号的数字也应接受（历史调用方可能这么发）。
	w := do(t, a, "POST", base+"/keys/update",
		`{"kid":"`+kid+`","quota_micro_usd":"3000000","token_limit":"900000","actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("字符串数字应被接受，得到 code=%d body=%s", w.Code, w.Body.String())
	}
	var k store.PluginKey
	decodeJSON(t, w, &k)
	if k.QuotaMicroUSD == nil || *k.QuotaMicroUSD != 3_000_000 {
		t.Errorf("quota 应为 3000000，得到 %v", k.QuotaMicroUSD)
	}
	if k.TokenLimit == nil || *k.TokenLimit != 900_000 {
		t.Errorf("token_limit 应为 900000，得到 %v", k.TokenLimit)
	}
}

func TestKeyUpdateNullClearsTokenLimits(t *testing.T) {
	a := newTestAPI(t)
	kid := issueTestKey(t, a,
		`{"label":"lim3","caller_id":"default","token_limit":1000000,"daily_token_limit":50000,"actor":"t"}`)

	// null 表示清空该限制（改为不限）；缺省的字段保持不变。
	w := do(t, a, "POST", base+"/keys/update",
		`{"kid":"`+kid+`","token_limit":null,"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var k store.PluginKey
	decodeJSON(t, w, &k)
	if k.TokenLimit != nil {
		t.Errorf("token_limit 应被清空，得到 %v", *k.TokenLimit)
	}
	if k.DailyTokenLimit == nil || *k.DailyTokenLimit != 50_000 {
		t.Errorf("未提及的 daily_token_limit 应保持不变，得到 %v", k.DailyTokenLimit)
	}
}

func TestKeyUpdateRejectsNegativeAndGarbageLimits(t *testing.T) {
	a := newTestAPI(t)
	kid := issueTestKey(t, a, `{"label":"lim4","caller_id":"default","actor":"t"}`)

	for _, tc := range []struct{ name, body, wantIn string }{
		{"负 token", `{"kid":"` + kid + `","token_limit":-1,"actor":"t"}`, "不能为负"},
		{"负金额", `{"kid":"` + kid + `","quota_micro_usd":-5,"actor":"t"}`, "不能为负"},
		{"非数字 token", `{"kid":"` + kid + `","token_limit":"abc","actor":"t"}`, "整数"},
		{"非数字金额", `{"kid":"` + kid + `","quota_micro_usd":"abc","actor":"t"}`, "整数"},
	} {
		w := do(t, a, "POST", base+"/keys/update", tc.body)
		if w.Code != 400 {
			t.Errorf("%s 应被拒（400），得到 %d body=%s", tc.name, w.Code, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), tc.wantIn) {
			t.Errorf("%s 的错误信息应含 %q，得到 %s", tc.name, tc.wantIn, w.Body.String())
		}
	}
}

func TestBalanceExposesTokenRemaining(t *testing.T) {
	a := newTestAPI(t)
	kid := issueTestKey(t, a,
		`{"label":"bal","caller_id":"default","quota_micro_usd":1000000,`+
			`"token_limit":800000,"daily_token_limit":200000,"actor":"t"}`)

	w := do(t, a, "GET", base+"/balance?key_id="+kid, "")
	if w.Code != 200 {
		t.Fatalf("balance code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// 面板按这些 JSON 名读取余量。字段改名会让表盘静默显示「—」，
	// 这条把契约钉住。
	for _, want := range []string{
		"total_remaining_micro_usd", "total_remaining_tokens",
		"daily_remaining_tokens", "held_tokens",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("balance 响应缺少 %q：%s", want, body)
		}
	}
}
