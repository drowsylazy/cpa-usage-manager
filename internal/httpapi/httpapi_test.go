package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

func TestManagementAuthConsoleAndNoStore(t *testing.T) {
	c := config.Default()
	c.DataDir = t.TempDir()
	c.DatabaseFile = "api.db"
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(c.DataDir, c.DatabaseFile), OwnerID: "api"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps, err := service.LoadPeppers(c, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, c, ps)
	api := New(svc, st, Options{ManagementKey: "secret", CompressionEnabled: true})
	r := httptest.NewRequest("GET", "/v0/management/plugins/cpa-usage-manager/health", nil)
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("未鉴权状态码=%d", w.Code)
	}
	r = httptest.NewRequest("GET", "/v0/management/plugins/cpa-usage-manager/health", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("health=%d %s", w.Code, w.Body)
	}
	r = httptest.NewRequest("POST", "/v0/management/plugins/cpa-usage-manager/keys/issue", strings.NewReader(`{"caller_id":"default"}`))
	r.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("issue=%d headers=%v", w.Code, w.Header())
	}
	var out map[string]any
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("响应不是 JSON")
	}
	r = httptest.NewRequest("GET", "/console", nil)
	w = httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "CPA 用量管理") {
		t.Fatalf("console=%d", w.Code)
	}
}
