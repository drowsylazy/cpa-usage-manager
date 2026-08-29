package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunAutoBackupRotates(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	dir := t.TempDir()

	keep := 3
	for i := 0; i < 5; i++ {
		// 手工落旧文件模拟历史备份（时间戳命名，字典序=时间序）。
		if i < 2 {
			name := "cpa-usage-manager_2026010" + string(rune('1'+i)) + "-000000.bak"
			if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := s.RunAutoBackup(ctx, dir, keep); err != nil {
				t.Fatalf("第 %d 次备份: %v", i+1, err)
			}
			// 同一秒可能同名覆盖：穿插推进真实时间。
			time.Sleep(1100 * time.Millisecond)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var baks []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cpa-usage-manager_") && strings.HasSuffix(e.Name(), ".bak") {
			baks = append(baks, e.Name())
		}
	}
	if len(baks) != keep {
		t.Fatalf("应轮转保留 %d 份，得到 %d: %v", keep, len(baks), baks)
	}
	// 最新一份必须是合法快照：能被 RestoreFrom 接受（旧假文件已被轮转删除）。
	newest := filepath.Join(dir, baks[len(baks)-1])
	f, err := os.Open(newest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := s.Restore(ctx, f, "t"); err != nil {
		t.Fatalf("最新备份应可完整恢复: %v", err)
	}
}
