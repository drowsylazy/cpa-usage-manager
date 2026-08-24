package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func jsonNum(v int64) string { return strconv.FormatInt(v, 10) }
func timeNow() time.Time     { return time.Now().UTC() }

func newTestAPI(t *testing.T) *API {
	t.Helper()
	c := config.Default()
	c.DataDir = t.TempDir()
	c.DatabaseFile = "api.db"
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(c.DataDir, c.DatabaseFile), OwnerID: "api"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ps, err := service.LoadPeppers(c, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return New(service.New(st, c, ps), st, Options{ManagementKey: "secret", CompressionEnabled: true})
}

func do(t *testing.T, a *API, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("HTTP %d body=%s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("JSON 解析失败: %v body=%s", err, w.Body)
	}
}

func TestCallersCRUD(t *testing.T) {
	a := newTestAPI(t)
	w := do(t, a, "POST", base+"/callers", `{"id":"team-a","display_name":"A 组","actor":"t"}`)
	var c store.Caller
	decodeJSON(t, w, &c)
	if c.ID != "team-a" || !c.Enabled {
		t.Fatalf("caller=%+v", c)
	}
	w = do(t, a, "GET", base+"/callers", "")
	var list struct {
		Items []store.Caller `json:"items"`
	}
	decodeJSON(t, w, &list)
	found := false
	for _, it := range list.Items {
		if it.ID == "team-a" && it.DisplayName == "A 组" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未找到 team-a: %+v", list.Items)
	}
	w = do(t, a, "POST", base+"/callers/enabled", `{"id":"team-a","enabled":false,"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("enable=%d", w.Code)
	}
	w = do(t, a, "GET", base+"/callers", "")
	decodeJSON(t, w, &list)
	for _, it := range list.Items {
		if it.ID == "team-a" && it.Enabled {
			t.Fatal("team-a 应已停用")
		}
	}
	w = do(t, a, "POST", base+"/callers", `{"id":"","display_name":"","actor":"t"}`)
	if w.Code != 400 {
		t.Fatalf("非法 caller 应 400，得到 %d", w.Code)
	}
}

func TestKeyLifecycleRoutes(t *testing.T) {
	a := newTestAPI(t)
	w := do(t, a, "POST", base+"/keys/issue", `{"label":"test","caller_id":"default","quota_micro_usd":1000000,"actor":"t"}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("issue=%d headers=%v", w.Code, w.Header())
	}
	var issued struct {
		Key string `json:"key"`
		KID string `json:"kid"`
	}
	decodeJSON(t, w, &issued)
	if !strings.HasPrefix(issued.Key, "cum-") || issued.KID == "" {
		t.Fatalf("issued=%+v", issued)
	}

	w = do(t, a, "POST", base+"/keys/update", `{"kid":"`+issued.KID+`","label":"updated","enabled":false,"actor":"t"}`)
	var k store.PluginKey
	decodeJSON(t, w, &k)
	if k.Label != "updated" || k.Enabled {
		t.Fatalf("updated=%+v", k)
	}

	w = do(t, a, "POST", base+"/keys/update", `{"kid":"`+issued.KID+`","quota_micro_usd":null,"actor":"t"}`)
	var cleared store.PluginKey
	decodeJSON(t, w, &cleared)
	if cleared.QuotaMicroUSD != nil {
		t.Fatalf("quota 应被清空: %+v", cleared.QuotaMicroUSD)
	}

	w = do(t, a, "GET", base+"/balance?key_id="+issued.KID, "")
	if w.Code != 200 {
		t.Fatalf("balance=%d %s", w.Code, w.Body)
	}

	w = do(t, a, "POST", base+"/keys/rotate", `{"kid":"`+issued.KID+`","actor":"t"}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate=%d", w.Code)
	}
	var rotated struct {
		Key string `json:"key"`
	}
	decodeJSON(t, w, &rotated)
	if rotated.Key == issued.Key {
		t.Fatal("轮换后 Key 不应相同")
	}

	w = do(t, a, "POST", base+"/keys/reveal", `{"kid":"`+issued.KID+`","actor":"t"}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal=%d", w.Code)
	}
	var revealed map[string]string
	decodeJSON(t, w, &revealed)
	if revealed["key"] != rotated.Key {
		t.Fatalf("reveal 应返回最新密文: got=%q want=%q", revealed["key"], rotated.Key)
	}

	w = do(t, a, "POST", base+"/keys/revoke", `{"kid":"`+issued.KID+`","actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("revoke=%d", w.Code)
	}
	w = do(t, a, "POST", base+"/keys/delete", `{"kid":"`+issued.KID+`","actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("delete=%d", w.Code)
	}
	w = do(t, a, "POST", base+"/keys/delete", `{"kid":"`+issued.KID+`","actor":"t"}`)
	if w.Code != 400 {
		t.Fatalf("重复删除应 400，得到 %d", w.Code)
	}
}

func TestPricingAndUsageRoutes(t *testing.T) {
	a := newTestAPI(t)
	w := do(t, a, "GET", base+"/pricing", "")
	if w.Code != 200 {
		t.Fatalf("pricing=%d", w.Code)
	}
	w = do(t, a, "POST", base+"/pricing", `{"match_kind":"exact","pattern":"gpt-4o","priority":10,"price_input":1000000,"price_output":3000000,"actor":"t"}`)
	var rule store.PricingRule
	decodeJSON(t, w, &rule)
	if rule.ID == 0 || rule.MatchKind != "exact" {
		t.Fatalf("rule=%+v", rule)
	}
	w = do(t, a, "POST", base+"/pricing/delete", `{"id":`+jsonNum(rule.ID)+`,"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("pricing/delete=%d", w.Code)
	}

	// 记录一条 usage 后检查汇总/成本/趋势。
	w = do(t, a, "POST", base+"/keys/issue", `{"actor":"t"}`)
	var issued struct {
		KID string `json:"kid"`
	}
	decodeJSON(t, w, &issued)
	st := a.st
	req := store.Request{
		ID: "req-1", KeyID: issued.KID, CallerID: "default", Model: "gpt-4o",
		Provider: "openai", Source: "api", Result: "ok", TS: timeNow(),
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		CostMicroUSD: 5000,
	}
	if err := st.RecordUsage(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	w = do(t, a, "GET", base+"/usage/summary", "")
	var sum struct {
		Overall struct {
			Requests int64 `json:"requests"`
		} `json:"overall"`
	}
	decodeJSON(t, w, &sum)
	if sum.Overall.Requests != 1 {
		t.Fatalf("summary=%+v", sum)
	}
	w = do(t, a, "GET", base+"/costs", "")
	if w.Code != 200 {
		t.Fatalf("costs=%d", w.Code)
	}
	w = do(t, a, "GET", base+"/trends?grain=day", "")
	if w.Code != 200 {
		t.Fatalf("trends=%d %s", w.Code, w.Body)
	}
	w = do(t, a, "GET", base+"/usage?limit=10", "")
	if w.Code != 200 {
		t.Fatalf("usage=%d", w.Code)
	}
	w = do(t, a, "GET", base+"/requests?limit=10", "")
	if w.Code != 200 {
		t.Fatalf("requests=%d", w.Code)
	}
	w = do(t, a, "GET", base+"/audit?limit=50", "")
	if w.Code != 200 {
		t.Fatalf("audit=%d", w.Code)
	}
	w = do(t, a, "GET", base+"/auth-quotas", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("auth-quotas=%d headers=%v", w.Code, w.Header())
	}
}

func TestPreferencesExchangeAndExports(t *testing.T) {
	a := newTestAPI(t)
	w := do(t, a, "POST", base+"/preferences", `{"lang":"zh-CN","unit":"k"}`)
	if w.Code != 200 {
		t.Fatalf("pref set=%d", w.Code)
	}
	w = do(t, a, "GET", base+"/preferences", "")
	var pref map[string]string
	decodeJSON(t, w, &pref)
	if pref["lang"] != "zh-CN" {
		t.Fatalf("pref=%v", pref)
	}
	w = do(t, a, "GET", base+"/exchange-rate", "")
	var rate struct {
		USDToCNYMicro json.Number `json:"usd_to_cny_micro"`
	}
	decodeJSON(t, w, &rate)
	if rate.USDToCNYMicro.String() == "" {
		t.Fatal("缺汇率")
	}

	w = do(t, a, "POST", base+"/export/csv", `{"kind":"requests","limit":10}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("export/csv=%d headers=%v", w.Code, w.Header())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "csv") {
		t.Fatalf("csv Content-Type=%q", ct)
	}

	w = do(t, a, "POST", base+"/export/png", `{"kind":"trends","metric":"cost","grain":"day"}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("export/png=%d headers=%v", w.Code, w.Header())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "png") {
		t.Fatalf("png Content-Type=%q", ct)
	}
}

func TestBackupRestoreResetMaintain(t *testing.T) {
	a := newTestAPI(t)
	// 先造点数据。
	do(t, a, "POST", base+"/keys/issue", `{"actor":"t"}`)

	w := do(t, a, "GET", base+"/backup", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("backup=%d headers=%v", w.Code, w.Header())
	}
	if w.Body.Len() == 0 {
		t.Fatal("备份为空")
	}

	w = do(t, a, "POST", base+"/restore", "not-a-db")
	if w.Code != 400 {
		t.Fatalf("restore 无确认头应 400，得到 %d", w.Code)
	}
	// 有确认头但内容非法也应失败，但绝不返回 2xx。
	rd := bytes.NewReader([]byte("junk"))
	r := httptest.NewRequest("POST", base+"/restore", rd)
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("X-Confirm-Restore", "replace")
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("非法恢复不应成功")
	}

	w = do(t, a, "POST", base+"/reset", `{"confirm":"no"}`)
	if w.Code != 400 {
		t.Fatalf("reset 无确认应 400，得到 %d", w.Code)
	}
	w = do(t, a, "POST", base+"/reset", `{"confirm":"reset","actor":"t"}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reset=%d headers=%v", w.Code, w.Header())
	}
	w = do(t, a, "POST", base+"/maintain", `{"vacuum":false,"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("maintain=%d", w.Code)
	}
	w = do(t, a, "POST", base+"/dedupe", `{"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("dedupe=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"merged"`) {
		t.Fatalf("dedupe 响应缺少 merged: %s", w.Body.String())
	}
}
