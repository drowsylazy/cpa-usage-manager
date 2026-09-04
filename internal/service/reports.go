package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// ---------- 报告配置 ----------

// ReportTop 描述一个分组板块的取样方式。
type ReportTop struct {
	On     bool   `json:"on"`
	Top    int    `json:"top"`    // 1..20
	Metric string `json:"metric"` // cost | tokens | requests
}

// ReportSections 是报告的内容开关；全部关闭时仅剩总览行。
type ReportSections struct {
	Summary  bool `json:"summary"`
	Failures bool `json:"failures"`

	ByModel  *ReportTop `json:"by_model,omitempty"`
	ByKey    *ReportTop `json:"by_key,omitempty"`
	ByCaller *ReportTop `json:"by_caller,omitempty"`
}

func (sec *ReportSections) normalize() {
	for _, t := range []*ReportTop{sec.ByModel, sec.ByKey, sec.ByCaller} {
		if t == nil {
			continue
		}
		if t.Top < 1 {
			t.Top = 5
		}
		if t.Top > 20 {
			t.Top = 20
		}
		switch t.Metric {
		case "tokens", "requests":
		default:
			t.Metric = "cost"
		}
	}
}

func parseSections(raw json.RawMessage) ReportSections {
	var sec ReportSections
	if len(raw) > 0 {
		if json.Unmarshal(raw, &sec) != nil {
			sec = ReportSections{}
		}
	}
	// 完全未配置时给「总览 + 模型 Top5」，别让首份报告变成空壳。
	if !sec.Summary && !sec.Failures && sec.ByModel == nil && sec.ByKey == nil && sec.ByCaller == nil {
		sec.Summary = true
		sec.ByModel = &ReportTop{On: true, Top: 5, Metric: "cost"}
	}
	sec.normalize()
	return sec
}

var reportFrequencies = map[string]bool{"daily": true, "weekly": true, "monthly": true}

// validateReport 校验并归一报告配置；错误信息面向面板（中文）。
func validateReport(r *store.ReportConfig) error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		r.Name = "用量报告"
	}
	if !reportFrequencies[r.Frequency] {
		return fmt.Errorf("frequency 须为 daily/weekly/monthly")
	}
	if _, err := time.Parse("15:04", r.TimeOfDay); err != nil {
		return fmt.Errorf("time_of_day 须为 HH:MM")
	}
	if r.Weekday < 1 || r.Weekday > 7 {
		r.Weekday = 1
	}
	if r.Monthday < 1 || r.Monthday > 28 {
		r.Monthday = 1
	}
	if r.TZOffsetMin < -840 || r.TZOffsetMin > 840 {
		return fmt.Errorf("tz_offset_min 须在 ±840 分钟内")
	}
	if len(r.EndpointIDs) == 0 {
		return fmt.Errorf("至少选择一个发送端点")
	}
	seen := map[int64]bool{}
	var uniq []int64
	for _, id := range r.EndpointIDs {
		if id > 0 && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return fmt.Errorf("至少选择一个发送端点")
	}
	r.EndpointIDs = uniq
	sec := parseSections(r.Sections)
	sec.normalize()
	out, e := json.Marshal(sec)
	if e != nil {
		return e
	}
	r.Sections = out
	return nil
}

// ListReports 返回全部报告配置。
func (s *Service) ListReports(ctx context.Context) ([]store.ReportConfig, error) {
	return s.st.ListReportConfigs(ctx)
}

// SaveReport 新增（id==0）或更新报告配置。
func (s *Service) SaveReport(ctx context.Context, in store.ReportConfig, actor string) (int64, error) {
	if err := validateReport(&in); err != nil {
		return 0, err
	}
	var (
		id  int64
		err error
	)
	if in.ID > 0 {
		err = s.st.UpdateReportConfig(ctx, in)
		id = in.ID
	} else {
		id, err = s.st.InsertReportConfig(ctx, in)
	}
	if err != nil {
		return 0, err
	}
	s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "report.save",
		EntityType: "report", EntityID: fmt.Sprint(id),
		Detail: map[string]any{"name": in.Name, "frequency": in.Frequency, "enabled": in.Enabled}})
	return id, nil
}

// DeleteReport 删除配置。
func (s *Service) DeleteReport(ctx context.Context, id int64, actor string) error {
	if err := s.st.DeleteReportConfig(ctx, id); err != nil {
		return err
	}
	s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "report.delete",
		EntityType: "report", EntityID: fmt.Sprint(id)})
	return nil
}

// ---------- 周期计算 ----------

// reportPeriod 计算最近一个已完成周期：报告在周期结束后发送。
// local 是已按报告时区偏移过的当前时刻。返回周期标识与该周期的
// [from,to) UTC 时间范围。
func reportPeriod(freq string, local time.Time) (key string, from, to time.Time) {
	switch freq {
	case "weekly":
		// ISO 周（周一起点）。回退到本周周一零点即为上一周期的终点。
		thisMonday := mondayOf(local)
		lastMonday := thisMonday.AddDate(0, 0, -7)
		y, w := lastMonday.ISOWeek()
		key = fmt.Sprintf("%d-W%02d", y, w)
		return key, lastMonday, thisMonday
	case "monthly":
		thisFirst := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, time.UTC)
		lastFirst := thisFirst.AddDate(0, -1, 0)
		key = lastFirst.Format("2006-01")
		return key, lastFirst, thisFirst
	default: // daily
		thisZero := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
		lastZero := thisZero.AddDate(0, 0, -1)
		key = lastZero.Format("2006-01-02")
		return key, lastZero, thisZero
	}
}

func mondayOf(t time.Time) time.Time {
	// Go 的 Weekday() 周日=0；换算为 ISO 周一=1..周日=7 后退到本周一零点。
	wd := (int(t.Weekday()) + 6) % 7
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -wd)
}

// reportDue 报告是否到期：本地时刻已过触发点，且最近完成周期尚未发送过。
func reportDue(cfg store.ReportConfig, now time.Time) (key string, from, to time.Time, due bool) {
	local := now.UTC().Add(time.Duration(cfg.TZOffsetMin) * time.Minute)
	hhmm := strings.SplitN(cfg.TimeOfDay, ":", 2)
	h, _ := strconv.Atoi(hhmm[0])
	m := 0
	if len(hhmm) == 2 {
		m, _ = strconv.Atoi(hhmm[1])
	}
	triggerMin := h*60 + m
	nowMin := local.Hour()*60 + local.Minute()

	key, from, to = reportPeriod(cfg.Frequency, local)
	if cfg.LastPeriod == key {
		return key, from, to, false
	}
	switch cfg.Frequency {
	case "weekly":
		wd := (int(local.Weekday())+6)%7 + 1
		if wd != cfg.Weekday || nowMin < triggerMin {
			return key, from, to, false
		}
	case "monthly":
		if local.Day() != cfg.Monthday || nowMin < triggerMin {
			return key, from, to, false
		}
	default:
		if nowMin < triggerMin {
			return key, from, to, false
		}
	}
	return key, from, to, true
}

// ---------- 内容生成 ----------

type reportMetricRow struct {
	label              string
	requests, failures int64
	tokens             int64
	cost               money.Micro
}

// buildReport 生成一份报告的标题与正文（纯文本，覆盖所有 shoutrrr 渠道）。
func (s *Service) buildReport(ctx context.Context, cfg store.ReportConfig, key string, from, to time.Time) (string, string, error) {
	sec := parseSections(cfg.Sections)
	f := UsageFilter{From: from, To: to}
	title := "CPA 用量" + frequencyName(cfg.Frequency) + " · " + periodLabel(cfg.Frequency, key, from)

	var b strings.Builder
	sum, err := s.UsageSummary(ctx, f)
	if err != nil {
		return "", "", err
	}
	b.WriteString(periodLabel(cfg.Frequency, key, from))
	b.WriteString("\n")
	if sec.Summary {
		fmt.Fprintf(&b, "请求 %s 次 · 费用 $%s · Token %s · 成功率 %s · 缓存命中 %s\n",
			comma(sum.Requests), money.Micro(sum.CostMicroUSD).USDString(), fmtTok(sum.TotalTokens),
			pctOf(sum.Requests-sum.Failures, sum.Requests), cacheHitPct(sum))
		// 环比：与上一个等长周期比较（上期无记录时省略，不给「新增」噪音）。
		if prevSum, perr := s.UsageSummary(ctx, UsageFilter{From: from.Add(-(to.Sub(from))), To: from}); perr == nil && prevSum.Requests > 0 {
			fmt.Fprintf(&b, "环比：请求 %s · 费用 %s · Token %s\n",
				deltaPct(sum.Requests, prevSum.Requests),
				deltaPct(int64(sum.CostMicroUSD), int64(prevSum.CostMicroUSD)),
				deltaPct(sum.TotalTokens, prevSum.TotalTokens))
		}
	}
	appendTop := func(name string, t *ReportTop, dimension string, rename func(string) string) error {
		if t == nil || !t.On {
			return nil
		}
		rep, err := s.GroupByDimension(ctx, f, dimension, 0)
		if err != nil {
			return err
		}
		rows := rep.Rows
		sort.SliceStable(rows, func(i, j int) bool { return metricOf(rows[i], t.Metric) > metricOf(rows[j], t.Metric) })
		if t.Top > 0 && len(rows) > t.Top {
			rows = rows[:t.Top]
		}
		b.WriteString("\n—— " + name + " Top " + strconv.Itoa(t.Top) + "（按" + metricName(t.Metric) + "）——\n")
		if len(rows) == 0 {
			b.WriteString("（无记录）\n")
			return nil
		}
		for i, r := range rows {
			label := r.Value
			if rename != nil {
				label = rename(label)
			}
			fmt.Fprintf(&b, "%d. %s · $%s · %s tok · %s 次",
				i+1, label, r.CostMicroUSD.USDString(), fmtTok(r.TotalTokens), comma(r.Requests))
			if r.Failures > 0 {
				fmt.Fprintf(&b, " · 失败 %s", comma(r.Failures))
			}
			b.WriteString("\n")
		}
		return nil
	}
	if err := appendTop("模型", sec.ByModel, "model", nil); err != nil {
		return "", "", err
	}
	if err := appendTop("密钥", sec.ByKey, "key_id", func(kid string) string { return s.keyLabel(ctx, kid) }); err != nil {
		return "", "", err
	}
	if err := appendTop("归属", sec.ByCaller, "caller_id", func(cid string) string { return s.callerLabel(ctx, cid) }); err != nil {
		return "", "", err
	}
	if sec.Failures {
		// result 维度分组后取非 ok 行（requestFilter 的 Result 是精确等值，
		// 没有「≠ok」写法）。
		rep, err := s.GroupByDimension(ctx, f, "result", 0)
		if err == nil && sum.Failures > 0 {
			fmt.Fprintf(&b, "\n—— 失败请求 ——\n%s 次（%s）\n", comma(sum.Failures), pctOf(sum.Failures, sum.Requests))
			shown := 0
			for _, r := range rep.Rows {
				if r.Value == "" || r.Value == "ok" || shown >= 5 {
					continue
				}
				fmt.Fprintf(&b, "%s %s 次\n", resultLabel(r.Value), comma(r.Requests))
				shown++
			}
		}
	}
	return title, strings.TrimRight(b.String(), "\n"), nil
}

func frequencyName(freq string) string {
	switch freq {
	case "weekly":
		return "周报"
	case "monthly":
		return "月报"
	default:
		return "日报"
	}
}

func periodLabel(freq, key string, from time.Time) string {
	switch freq {
	case "weekly":
		return key + "（" + from.Format("01-02") + " 起）"
	default:
		return key
	}
}

func metricOf(r DimensionRow, metric string) int64 {
	switch metric {
	case "tokens":
		return r.TotalTokens
	case "requests":
		return r.Requests
	default:
		return int64(r.CostMicroUSD)
	}
}

func metricName(m string) string {
	switch m {
	case "tokens":
		return "Token"
	case "requests":
		return "请求数"
	default:
		return "费用"
	}
}

func resultLabel(v string) string {
	if v == "" {
		v = "unknown"
	}
	return v
}

func pctOf(part, total int64) string {
	if total <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", float64(part)*100/float64(total))
}

func cacheHitPct(sum UsageSummary) string {
	hit := sum.CachedTokens
	if hit == 0 {
		hit = sum.CacheReadTokens
	} else if sum.CacheReadTokens > hit {
		hit = sum.CacheReadTokens
	}
	total := sum.InputTokens + sum.OutputTokens + sum.ReasoningTokens
	if total <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", float64(hit)*100/float64(total))
}

// comma 千分位格式化整数。
func comma(v int64) string {
	s := strconv.FormatInt(v, 10)
	if len(s) <= 4 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return s + "," + strings.Join(parts, ",")
}

// fmtTok token 数的 K/M/B 缩写（与面板 fmtTok 同风格）。
func fmtTok(v int64) string {
	neg := ""
	if v < 0 {
		neg = "-"
		v = -v
	}
	switch {
	case v >= 999_500_000_000:
		return fmt.Sprintf("%s%.2fT", neg, float64(v)/1e12)
	case v >= 999_500_000:
		return fmt.Sprintf("%s%.2fB", neg, float64(v)/1e9)
	case v >= 999_500:
		return fmt.Sprintf("%s%.2fM", neg, float64(v)/1e6)
	case v >= 1000:
		return fmt.Sprintf("%s%.2fK", neg, float64(v)/1e3)
	default:
		return neg + strconv.FormatInt(v, 10)
	}
}

// keyLabel 把 kid 映射为「标签 (短kid)」，查不到就用短 kid。
func (s *Service) keyLabel(ctx context.Context, kid string) string {
	short := kid
	if len(short) > 12 {
		short = short[:6] + "…" + short[len(short)-4:]
	}
	if k, err := s.st.GetKey(ctx, kid); err == nil && k.Label != "" {
		return k.Label + " (" + short + ")"
	}
	return short
}

// callerLabel 把 caller_id 映射为显示名。
func (s *Service) callerLabel(ctx context.Context, cid string) string {
	if c, err := s.st.GetCaller(ctx, cid); err == nil && c.DisplayName != "" {
		return c.DisplayName
	}
	return cid
}

// ---------- 发送与调度 ----------

// RunReportsSweep 扫描全部启用报告，把到期的发出去。非租约持有者直接跳过。
func (s *Service) RunReportsSweep(ctx context.Context) (int, error) {
	if !s.st.Writable() {
		return 0, nil
	}
	cfgs, err := s.st.ListReportConfigs(ctx)
	if err != nil {
		return 0, err
	}
	endpoints, err := s.st.ListNotifyEndpoints(ctx)
	if err != nil {
		return 0, err
	}
	epByID := map[int64]store.NotifyEndpoint{}
	for _, e := range endpoints {
		epByID[e.ID] = e
	}
	now := time.Now().UTC()
	sent := 0
	for _, cfg := range cfgs {
		if !cfg.Enabled || ctx.Err() != nil {
			continue
		}
		key, from, to, due := reportDue(cfg, now)
		if !due {
			continue
		}
		title, body, err := s.buildReport(ctx, cfg, key, from, to)
		if err != nil {
			_ = s.st.UpdateReportResult(ctx, cfg.ID, key, "生成报告失败："+err.Error(), time.Now().UTC())
			s.NotifyErrorEvent(ctx, "report", fmt.Sprintf("报告「%s」生成失败：%v", cfg.Name, err))
			continue
		}
		sendErr := s.sendToEndpoints(ctx, epByID, cfg.EndpointIDs, title, body)
		msg := ""
		if sendErr != nil {
			msg = sendErr.Error()
			s.NotifyErrorEvent(ctx, "report", fmt.Sprintf("报告「%s」发送失败：%s", cfg.Name, msg))
		} else {
			sent++
		}
		_ = s.st.UpdateReportResult(ctx, cfg.ID, key, msg, time.Now().UTC())
		s.st.AppendAudit(ctx, store.AuditEvent{Actor: "scheduler", Action: "report.send",
			EntityType: "report", EntityID: fmt.Sprint(cfg.ID),
			Detail: map[string]any{"period": key, "ok": sendErr == nil}})
	}
	return sent, nil
}

// sendToEndpoints 向指定端点集合发送一条消息；至少一个成功即视为整体成功。
func (s *Service) sendToEndpoints(ctx context.Context, epByID map[int64]store.NotifyEndpoint, ids []int64, title, body string) error {
	var errs []string
	delivered := false
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		e, ok := epByID[id]
		if !ok || !e.Enabled {
			continue
		}
		url, err := s.decryptURL(e.URLEnc)
		if err != nil {
			errs = append(errs, fmt.Sprintf("#%d URL 解密失败", id))
			continue
		}
		if err := shoutrrrSend(url, title, body); err != nil {
			errs = append(errs, fmt.Sprintf("#%d %s", id, err.Error()))
			continue
		}
		delivered = true
	}
	if delivered {
		if len(errs) > 0 {
			return fmt.Errorf("部分端点失败：%s", strings.Join(errs, "；"))
		}
		return nil
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "；"))
	}
	return errors.New("没有可用的启用端点")
}

// TestReport 按最近一个已完成周期立即生成并发送报告，不推进 last_period。
func (s *Service) TestReport(ctx context.Context, id int64, actor string) error {
	cfg, err := s.st.GetReportConfig(ctx, id)
	if err != nil {
		return err
	}
	endpoints, err := s.st.ListNotifyEndpoints(ctx)
	if err != nil {
		return err
	}
	epByID := map[int64]store.NotifyEndpoint{}
	for _, e := range endpoints {
		epByID[e.ID] = e
	}
	now := time.Now().UTC()
	key, from, to, _ := reportDue(cfg, now)
	title, body, err := s.buildReport(ctx, cfg, key, from, to)
	if err != nil {
		return err
	}
	title = "[测试] " + title
	sendErr := s.sendToEndpoints(ctx, epByID, cfg.EndpointIDs, title, body)
	msg := ""
	if sendErr != nil {
		msg = sendErr.Error()
	}
	_ = s.st.UpdateReportResult(ctx, cfg.ID, "", msg, time.Now().UTC())
	s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "report.test",
		EntityType: "report", EntityID: fmt.Sprint(cfg.ID), Detail: map[string]any{"ok": sendErr == nil}})
	return sendErr
}

// deltaPct 计算环比百分比文本（当前 vs 上期）。
func deltaPct(cur, prev int64) string {
	if prev == 0 {
		if cur == 0 {
			return "持平"
		}
		return "新增"
	}
	d := (cur - prev) * 100 / prev
	if d == 0 {
		return "持平"
	}
	sign := "+"
	if d < 0 {
		sign = ""
	}
	return fmt.Sprintf("%s%d%%", sign, d)
}
