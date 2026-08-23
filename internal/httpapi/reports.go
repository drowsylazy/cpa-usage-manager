package httpapi

import (
	"net/http"

	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// GET /reports：全部报告配置。
func (a *API) reports(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	list, err := a.svc.ListReports(r.Context())
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 500)
		return
	}
	if list == nil {
		list = []store.ReportConfig{}
	}
	jsonOut(w, map[string]any{"items": list}, 200)
}

// POST /reports/save：新增（id 缺省/0）或更新报告配置。
func (a *API) reportSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.ReportConfig
		Actor string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	id, err := a.svc.SaveReport(r.Context(), in.ReportConfig, in.Actor)
	if err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 400)
		return
	}
	jsonOut(w, map[string]int64{"id": id}, 200)
}

// POST /reports/delete。
func (a *API) reportDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    int64  `json:"id"`
		Actor string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if in.ID <= 0 {
		jsonOut(w, map[string]string{"error": "缺少 id"}, 400)
		return
	}
	if err := a.svc.DeleteReport(r.Context(), in.ID, in.Actor); err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 404)
		return
	}
	jsonOut(w, map[string]bool{"ok": true}, 200)
}

// POST /reports/test：按最近一个已完成周期立即发送，不推进 last_period。
func (a *API) reportTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    int64  `json:"id"`
		Actor string `json:"actor"`
	}
	if e := decode(r, &in); e != nil {
		jsonOut(w, map[string]string{"error": e.Error()}, 400)
		return
	}
	if in.ID <= 0 {
		jsonOut(w, map[string]string{"error": "缺少 id"}, 400)
		return
	}
	if err := a.svc.TestReport(r.Context(), in.ID, in.Actor); err != nil {
		jsonOut(w, map[string]string{"error": err.Error()}, 400)
		return
	}
	jsonOut(w, map[string]bool{"ok": true}, 200)
}
