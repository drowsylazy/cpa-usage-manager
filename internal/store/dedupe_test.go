package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestModelMatchFragmentSQL 验证候选数为 1/2/3 时片段都是合法 SQL，
// 且既能精确命中又能命中「渠道/模型」别名。
//
// 回归点：早期实现把 IN 片段与 LIKE 片段用空格拼接，生成
// `model IN (?) model LIKE ?` 这种语法错误，任何判重查询都直接报错，
// 于是「找不到重复行」被误当成「没有重复」，同一请求被记两次。
func TestModelMatchFragmentSQL(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000).UTC()

	rows := []Request{
		{ID: "exact", TS: base, Model: "gpt-5"},
		{ID: "aliased", TS: base, Model: "azure/claude-sonnet-5"},
		{ID: "other", TS: base, Model: "gemini-3-pro"},
		{ID: "wildcard", TS: base, Model: "a_b"}, // 下划线是 LIKE 元字符，必须被转义
	}
	for _, r := range rows {
		if err := s.RecordUsage(ctx, r); err != nil {
			t.Fatalf("写入 %s 失败: %v", r.ID, err)
		}
	}

	cases := []struct {
		name   string
		models []string
		want   []string
	}{
		{"单候选精确", []string{"gpt-5"}, []string{"exact"}},
		{"双候选含别名后缀", []string{"claude-sonnet-5", "gpt-5"}, []string{"aliased", "exact"}},
		{"三候选", []string{"gpt-5", "claude-sonnet-5", "gemini-3-pro"}, []string{"aliased", "exact", "other"}},
		{"LIKE 元字符转义", []string{"a_b"}, []string{"wildcard"}},
		{"元字符不得匹配任意字符", []string{"axb"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frag, args := modelMatchFragment(tc.models)
			var got []string
			if err := s.Read(ctx, func(q Querier) error {
				rs, err := q.QueryContext(ctx,
					`SELECT id FROM requests WHERE `+frag+` ORDER BY id`, args...)
				if err != nil {
					return err
				}
				defer rs.Close()
				for rs.Next() {
					var id string
					if err := rs.Scan(&id); err != nil {
						return err
					}
					got = append(got, id)
				}
				return rs.Err()
			}); err != nil {
				t.Fatalf("片段 %q 不是合法 SQL: %v", frag, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("匹配结果 = %v, 期望 %v（片段 %s）", got, tc.want, frag)
			}
		})
	}
}

// rollupTotals 汇总全库分钟聚合，用于验证合并前后口径守恒。
type rollupTotals struct {
	rows, reqCount, totalTokens, inputTokens, ttftSum, ttftCount, cost int64
}

func readRollupTotals(t *testing.T, s *Store) rollupTotals {
	t.Helper()
	ctx := context.Background()
	var out rollupTotals
	if err := s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(req_count),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(input_tokens),0),
			COALESCE(SUM(ttft_sum),0), COALESCE(SUM(ttft_count),0),
			COALESCE(SUM(cost_micro_usd),0) FROM usage_rollups`).Scan(
			&out.rows, &out.reqCount, &out.totalTokens, &out.inputTokens,
			&out.ttftSum, &out.ttftCount, &out.cost)
	}); err != nil {
		t.Fatalf("读取聚合失败: %v", err)
	}
	return out
}

func countRequests(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	if err := s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&n)
	}); err != nil {
		t.Fatalf("统计请求行失败: %v", err)
	}
	return n
}

// seedDuplicatePair 写入一对「执行器行 + 宿主被动行」，模拟 v0.2.2 之前的双写。
// 执行器行有 key_id 与费用但缺 token 明细；被动行相反。
func seedDuplicatePair(t *testing.T, s *Store, suffix string, ts time.Time, execModel, hostModel string) (string, string) {
	t.Helper()
	ctx := context.Background()
	execID, hostID := "exec-"+suffix, "host-"+suffix
	exec := Request{
		ID: execID, TS: ts, KeyID: "kid1", CallerID: "default",
		Model: execModel, Result: ResultOK,
		LatencyMS: 9864, CostMicroUSD: 1234, Priced: true,
	}
	host := Request{
		ID: hostID, TS: ts.Add(400 * time.Millisecond), Model: hostModel,
		Provider: "openai", AuthType: "api_key", Tier: "default", Result: ResultOK,
		InputTokens: 700, OutputTokens: 300, TotalTokens: 1000,
		LatencyMS: 9871, TTFTMS: 887,
	}
	if err := s.RecordUsage(ctx, exec); err != nil {
		t.Fatalf("写入执行器行失败: %v", err)
	}
	if err := s.RecordUsage(ctx, host); err != nil {
		t.Fatalf("写入被动行失败: %v", err)
	}
	return execID, hostID
}

func TestDedupeRequestsSweep(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000).UTC()

	// 两对重复（其中一对是「渠道/模型」别名），外加一条无重复的孤立行。
	execA, hostA := seedDuplicatePair(t, s, "a", base, "gpt-5", "gpt-5")
	execB, hostB := seedDuplicatePair(t, s, "b", base.Add(time.Minute), "azure/gpt-5", "gpt-5")
	lone := Request{
		ID: "lone", TS: base.Add(2 * time.Minute), KeyID: "kid1", Model: "gpt-5",
		Result: ResultOK, LatencyMS: 100, InputTokens: 5, TotalTokens: 5,
	}
	if err := s.RecordUsage(ctx, lone); err != nil {
		t.Fatalf("写入孤立行失败: %v", err)
	}

	before := readRollupTotals(t, s)
	if got := countRequests(t, s); got != 5 {
		t.Fatalf("准备数据后应有 5 行请求，得到 %d", got)
	}
	if before.reqCount != 5 {
		t.Fatalf("合并前聚合请求数应为 5，得到 %d", before.reqCount)
	}

	merged, err := s.DedupeRequests(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DedupeRequests 失败: %v", err)
	}
	if merged != 2 {
		t.Fatalf("应合并 2 对，得到 %d", merged)
	}

	if got := countRequests(t, s); got != 3 {
		t.Fatalf("合并后应剩 3 行请求，得到 %d", got)
	}
	for _, id := range []string{hostA, hostB} {
		if _, err := s.GetRequest(ctx, id); err == nil {
			t.Fatalf("被动行 %s 应已被删除", id)
		}
	}
	for _, id := range []string{execA, execB} {
		r, err := s.GetRequest(ctx, id)
		if err != nil {
			t.Fatalf("执行器行 %s 应保留: %v", id, err)
		}
		if r.InputTokens != 700 || r.OutputTokens != 300 || r.TotalTokens != 1000 {
			t.Fatalf("%s 未合并 token 明细: %+v", id, r)
		}
		if r.TTFTMS != 887 {
			t.Fatalf("%s 未合并首字延迟: %d", id, r.TTFTMS)
		}
		if r.Provider != "openai" || r.AuthType != "api_key" || r.Tier != "default" {
			t.Fatalf("%s 未合并展示字段: %+v", id, r)
		}
		// 费用不参与合并：执行器行的结算金额必须原样保留。
		if r.CostMicroUSD != 1234 {
			t.Fatalf("%s 费用被改写: %d", id, r.CostMicroUSD)
		}
		if r.KeyID != "kid1" {
			t.Fatalf("%s 归属丢失: %+v", id, r)
		}
	}

	after := readRollupTotals(t, s)
	if after.reqCount != 3 {
		t.Fatalf("合并后聚合请求数应为 3，得到 %d", after.reqCount)
	}
	// token 与首字延迟总量守恒：被动行整行贡献被扣减，合并进执行器行的部分被回补。
	if after.totalTokens != before.totalTokens {
		t.Fatalf("总 token 不守恒: 合并前 %d, 合并后 %d", before.totalTokens, after.totalTokens)
	}
	if after.inputTokens != before.inputTokens {
		t.Fatalf("输入 token 不守恒: 合并前 %d, 合并后 %d", before.inputTokens, after.inputTokens)
	}
	if after.ttftSum != before.ttftSum || after.ttftCount != before.ttftCount {
		t.Fatalf("首字延迟不守恒: 前 sum=%d cnt=%d, 后 sum=%d cnt=%d",
			before.ttftSum, before.ttftCount, after.ttftSum, after.ttftCount)
	}
	if after.cost != before.cost {
		t.Fatalf("费用不守恒: 合并前 %d, 合并后 %d", before.cost, after.cost)
	}

	// 幂等：再扫一遍不应再有可合并对。
	again, err := s.DedupeRequests(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("二次 DedupeRequests 失败: %v", err)
	}
	if again != 0 {
		t.Fatalf("二次扫描应为 0，得到 %d", again)
	}
}

func TestDedupeRequestsRespectsSince(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000).UTC()
	seedDuplicatePair(t, s, "old", base, "gpt-5", "gpt-5")

	merged, err := s.DedupeRequests(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("DedupeRequests 失败: %v", err)
	}
	if merged != 0 {
		t.Fatalf("since 之前的行不应被合并，得到 %d", merged)
	}
	if got := countRequests(t, s); got != 2 {
		t.Fatalf("请求行数应不变，得到 %d", got)
	}
}

// TestRecordPassiveUsageMergesIntoExecutor 验证入库时防重：
// 执行器行已存在时，迟到的宿主回调在入库事务内被直接并入该行，
// 不产生任何时刻可见的重复行；聚合只增加被吸收的明细增量。
func TestRecordPassiveUsageMergesIntoExecutor(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000).UTC()

	exec := Request{
		ID: "exec-race", TS: base, KeyID: "kid1", CallerID: "default",
		Model: "azure/gpt-5", Result: ResultOK,
		LatencyMS: 9864, CostMicroUSD: 1234, Priced: true,
	}
	if err := s.RecordUsage(ctx, exec); err != nil {
		t.Fatalf("写入执行器行失败: %v", err)
	}
	before := readRollupTotals(t, s)

	host := Request{
		ID: "host-race", TS: base.Add(400 * time.Millisecond), Model: "gpt-5",
		Provider: "openai", AuthType: "api_key", Tier: "default", Result: ResultOK,
		InputTokens: 700, OutputTokens: 300, TotalTokens: 1000,
		LatencyMS: 9871, TTFTMS: 887,
	}
	hint := PassiveDedupeHint{Models: []string{"gpt-5"}, Near: host.TS, LatencyMS: host.LatencyMS}
	if err := s.RecordPassiveUsage(ctx, host, hint); err != nil {
		t.Fatalf("被动入库失败: %v", err)
	}

	if _, err := s.GetRequest(ctx, host.ID); err == nil {
		t.Fatal("被动行不应插入，应被合并进执行器行")
	}
	got, err := s.GetRequest(ctx, exec.ID)
	if err != nil {
		t.Fatalf("执行器行应保留: %v", err)
	}
	if got.InputTokens != 700 || got.OutputTokens != 300 || got.TotalTokens != 1000 {
		t.Fatalf("token 明细未合并: %+v", got)
	}
	if got.TTFTMS != 887 || got.Provider != "openai" || got.AuthType != "api_key" || got.Tier != "default" {
		t.Fatalf("展示字段未合并: %+v", got)
	}
	if got.CostMicroUSD != 1234 {
		t.Fatalf("费用被改写: %d", got.CostMicroUSD)
	}
	after := readRollupTotals(t, s)
	if after.reqCount != before.reqCount {
		t.Fatalf("聚合请求数不应变化: 前 %d 后 %d", before.reqCount, after.reqCount)
	}
	if after.totalTokens != before.totalTokens+1000 || after.inputTokens != before.inputTokens+700 {
		t.Fatalf("被吸收的明细应回补到聚合: 前 %+v 后 %+v", before, after)
	}
	if after.ttftSum != before.ttftSum+887 || after.ttftCount != before.ttftCount+1 {
		t.Fatalf("首字延迟应回补: 前 %+v 后 %+v", before, after)
	}
	if after.cost != before.cost {
		t.Fatalf("费用不应变化: 合并前 %d, 合并后 %d", before.cost, after.cost)
	}

	// 模型不匹配：按普通被动行落库。
	lone := Request{
		ID: "lone-race", TS: base.Add(time.Minute), CallerID: "default",
		Model: "other-model", Result: ResultOK, InputTokens: 3, TotalTokens: 3,
	}
	if err := s.RecordPassiveUsage(ctx, lone, PassiveDedupeHint{Models: []string{"other-model"}, Near: lone.TS, LatencyMS: 50}); err != nil {
		t.Fatalf("无重复被动入库失败: %v", err)
	}
	if _, err := s.GetRequest(ctx, lone.ID); err != nil {
		t.Fatalf("无重复时应正常插入: %v", err)
	}
}

// TestProbeMatchesHostFullChainLatency 钉住根因场景一：宿主 Latency 含全链路
// 开销（鉴权/翻译/重试，对照宿主源码核实的 reporter 语义），同一请求两侧
// 延迟差可达数秒。旧谓词按「延迟±150ms」配对在此漏判，重复行残留到事后
// 对账才被删掉——面板表现为请求数先涨后缩的波动。
func TestProbeMatchesHostFullChainLatency(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000).UTC()

	// 执行器行：startedAt=base，纯上游延迟 2.8s（host.model.execute 调用耗时）。
	exec := Request{
		ID: "exec-slow", TS: base, KeyID: "kid1", CallerID: "default",
		Model: "stepfun/step-3.7-flash", Result: ResultOK,
		LatencyMS: 2805, CostMicroUSD: 42, Priced: true,
	}
	if err := s.RecordUsage(ctx, exec); err != nil {
		t.Fatalf("写入执行器行失败: %v", err)
	}

	// 宿主回调：RequestedAt 晚 1.2s（宿主入口开销），Latency 9.9s（全链路），
	// 记账又晚了 4s（单 worker 队列积压）。旧谓词：延迟差 7.1s >> 150ms → 漏判。
	host := Request{
		ID: "host-late", TS: base.Add(1200 * time.Millisecond), Model: "step-3.7-flash",
		Provider: "openai", Result: ResultOK,
		InputTokens: 700, OutputTokens: 300, TotalTokens: 1000,
		LatencyMS: 9900,
	}
	hint := PassiveDedupeHint{Models: []string{"step-3.7-flash"}, Near: host.TS, LatencyMS: host.LatencyMS}
	if err := s.RecordPassiveUsage(ctx, host, hint); err != nil {
		t.Fatalf("被动入库失败: %v", err)
	}
	if _, err := s.GetRequest(ctx, host.ID); err == nil {
		t.Fatal("宿主全链路延迟的迟到回调应被并入执行器行，不得插入重复行")
	}
	got, err := s.GetRequest(ctx, exec.ID)
	if err != nil {
		t.Fatalf("执行器行应保留: %v", err)
	}
	if got.InputTokens != 700 || got.TotalTokens != 1000 {
		t.Fatalf("token 明细应合并: %+v", got)
	}
}

// TestProbeRejectsDifferentConcurrentRequests 钉住收紧后的误配防线：
// 同模型、时间相近、但属于不同请求的两行不得被判为重复。
func TestProbeRejectsDifferentConcurrentRequests(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000).UTC()

	// 两笔独立请求：请求 A（执行器），请求 B（另一用户经原生 Key 的被动行）。
	// B 的 ts 落在 A 的区间边缘、token 计数差异巨大——都是不同请求的特征。
	a := Request{
		ID: "exec-a", TS: base, KeyID: "kidA", CallerID: "default",
		Model: "gpt-5", Result: ResultOK,
		LatencyMS: 5000, InputTokens: 18000, TotalTokens: 19000, CostMicroUSD: 9, Priced: true,
	}
	if err := s.RecordUsage(ctx, a); err != nil {
		t.Fatalf("写入 A 失败: %v", err)
	}
	// B：ts 落在 A 完成时刻附近（区间包含成立、完成时刻也在容差内——宿主
	// 队列尾延迟下这是合法形态），但 token 计数与 A 差异巨大 → 靠 token
	// 相容性拒绝。同模型并发下时间维度本就不可分辨，token 计数才是
	// 区分不同请求的可靠信号。
	b := Request{
		ID: "host-b", TS: base.Add(9 * time.Second), Model: "gpt-5",
		Provider: "openai", Result: ResultOK,
		InputTokens: 120, TotalTokens: 150, LatencyMS: 4000,
	}
	hint := PassiveDedupeHint{Models: []string{"gpt-5"}, Near: b.TS, LatencyMS: b.LatencyMS,
		TotalTokens: b.TotalTokens, InputTokens: b.InputTokens}
	if err := s.RecordPassiveUsage(ctx, b, hint); err != nil {
		t.Fatalf("被动入库失败: %v", err)
	}
	if _, err := s.GetRequest(ctx, b.ID); err != nil {
		t.Fatalf("不同请求的被动行不得被误并: %v", err)
	}
	if _, err := s.GetRequest(ctx, a.ID); err != nil {
		t.Fatalf("执行器行应保留: %v", err)
	}
}

// TestDedupeRequestsFullChainLatencyPair 事后对账同口径：宿主全链路延迟的
// 历史重复对应被合并；token 计数不相容（差异超容差）的行不得合并。
func TestDedupeRequestsFullChainLatencyPair(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000).UTC()

	// 真重复：宿主延迟远大于执行器延迟（全链路开销），token 相同。
	exec := Request{
		ID: "exec-h1", TS: base, KeyID: "kid1", CallerID: "default",
		Model: "gpt-5", Result: ResultOK, LatencyMS: 2805, CostMicroUSD: 12, Priced: true,
	}
	host := Request{
		ID: "host-h1", TS: base.Add(900 * time.Millisecond), Model: "gpt-5",
		Provider: "openai", Result: ResultOK,
		InputTokens: 500, OutputTokens: 200, TotalTokens: 700, LatencyMS: 8800,
	}
	// 不合并：token 差异巨大（不同请求）。
	exec2 := Request{
		ID: "exec-h2", TS: base.Add(time.Minute), KeyID: "kid1", CallerID: "default",
		Model: "claude-5", Result: ResultOK, LatencyMS: 3000, InputTokens: 900, TotalTokens: 950, CostMicroUSD: 3, Priced: true,
	}
	host2 := Request{
		ID: "host-h2", TS: base.Add(time.Minute + 500*time.Millisecond), Model: "claude-5",
		Provider: "anthropic", Result: ResultOK,
		InputTokens: 12000, TotalTokens: 13000, LatencyMS: 9500,
	}
	for _, r := range []Request{exec, host, exec2, host2} {
		if err := s.RecordUsage(ctx, r); err != nil {
			t.Fatalf("写入 %s 失败: %v", r.ID, err)
		}
	}

	merged, err := s.DedupeRequests(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("DedupeRequests 失败: %v", err)
	}
	if merged != 1 {
		t.Fatalf("应只合并全链路延迟的真重复对（token 不相容的不得合并），得到 %d", merged)
	}
	if _, err := s.GetRequest(ctx, host.ID); err == nil {
		t.Fatal("真重复的被动行应被删除")
	}
	if _, err := s.GetRequest(ctx, host2.ID); err != nil {
		t.Fatalf("token 不相容的被动行应保留: %v", err)
	}
}
