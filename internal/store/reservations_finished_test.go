package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReservationFinishedAtReads 钉住 v19 finished_at 派生列的两个消费面：
// 「最近预占」按完结时刻倒序、held 行不出现在已完结列表；「预占精度」只吃
// 已结算且两侧 token 为正的样本。EXPLAIN QUERY PLAN 同时钉住两个读数都走
// idx_reservations_finished——这是 v19 的存在意义，索引一旦退化回全表扫描
// （如查询被改成表达式排序）测试必须先红。
func TestReservationFinishedAtReads(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := s.InsertKey(ctx, InsertKeyParams{
		KID: "kidfin1", KeyHash: []byte("h"), EncryptedMaterial: []byte("e"), PepperID: "p1",
		CallerScope: CallerScopeCaller, CallerID: DefaultCallerID,
	}); err != nil {
		t.Fatal(err)
	}
	seed := func(id string, settle bool, at time.Time) {
		t.Helper()
		if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
			ID: id, KeyID: "kidfin1", Model: "m-" + id,
			HeldMicroUSD: 700, ReservedTokens: 300,
			Now: now.Add(-10 * time.Minute), ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if !settle {
			if _, err := s.ReleaseReservation(ctx, id, at); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := s.SettleReservation(ctx, id, 120, 260, at, nil); err != nil {
			t.Fatal(err)
		}
	}
	// 完结顺序：r1 结算最早、r3 其次、r2 释放最晚；r4 保持 held。
	seed("fin-r1", true, now.Add(-3*time.Minute))
	seed("fin-r2", false, now.Add(-time.Minute))
	seed("fin-r3", true, now.Add(-2*time.Minute))
	if _, _, err := s.HoldReservation(ctx, HoldReservationParams{
		ID: "fin-r4", KeyID: "kidfin1", Model: "m-fin-r4",
		HeldMicroUSD: 700, ReservedTokens: 300,
		Now: now.Add(-10 * time.Minute), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	recent, err := s.ListRecentReservations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"fin-r2", "fin-r3", "fin-r1"}
	if len(recent) != len(wantOrder) {
		t.Fatalf("已完结应为 %d 条（held 不出现），得到 %d: %+v", len(wantOrder), len(recent), recent)
	}
	for i, id := range wantOrder {
		if recent[i].ID != id {
			t.Fatalf("第 %d 条应为 %s，得到 %s（完整: %+v）", i, id, recent[i].ID, recent)
		}
	}
	if recent[0].Status != "released" || recent[0].SettledTokens != 0 {
		t.Fatalf("released 行口径异常: %+v", recent[0])
	}
	if recent[1].SettledTokens != 260 {
		t.Fatalf("结算 token 未落库: %+v", recent[1])
	}

	acc, err := s.ReservationAccuracy(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(acc) != 2 {
		t.Fatalf("精度样本只含已结算模型（2 个），得到 %d: %+v", len(acc), acc)
	}
	for _, row := range acc {
		if row.Samples != 1 || row.P50RatioMilli != 867 { // 260/300 = 0.8667 → 867
			t.Fatalf("精度读数异常: %+v", row)
		}
	}

	for name, sql := range map[string]string{
		"recent": `SELECT id FROM reservations
		 WHERE status IN ('settled','released')
		 ORDER BY finished_at DESC LIMIT 25`,
		"accuracy": `SELECT model FROM reservations
		 WHERE status = 'settled' AND reserved_tokens > 0 AND settled_tokens > 0
		 ORDER BY finished_at DESC LIMIT 20000`,
	} {
		var plan string
		if err := s.Read(ctx, func(q Querier) error {
			rows, err := q.QueryContext(ctx, `EXPLAIN QUERY PLAN `+sql)
			if err != nil {
				return err
			}
			defer rows.Close()
			var parts []string
			for rows.Next() {
				var a, b, c string
				var detail string
				if err := rows.Scan(&a, &b, &c, &detail); err != nil {
					return err
				}
				parts = append(parts, detail)
			}
			plan = strings.Join(parts, " | ")
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, "idx_reservations_finished") {
			t.Fatalf("%s 查询未走 idx_reservations_finished，计划: %s", name, plan)
		}
	}
}
