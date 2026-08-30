package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestRedactSourceAtWrite 锁住写入收口：requests 表唯一的 INSERT 点
// （insertRequestTx）与 rollup 聚合必须自带清洗，任何调用方（被动入库、
// 执行器结算、判重合并）带来的上游凭据都到不了库、出不了维度接口。
// 背景见 usage.go RedactSource 注释：宿主 resolveUsageSource 在 auth 无
// 邮箱/账号信息时会把上游 api_key 原样填进 Source。
func TestRedactSourceAtWrite(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()

	// 形似凭据但旧启发式拦不住的形态（带分隔符）。
	leaked := "fk-a1b2c3d4e5f6g7h8i9j0"
	if RedactSource(leaked) != "" {
		t.Fatalf("测试前置失效：%q 应被 RedactSource 判定为凭据", leaked)
	}
	if err := s.RecordUsage(ctx, Request{
		ID:     "redact-write",
		TS:     time.UnixMilli(1_700_000_000_000).UTC(),
		Model:  "gpt-5",
		Source: leaked,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, err := s.GetRequest(ctx, "redact-write")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Source != "" {
		t.Fatalf("requests.source 写入后应已清空，得到 %q", got.Source)
	}
}
