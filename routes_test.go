//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/httpapi"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// TestManagementRegistrationCoversAllRoutes 断言宿主声明表与实际注册的路径一一对应。
//
// 回归点：v0.1.2 的 /pricing/search 在 httpapi 里注册了却漏了声明，宿主不转发，
// 面板搜索直接 404。这类缺陷不会让任何单包测试失败，只能靠跨层比对拦住。
func TestManagementRegistrationCoversAllRoutes(t *testing.T) {
	c := config.Default()
	c.DataDir = t.TempDir()
	c.DatabaseFile = "routes.db"
	st, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(c.DataDir, c.DatabaseFile), OwnerID: "routes"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ps, err := service.LoadPeppers(c, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(service.New(st, c, ps), st, "secret")

	reg := managementRegistration()
	const base = "/v0/management/plugins/cpa-usage-manager"

	declared := map[string]bool{}
	for _, r := range reg.Routes {
		if !strings.HasPrefix(r.Path, base) {
			t.Fatalf("声明的路径不在插件前缀下: %q", r.Path)
		}
		if strings.TrimSpace(r.Method) == "" || strings.TrimSpace(r.Description) == "" {
			t.Fatalf("声明缺少方法或说明: %+v", r)
		}
		declared[strings.TrimPrefix(r.Path, base)] = true
	}

	served := map[string]bool{}
	for _, p := range api.Paths() {
		served[p] = true
	}
	// 防止 Paths 退化为空表让本测试空转。
	if len(served) < 30 {
		t.Fatalf("注册路径数异常偏少（%d），Paths 可能失效", len(served))
	}

	var missing, extra []string
	for p := range served {
		if !declared[p] {
			missing = append(missing, p)
		}
	}
	for p := range declared {
		if !served[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("已注册但未向宿主声明（面板会 404）: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("已声明但没有对应处理器: %v", extra)
	}

	// 资源路由必须包含面板本体，否则宿主菜单里没有入口。
	hasConsole := false
	for _, r := range reg.Resources {
		if r.Path == "/console" {
			hasConsole = true
		}
	}
	if !hasConsole {
		t.Fatal("资源声明缺少 /console")
	}
}
