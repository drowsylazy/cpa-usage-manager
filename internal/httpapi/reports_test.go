package httpapi

import (
	"testing"
)

func TestReportRoutes(t *testing.T) {
	a := newTestAPI(t)

	// 前置：建一个通知端点供报告引用。
	w := do(t, a, "POST", base+"/notify/endpoint/save", `{"label":"通道","url":"logger://r","enabled":true,"actor":"t"}`)
	var ep struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, w, &ep)
	if ep.ID <= 0 {
		t.Fatalf("端点创建失败: %+v", ep)
	}

	// 空列表
	w = do(t, a, "GET", base+"/reports", "")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	decodeJSON(t, w, &list)
	if len(list.Items) != 0 {
		t.Fatalf("应为空: %+v", list.Items)
	}

	// 新增
	body := `{"name":"日报","enabled":true,"frequency":"daily","time_of_day":"09:00",` +
		`"weekday":1,"monthday":1,"tz_offset_min":480,` +
		`"sections":{"summary":true,"failures":true,"by_model":{"on":true,"top":5,"metric":"cost"}},` +
		`"endpoint_ids":[` + jsonNum(ep.ID) + `],"actor":"t"}`
	w = do(t, a, "POST", base+"/reports/save", body)
	var saved struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, w, &saved)
	if saved.ID <= 0 {
		t.Fatalf("应返回新 id")
	}

	// 回读
	w = do(t, a, "GET", base+"/reports", "")
	decodeJSON(t, w, &list)
	if len(list.Items) != 1 {
		t.Fatalf("items=%d", len(list.Items))
	}
	item := list.Items[0]
	if item["frequency"] != "daily" || item["tz_offset_min"] != float64(480) {
		t.Fatalf("回读异常: %+v", item)
	}
	if _, ok := item["sections"].(map[string]any); !ok {
		t.Fatalf("sections 应为对象: %+v", item["sections"])
	}

	// 校验：非法频率 / 缺端点 / 非法时刻
	w = do(t, a, "POST", base+"/reports/save",
		`{"name":"x","frequency":"hourly","time_of_day":"09:00","endpoint_ids":[1],"actor":"t"}`)
	if w.Code != 400 {
		t.Fatalf("非法频率应 400: %d", w.Code)
	}
	w = do(t, a, "POST", base+"/reports/save",
		`{"name":"x","frequency":"daily","time_of_day":"09:00","endpoint_ids":[],"actor":"t"}`)
	if w.Code != 400 {
		t.Fatalf("缺端点应 400: %d", w.Code)
	}
	w = do(t, a, "POST", base+"/reports/save",
		`{"name":"x","frequency":"daily","time_of_day":"99:00","endpoint_ids":[1],"actor":"t"}`)
	if w.Code != 400 {
		t.Fatalf("非法时刻应 400: %d", w.Code)
	}

	// 测试不存在的报告
	w = do(t, a, "POST", base+"/reports/test", `{"id":99999,"actor":"t"}`)
	if w.Code != 400 {
		t.Fatalf("测试不存在的报告应 400: %d", w.Code)
	}

	// 删除与幂等
	w = do(t, a, "POST", base+"/reports/delete", `{"id":`+jsonNum(saved.ID)+`,"actor":"t"}`)
	if w.Code != 200 {
		t.Fatalf("删除应 200: %d", w.Code)
	}
	w = do(t, a, "POST", base+"/reports/delete", `{"id":`+jsonNum(saved.ID)+`,"actor":"t"}`)
	if w.Code != 404 {
		t.Fatalf("重复删除应 404: %d", w.Code)
	}
}
