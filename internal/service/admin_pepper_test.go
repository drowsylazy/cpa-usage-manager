package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// TestRunAutoBackupPeppersSidecar 覆盖 include_peppers 的三条路径：
// 开启时侧车内容可被 LoadPeppers 解析且与当前 pepper 一致；
// 关闭时不写侧车；轮转删除旧 .bak 时同步删除其侧车。
func TestRunAutoBackupPeppersSidecar(t *testing.T) {
	ctx := context.Background()

	// 开启：侧车存在、0600、可被 LoadPeppers 解析回同一组 pepper。
	s, _ := testService(t)
	s.cfg.Backup.IncludePeppers = true
	dir := t.TempDir()
	path, err := s.RunAutoBackup(ctx, dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path + ".peppers")
	if err != nil {
		t.Fatalf("侧车应存在: %v", err)
	}
	if fi, err := os.Stat(path + ".peppers"); err != nil {
		t.Fatalf("侧车应可 stat: %v", err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != config.PepperFilePerm {
		// Windows 不呈现 POSIX 权限位，只读位之外的断言仅在类 Unix 下有效。
		t.Fatalf("侧车权限应为 %v: %v", config.PepperFilePerm, fi.Mode())
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("侧车应为 JSON 对象: %v", err)
	}
	for id, p := range s.peppers.Items {
		got, ok := m[id]
		if !ok {
			t.Fatalf("侧车缺少 pepper %q", id)
		}
		b, err := base64.StdEncoding.DecodeString(got)
		if err != nil || string(b) != string(p.Value) {
			t.Fatalf("pepper %q 内容不一致: %v", id, err)
		}
	}
	if len(m) != len(s.peppers.Items) {
		t.Fatalf("侧车 pepper 数不符: %d != %d", len(m), len(s.peppers.Items))
	}

	// 关闭：不写侧车。
	s2, _ := testService(t)
	dir2 := t.TempDir()
	path2, err := s2.RunAutoBackup(ctx, dir2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path2 + ".peppers"); !os.IsNotExist(err) {
		t.Fatalf("默认配置不应写侧车: %v", err)
	}

	// 轮转：被删 .bak 的侧车一并消失，保留份的侧车仍在。
	if _, err := s.RunAutoBackup(ctx, dir, 1); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var baks, peppers int
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".bak") {
			baks++
		}
		if strings.HasSuffix(name, ".bak.peppers") {
			peppers++
		}
	}
	if baks != 1 || peppers != 1 {
		t.Fatalf("轮转后应剩 1 份 .bak + 1 份侧车，得到 %d + %d", baks, peppers)
	}
}

// TestRestorePepperWarning 实锤恢复自检：备份来自 pepper 不同的环境时，
// Restore 必须在响应里当场报告不可解密密钥数；pepper 一致时不告警。
func TestRestorePepperWarning(t *testing.T) {
	ctx := context.Background()

	// 源实例：签发一个带密文的 Key，导出快照。
	src, _ := testService(t)
	quota := money.Micro(1000)
	issued, err := src.IssueKey(ctx, IssueRequest{CallerID: "default", QuotaMicroUSD: &quota, Actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var snap strings.Builder
	if _, err := src.Backup(ctx, &snap, "t"); err != nil {
		t.Fatal(err)
	}

	// 目标实例 A：pepper 不同（新建实例自动生成新 pepper）→ 必须告警。
	dst, _ := testService(t)
	res, err := dst.Restore(ctx, strings.NewReader(snap.String()), "t")
	if err != nil {
		t.Fatal(err)
	}
	if res.UndecryptableKeys != 1 || res.PepperWarning == "" {
		t.Fatalf("pepper 不同应告警: undecryptable=%d warning=%q", res.UndecryptableKeys, res.PepperWarning)
	}
	// 解不开也解不出明文：reveal 应失败。
	if _, err := dst.RevealKey(ctx, issued.KID, "t"); err == nil {
		t.Fatal("pepper 不同时 reveal 不应成功")
	}

	// 目标实例 B：空库 + 与源实例相同的 pepper 集 → 不告警且可解密。
	same := testServiceWithPeppers(t, src.cfg, src.peppers)
	res2, err := same.Restore(ctx, strings.NewReader(snap.String()), "t")
	if err != nil {
		t.Fatal(err)
	}
	if res2.UndecryptableKeys != 0 || res2.PepperWarning != "" {
		t.Fatalf("pepper 一致不应告警: undecryptable=%d warning=%q", res2.UndecryptableKeys, res2.PepperWarning)
	}
	plain, err := same.RevealKey(ctx, issued.KID, "t")
	if err != nil || plain != issued.Key {
		t.Fatalf("pepper 一致时 reveal 应成功: %v", err)
	}
}

// testServiceWithPeppers 与 testService 同构，但使用调用方给定的配置与
// pepper 集（pepper 不落盘——测试里密码材料在内存中传递即可）。
func testServiceWithPeppers(t *testing.T, c config.Config, ps PepperSet) *Service {
	t.Helper()
	ctx := context.Background()
	c = config.Default()
	c.DataDir = t.TempDir()
	c.DatabaseFile = "test.db"
	if err := c.EnsureDataDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(c.DataDir, c.DatabaseFile), OwnerID: "service-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, c, ps)
}
