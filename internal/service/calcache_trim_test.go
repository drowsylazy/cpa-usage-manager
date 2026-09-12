package service

import (
	"testing"
	"time"
)

// TestTrimCalCache 钉住校准缓存的定容契约：未超容不动；超容先摘过期条目；
// 摘完仍超则按写入时刻淘汰最旧的一半——TTL 内的活跃模型必须保留，
// 否则热路径预占会在每次写入后丢学习结果、反复回退保守口径。
func TestTrimCalCache(t *testing.T) {
	now := time.Now()
	at := func(e outputCalEntry) time.Time { return e.at }
	mk := func(n int, age time.Duration) map[string]outputCalEntry {
		m := make(map[string]outputCalEntry, n)
		for i := 0; i < n; i++ {
			m[string(rune('a'+i%26))+string(rune('0'+i/26))] = outputCalEntry{p95: int64(i), at: now.Add(-age)}
		}
		return m
	}

	// 未超容：原样返回。
	small := mk(calCacheMaxEntries-1, 0)
	if got := trimCalCache(small, at, now, outputCalTTL, calCacheMaxEntries); len(got) != calCacheMaxEntries-1 {
		t.Fatalf("未超容不应裁剪: %d", len(got))
	}

	// 超容且全部过期：清空。
	stale := mk(calCacheMaxEntries+10, 2*outputCalTTL)
	if got := trimCalCache(stale, at, now, outputCalTTL, calCacheMaxEntries); len(got) != 0 {
		t.Fatalf("过期条目应全部摘除: %d", len(got))
	}

	// 超容、一半过期一半新鲜：只留新鲜的一半。
	mixed := mk(calCacheMaxEntries, 0)
	fresh := 0
	for k, e := range mixed {
		if k[0] < 'm' { // 一半标记为过期
			e.at = now.Add(-2 * outputCalTTL)
			mixed[k] = e
		} else {
			fresh++
		}
	}
	got := trimCalCache(mixed, at, now, outputCalTTL, calCacheMaxEntries)
	if len(got) != fresh {
		t.Fatalf("过期摘除后应只剩新鲜条目 %d，得到 %d", fresh, len(got))
	}

	// 超容且全部新鲜：按 at 淘汰最旧的一半。k[0] 越大写入越早（age 越
	// 大），所以最旧的必然是 z 系、最新的必然是 a 系。
	all := mk(calCacheMaxEntries, 0)
	for k, e := range all {
		e.at = now.Add(-time.Duration(k[0]) * time.Millisecond) // a..z 写入时刻递减（z 最旧）
		all[k] = e
	}
	got = trimCalCache(all, at, now, outputCalTTL, calCacheMaxEntries)
	if len(got) != calCacheMaxEntries/2 {
		t.Fatalf("仍超容应淘汰一半: %d", len(got))
	}
	if _, ok := got["a0"]; !ok {
		t.Fatalf("最新的条目不应被淘汰")
	}
	if _, ok := got["z0"]; ok {
		t.Fatalf("最旧的条目应被淘汰")
	}
}
