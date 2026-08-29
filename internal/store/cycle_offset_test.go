package store

import (
	"testing"
	"time"
)

func TestCycleOffset(t *testing.T) {
	// 无偏移：纯 UTC 行为。
	SetCycleOffsetMinutes(0)
	t.Cleanup(func() { SetCycleOffsetMinutes(0) })
	ts := time.Date(2026, 8, 29, 7, 30, 0, 0, time.UTC)
	cy := CyclesFor(ts)
	if cy.Daily != "2026-08-29" || cy.Monthly != "2026-08" {
		t.Fatalf("零偏移周期不符: %+v", cy)
	}

	// UTC+8：07:30 UTC 本地是 15:30，日键不变；16:00 UTC 本地跨日。
	SetCycleOffsetMinutes(480)
	if got := CyclesFor(ts).Daily; got != "2026-08-29" {
		t.Fatalf("UTC+8 07:30 应仍属 08-29: %s", got)
	}
	next := time.Date(2026, 8, 29, 15, 30, 0, 0, time.UTC) // 本地 23:30
	if got := CyclesFor(next).Daily; got != "2026-08-29" {
		t.Fatalf("UTC+8 23:30 应属 08-29: %s", got)
	}
	cross := next.Add(31 * time.Minute) // 本地 00:01，跨入 08-30
	cy = CyclesFor(cross)
	if cy.Daily != "2026-08-30" {
		t.Fatalf("UTC+8 本地零点后应归 08-30: %s", cy.Daily)
	}
	if cy.Monthly != "2026-08" {
		t.Fatalf("月键不符: %s", cy.Monthly)
	}

	// 跨月边界：本地 9-01 00:01（UTC 08-31 16:01）应归 9 月。
	SetCycleOffsetMinutes(-480) // UTC-8：UTC 07:30 = 本地 08-29 23:30，日键是 08-28
	if got := CyclesFor(ts).Daily; got != "2026-08-28" {
		t.Fatalf("UTC-8 07:30 应属 08-28: %s", got)
	}

	// CycleStart 是真实时刻：UTC+8 下本地 08-30 的日周期起点 = UTC 08-29 16:00。
	SetCycleOffsetMinutes(480)
	d, w, m := CycleStart(cross)
	if got := d.UTC(); got != time.Date(2026, 8, 29, 16, 0, 0, 0, time.UTC) {
		t.Fatalf("日周期起点不符: %s", got)
	}
	// 2026-08-30（周一）本地周起点 = UTC 08-23 16:00。
	if got := w.UTC(); got != time.Date(2026, 8, 23, 16, 0, 0, 0, time.UTC) {
		t.Fatalf("周周期起点不符: %s", got)
	}
	if got := m.UTC(); got != time.Date(2026, 7, 31, 16, 0, 0, 0, time.UTC) {
		t.Fatalf("月周期起点不符: %s", got)
	}

	// 零偏移时 CycleStart 与旧行为一致（对齐 ISO 周一）。
	SetCycleOffsetMinutes(0)
	d0, w0, m0 := CycleStart(ts)
	if got := d0.UTC(); got != time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("零偏移日起点不符: %s", got)
	}
	if got := w0.UTC(); got != time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("零偏移周起点不符: %s", got)
	}
	if got := m0.UTC(); got != time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("零偏移月起点不符: %s", got)
	}
}
