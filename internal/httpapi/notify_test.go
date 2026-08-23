package httpapi

import (
	"strings"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func TestNotifyRoutes(t *testing.T) {
	a := newTestAPI(t)

	// 空配置：默认设置 + 空端点列表
	w := do(t, a, "GET", base+"/notify", "")
	var out struct {
		Settings  service.NotifySettings `json:"settings"`
		Endpoints []store.NotifyEndpoint `json:"endpoints"`
	}
	decodeJSON(t, w, &out)
	if out.Settings.Enabled || out.Settings.WarnPct != 20 {
		t.Fatalf("默认设置异常: %+v", out.Settings)
	}
	if out.Endpoints == nil || len(out.Endpoints) != 0 {
		t.Fatalf("端点应为空数组: %+v", out.Endpoints)
	}

	// 保存设置：非法阈值钳制
	w = do(t, a, "POST", base+"/notify/settings", `{"enabled":true,"warn_pct":999,"actor":"t"}`)
	var st service.NotifySettings
	decodeJSON(t, w, &st)
	if !st.Enabled || st.WarnPct != 95 {
		t.Fatalf("阈值应钳制到 95: %+v", st)
	}

	// 新增端点
	w = do(t, a, "POST", base+"/notify/endpoint/save", `{"label":"主通道","url":"logger://test","enabled":true,"actor":"t"}`)
	var saved struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, w, &saved)
	if saved.ID <= 0 {
		t.Fatalf("应返回新 id: %+v", saved)
	}

	// 列表回显解密 URL，且不含密文字段
	w = do(t, a, "GET", base+"/notify", "")
	decodeJSON(t, w, &out)
	if len(out.Endpoints) != 1 {
		t.Fatalf("端点数=%d", len(out.Endpoints))
	}
	ep := out.Endpoints[0]
	if ep.URL != "logger://test" || ep.Label != "主通道" || !ep.Enabled {
		t.Fatalf("端点回显异常: %+v", ep)
	}
	if w.Body.String() == "" || strings.Contains(w.Body.String(), "url_enc") {
		t.Fatalf("响应不应含密文字段: %s", w.Body)
	}

	// 更新端点：停用 + 改标签
	w = do(t, a, "POST", base+"/notify/endpoint/save",
		`{"id":1,"label":"备用通道","url":"logger://test2","enabled":false,"actor":"t"}`)
	decodeJSON(t, w, &saved)
	w = do(t, a, "GET", base+"/notify", "")
	decodeJSON(t, w, &out)
	if out.Endpoints[0].Label != "备用通道" || out.Endpoints[0].Enabled {
		t.Fatalf("更新未生效: %+v", out.Endpoints[0])
	}

	// 测试已存端点（logger:// 经真实 shoutrrr 发送，成功即 200）
	w = do(t, a, "POST", base+"/notify/endpoint/test", `{"id":1,"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("测试已存端点应 200: %d %s", w.Code, w.Body)
	}
	// 测试 draft 非法 URL → 400
	w = do(t, a, "POST", base+"/notify/endpoint/test", `{"url":"bogus-scheme://x","actor":"t"}`)
	if w.Code != 400 {
		t.Fatalf("非法 URL 应 400: %d", w.Code)
	}

	// 删除与幂等性
	w = do(t, a, "POST", base+"/notify/endpoint/delete", `{"id":1,"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("删除应 200: %d", w.Code)
	}
	w = do(t, a, "POST", base+"/notify/endpoint/delete", `{"id":1,"actor":"t"}`)
	if w.Code != 404 {
		t.Fatalf("重复删除应 404: %d", w.Code)
	}
}
