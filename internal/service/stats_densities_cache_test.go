package service

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestStatsCached 钉住库规模计数的 15s TTL 缓存：缓存生效后底层新增行不
// 应立即反映在返回值里（三个大表的 COUNT 各为一次全表扫描，overview 默认
// 页与 health 探针不该逐次付代价）。
func TestStatsCached(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	first, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO requests (id, ts) VALUES (?, ?)`,
			"stats-cached-1", time.Now().UTC().UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cached, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Requests != first.Requests {
		t.Fatalf("缓存窗口内计数不应变化: first=%d cached=%d", first.Requests, cached.Requests)
	}
}

// TestModelDensitiesCached 钉住密度面板读数的 30s TTL 缓存：缓存生效后新
// 增模型样本不立即出现；ResetModelDensity 写入新基线必须立即失效缓存——
// 重置后面板不能显示旧读数（该模型转「待学习」、新样本模型出现）。
func TestModelDensitiesCached(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	seq := 0
	seedSample := func(model string) {
		t.Helper()
		seq++
		err := st.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO requests (id, ts, model, result, body_len, context_tokens) VALUES (?, ?, ?, 'ok', 4096, 560)`,
				"dens-"+model+"-"+string(rune('0'+seq)), now, model)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// 密度样本 <3 条不构成有效估计（会落进盲区计数且盲区为 0，不出现行），
	// 每模型铺 3 条。
	for i := 0; i < 3; i++ {
		seedSample("m-a")
	}
	first, err := s.ModelDensities(ctx, 200)
	if err != nil || len(first) != 1 || first[0].Model != "m-a" {
		t.Fatalf("首次读数应含 1 个模型: items=%+v err=%v", first, err)
	}
	// 缓存窗口内新增一个有样本的模型：返回值不变。
	for i := 0; i < 3; i++ {
		seedSample("m-b")
	}
	cached, err := s.ModelDensities(ctx, 200)
	if err != nil || len(cached) != 1 || cached[0].Model != "m-a" {
		t.Fatalf("缓存窗口内读数不应变化: items=%+v err=%v", cached, err)
	}
	// 重置基线立即失效缓存：重算后 m-a 转「待学习」（ResetAt 非空），
	// m-b 以新样本出现在列表里。
	if err := s.ResetModelDensity(ctx, "m-a", "test"); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ModelDensities(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 2 {
		t.Fatalf("失效后应重算出 2 个模型，得到 %d: %+v", len(fresh), fresh)
	}
	var aRow, bRow bool
	for _, row := range fresh {
		switch row.Model {
		case "m-a":
			aRow = row.ResetAt != nil
		case "m-b":
			bRow = row.Samples == 3
		}
	}
	if !aRow || !bRow {
		t.Fatalf("重算读数异常（m-a 应待学习、m-b 应有样本）: %+v", fresh)
	}
}
