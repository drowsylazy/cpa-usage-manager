// Package service 内的管理面辅助逻辑：归属、认证额度、维护动作与导出。
package service

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// ---------- 归属（caller） ----------

// ListCallers 列出全部归属。
func (s *Service) ListCallers(ctx context.Context) ([]store.Caller, error) {
	return s.st.ListCallers(ctx)
}

// UpsertCaller 创建归属，或在已存在时更新其展示名。
func (s *Service) UpsertCaller(ctx context.Context, id, displayName, actor string) (store.Caller, error) {
	id = strings.TrimSpace(id)
	if err := store.ValidCallerID(id); err != nil {
		return store.Caller{}, err
	}
	c, err := s.st.CreateCaller(ctx, id, displayName)
	if errors.Is(err, store.ErrConflict) {
		if uErr := s.st.UpdateCallerDisplayName(ctx, id, displayName); uErr != nil {
			return store.Caller{}, uErr
		}
		c, err = s.st.GetCaller(ctx, id)
	}
	if err != nil {
		return store.Caller{}, err
	}
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: actor, Action: "caller.upsert", EntityType: "caller", EntityID: id,
		Detail: map[string]any{"display_name": c.DisplayName},
	})
	return c, nil
}

// SetCallerEnabled 启用/停用归属。停用后其下 Key 无法通过鉴权。
func (s *Service) SetCallerEnabled(ctx context.Context, id string, enabled bool, actor string) error {
	if err := s.st.SetCallerEnabled(ctx, id, enabled); err != nil {
		return err
	}
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: actor, Action: "caller.enabled", EntityType: "caller", EntityID: id,
		Detail: map[string]any{"enabled": enabled},
	})
	return nil
}

// ---------- 认证额度 ----------

// AuthQuotaWindow 是面板展示用的一个额度窗口。
type AuthQuotaWindow struct {
	WindowID string `json:"window_id"`
	CycleKey string `json:"cycle_key"`
	Observed int64  `json:"observed"`
	Baseline int64  `json:"baseline"`
	// Delta 是本周期内的增量（observed - baseline，下界 0）。
	Delta     int64     `json:"delta"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AuthQuotaView 是一个认证账号的额度视图。
type AuthQuotaView struct {
	Provider  string            `json:"provider"`
	AuthID    string            `json:"auth_id"`
	Status    string            `json:"status"`
	FetchedAt time.Time         `json:"fetched_at"`
	Snapshot  map[string]any    `json:"snapshot,omitempty"`
	Windows   []AuthQuotaWindow `json:"windows,omitempty"`
}

// AuthQuotas 汇总认证额度快照与窗口增量。
//
// 返回内容只含宿主已清洗过的展示信息：不含上游 API Key 明文或密文。
func (s *Service) AuthQuotas(ctx context.Context) ([]AuthQuotaView, error) {
	snaps, err := s.st.ListAuthQuotaSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	windows, err := s.st.ListAuthQuotaWindows(ctx, "", "")
	if err != nil {
		return nil, err
	}
	byAuth := make(map[string][]AuthQuotaWindow, len(snaps))
	for _, w := range windows {
		k := w.Provider + "\x00" + w.AuthID
		byAuth[k] = append(byAuth[k], AuthQuotaWindow{
			WindowID: w.WindowID, CycleKey: w.CycleKey,
			Observed: w.Observed, Baseline: w.Baseline,
			Delta: w.Delta(), UpdatedAt: w.UpdatedAt,
		})
	}
	out := make([]AuthQuotaView, 0, len(snaps))
	seen := make(map[string]bool, len(snaps))
	for _, sn := range snaps {
		k := sn.Provider + "\x00" + sn.AuthID
		seen[k] = true
		out = append(out, AuthQuotaView{
			Provider: sn.Provider, AuthID: sn.AuthID, Status: sn.Status,
			FetchedAt: sn.FetchedAt, Snapshot: sn.Snapshot, Windows: byAuth[k],
		})
	}
	// 只有窗口观测、还没有快照的账号也要出现在面板上。
	for _, w := range windows {
		k := w.Provider + "\x00" + w.AuthID
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, AuthQuotaView{
			Provider: w.Provider, AuthID: w.AuthID, Status: "unknown", Windows: byAuth[k],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].AuthID < out[j].AuthID
	})
	return out, nil
}

// ---------- 维护动作 ----------

// BackupMaxBytes 是备份接口允许导出的上限（DESIGN §5.1）。
const BackupMaxBytes int64 = 64 << 20

// Backup 把数据库写入 w，并记审计。
func (s *Service) Backup(ctx context.Context, w io.Writer, actor string) (store.BackupResult, error) {
	res, err := s.st.BackupTo(ctx, w, store.BackupOptions{MaxBytes: BackupMaxBytes})
	if err != nil {
		return store.BackupResult{}, err
	}
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: actor, Action: "system.backup", EntityType: "system", EntityID: "database",
		Detail: map[string]any{"bytes": res.Bytes},
	})
	return res, nil
}

// RunAutoBackup 写出一份定时备份文件（与 /backup 下载同格式）并按 keep
// 轮转删除最旧份。返回文件路径。dir 不存在时按 0700 创建。
func (s *Service) RunAutoBackup(ctx context.Context, dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建备份目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".bak-tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	res, err := s.st.BackupTo(ctx, tmp, store.BackupOptions{MaxBytes: BackupMaxBytes})
	if err != nil {
		cleanup()
		return "", fmt.Errorf("备份快照写出失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	final := filepath.Join(dir, "cpa-usage-manager_"+time.Now().Format("20060102-150405")+".bak")
	if err := os.Rename(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	s.rotateAutoBackups(dir, keep)
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: "auto", Action: "system.auto_backup", EntityType: "system", EntityID: "database",
		Detail: map[string]any{"bytes": res.Bytes, "path": final},
	})
	return final, nil
}

// rotateAutoBackups 删除超出 keep 份数的最旧备份（文件名时间戳降序，尾部即最旧）。
func (s *Service) rotateAutoBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil || keep < 1 {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "cpa-usage-manager_") && strings.HasSuffix(e.Name(), ".bak") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names) // 时间戳命名，字典序即时间序
	for _, n := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(dir, n))
	}
}

// Restore 用上传的快照替换库内容，并记审计。
//
// 注意：备份文件不含 key-peppers。恢复到另一台机器时必须同时带上
// data_dir/key-peppers，否则 Key 密文无法解密（哈希校验仍可用）。
func (s *Service) Restore(ctx context.Context, src io.Reader, actor string) (store.RestoreResult, error) {
	res, err := s.st.RestoreFrom(ctx, src, BackupMaxBytes)
	if err != nil {
		return store.RestoreResult{}, err
	}
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: actor, Action: "system.restore", EntityType: "system", EntityID: "database",
		Detail: map[string]any{"bytes": res.Bytes, "tables": res.Tables},
	})
	return res, nil
}

// Reset 按范围清空统计，并记审计。
func (s *Service) Reset(ctx context.Context, opts store.ResetOptions, actor string) (store.ResetResult, error) {
	res, err := s.st.ResetStatistics(ctx, opts)
	if err != nil {
		return store.ResetResult{}, err
	}
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: actor, Action: "system.reset", EntityType: "system", EntityID: "statistics",
		Detail: map[string]any{
			"requests": res.Requests, "rollups": res.Rollups,
			"reservations": res.Reservations, "keys": res.Keys,
			"scope": map[string]bool{
				"requests": opts.Requests, "reservations": opts.Reservations,
				"key_counters": opts.KeyCounters, "audit": opts.Audit,
			},
		},
	})
	return res, nil
}

// Maintain 执行保留清理、历史重复行对账，并可选 VACUUM，供系统页与宿主定时任务调用。
//
// 对账（DedupeRequests）与保留清理放在同一次维护里：v0.2.2 之前的版本会把同一请求
// 记两次（执行器行 + 宿主被动行），这些历史行只能靠事后扫描合并。
func (s *Service) Maintain(ctx context.Context, vacuum bool, actor string) (store.RetentionResult, error) {
	cfg := s.Config()
	now := time.Now().UTC()
	res, err := s.st.ApplyRetention(ctx, cfg.RetentionDays, now)
	if err != nil {
		return store.RetentionResult{}, err
	}
	// 顺带释放陈旧预占：崩溃/重启残留的 held 行没有定时清扫方，
	// 借维护入口兜底，让面板「清理」也能在无流量时归零在途读数。
	if _, err := s.st.ReleaseStaleReservations(ctx, now.Add(-cfg.Quota.Stream.StaleReservationTimeout.Std())); err != nil {
		return store.RetentionResult{}, fmt.Errorf("释放陈旧预占失败（保留清理已完成）: %w", err)
	}
	// 对账窗口跟随保留期：更早的行已被上一步删掉，扫描它们没有意义。
	since := now.AddDate(0, 0, -maxInt(1, cfg.RetentionDays))
	deduped, derr := s.st.DedupeRequests(ctx, since)
	res.Deduped = deduped
	if vacuum {
		if err := s.st.Vacuum(ctx); err != nil {
			return res, err
		}
	}
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: actor, Action: "system.maintain", EntityType: "system", EntityID: "retention",
		Detail: map[string]any{
			"retention_days": cfg.RetentionDays, "vacuum": vacuum,
			"requests": res.Requests, "rollups": res.Rollups, "reservations": res.Reservations,
			"deduped": deduped,
		},
	})
	if derr != nil {
		return res, fmt.Errorf("重复请求对账失败（保留清理已完成）: %w", derr)
	}
	return res, nil
}

// Dedupe 单独执行历史重复行对账，供系统页「对账去重」按钮调用。
// since 为零值时按保留期回溯。
func (s *Service) Dedupe(ctx context.Context, since time.Time, actor string) (int, error) {
	if since.IsZero() {
		since = time.Now().UTC().AddDate(0, 0, -maxInt(1, s.Config().RetentionDays))
	}
	n, err := s.st.DedupeRequests(ctx, since)
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: actor, Action: "system.dedupe", EntityType: "system", EntityID: "requests",
		Detail: map[string]any{"since": since.UTC().Format(time.RFC3339), "merged": n},
	})
	if err != nil {
		return n, fmt.Errorf("重复请求对账失败: %w", err)
	}
	return n, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------- CSV 导出 ----------

// ExportKinds 是导出接口支持的数据集。
var ExportKinds = []string{"requests", "dimension", "trends", "keys", "pricing", "audit"}

// ExportRequest 描述一次导出。
type ExportRequest struct {
	Kind      string      `json:"kind"`
	Dimension string      `json:"dimension,omitempty"`
	Grain     string      `json:"grain,omitempty"`
	Metric    string      `json:"metric,omitempty"`
	Limit     int         `json:"limit,omitempty"`
	Filter    QueryFilter `json:"filter"`
}

// exportFileName 构造导出文件名：kind + 时间范围标记（归档后可直接对账，
// 无范围标记 _all）+ UTC 时间戳。ExportCSV/ExportPNG 与 ExportTarget 共用，
// 保证头预置的 Content-Disposition 与实际内容一致。
func exportFileName(kind string, f UsageFilter, ext string) string {
	rangeTag := ""
	if !f.From.IsZero() {
		rangeTag += "_" + f.From.UTC().Format("20060102-1504")
	}
	if !f.To.IsZero() {
		rangeTag += "-" + f.To.UTC().Format("20060102-1504")
	}
	if rangeTag == "" {
		rangeTag = "_all"
	}
	return fmt.Sprintf("cpa-usage-manager_%s%s_%s.%s", kind, rangeTag, time.Now().UTC().Format("20060102-150405"), ext)
}

// ExportTarget 返回导出端点的建议文件名与 Content-Type。HTTP 处理器必须
// 在写响应体之前设置响应头（首次写入即快照 header，之后再设会被丢弃），
// 因此文件名要先于 Export 的写入过程算出来。
func (s *Service) ExportTarget(req ExportRequest, png bool) (string, string) {
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		if png {
			kind = "trends"
		} else {
			kind = "requests"
		}
	}
	f := req.Filter.Usage()
	if png {
		return exportFileName(kind, f, "png"), "image/png"
	}
	return exportFileName(kind, f, "csv"), "text/csv; charset=utf-8"
}

// ExportCSV 把指定数据集写成 CSV（UTF-8 BOM 开头，便于 Excel 直接打开）。
// 返回建议的文件名。
func (s *Service) ExportCSV(ctx context.Context, w io.Writer, req ExportRequest) (string, error) {
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "requests"
	}
	limit := req.Limit
	if limit <= 0 || limit > 100000 {
		limit = 10000
	}
	f := req.Filter.Usage()

	if _, err := w.Write([]byte("\xef\xbb\xbf")); err != nil {
		return "", err
	}
	cw := csv.NewWriter(w)
	defer cw.Flush()

	name := exportFileName(kind, f, "csv")
	switch kind {
	case "requests":
		// 流式写出：逐行遍历，不把整页请求装载进内存（上限 10 万行时
		// 全量装载约需数十 MB 峰值）。
		if err := cw.Write(requestCSVHeader); err != nil {
			return "", err
		}
		if err := s.IterateRequests(ctx, f, limit, req.Filter.Sort, req.Filter.Order, func(r store.Request) error {
			return cw.Write(requestCSVRow(r))
		}); err != nil {
			return "", err
		}
	case "dimension":
		rep, err := s.GroupByDimension(ctx, f, req.Dimension, limit)
		if err != nil {
			return "", err
		}
		if err := writeCSV(cw, dimensionCSVHeader, len(rep.Rows), func(i int) []string {
			return dimensionCSVRow(rep.Rows[i])
		}); err != nil {
			return "", err
		}
	case "trends":
		points, err := s.Trends(ctx, f, req.Grain)
		if err != nil {
			return "", err
		}
		header := []string{"bucket", "requests", "failures", "input_tokens", "output_tokens", "total_tokens", "cost_usd"}
		if err := writeCSV(cw, header, len(points), func(i int) []string {
			p := points[i]
			return []string{p.Bucket.Format(time.RFC3339), itoa(p.Requests), itoa(p.Failures),
				itoa(p.InputTokens), itoa(p.OutputTokens), itoa(p.TotalTokens),
				money.Micro(p.CostMicroUSD).USDString()}
		}); err != nil {
			return "", err
		}
	case "keys":
		rows, err := s.UsageSummaryByKey(ctx, f, time.Now().UTC())
		if err != nil {
			return "", err
		}
		if err := writeCSV(cw, keyCSVHeader, len(rows), func(i int) []string {
			return keyCSVRow(rows[i])
		}); err != nil {
			return "", err
		}
	case "pricing":
		rules, err := s.st.ListPricingRules(ctx, false)
		if err != nil {
			return "", err
		}
		if err := writeCSV(cw, pricingCSVHeader, len(rules), func(i int) []string {
			return pricingCSVRow(rules[i])
		}); err != nil {
			return "", err
		}
	case "audit":
		events, err := s.st.ListAudit(ctx, limit, 0)
		if err != nil {
			return "", err
		}
		header := []string{"id", "ts", "actor", "action", "entity_type", "entity_id"}
		if err := writeCSV(cw, header, len(events), func(i int) []string {
			e := events[i]
			return []string{itoa(e.ID), e.TS.Format(time.RFC3339), e.Actor, e.Action, e.EntityType, e.EntityID}
		}); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("不支持的导出类型 %q，可选：%s", kind, strings.Join(ExportKinds, ", "))
	}
	cw.Flush()
	return name, cw.Error()
}

// writeCSV 写表头与 n 行数据。
func writeCSV(cw *csv.Writer, header []string, n int, row func(i int) []string) error {
	if err := cw.Write(header); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if err := cw.Write(row(i)); err != nil {
			return err
		}
	}
	return nil
}

var requestCSVHeader = []string{"id", "ts", "key_id", "caller_id", "model", "upstream_model", "provider", "source",
	"auth_label", "auth_type", "tier", "result", "input_tokens", "output_tokens", "reasoning_tokens",
	"cached_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens",
	"latency_ms", "ttft_ms", "tps", "thinking_intensity", "cost", "currency", "cost_usd", "priced"}

func requestCSVRow(r store.Request) []string {
	currency := r.Currency
	if currency == "" {
		currency = "USD"
	}
	return []string{r.ID, r.TS.Format(time.RFC3339), r.KeyID, r.CallerID, r.Model, r.UpstreamModel, r.Provider, r.Source,
		r.AuthLabel, r.AuthType, r.Tier, r.Result,
		itoa(r.InputTokens), itoa(r.OutputTokens), itoa(r.ReasoningTokens), itoa(r.CachedTokens),
		itoa(r.CacheReadTokens), itoa(r.CacheCreationTokens), itoa(r.TotalTokens),
		itoa(r.LatencyMS), itoa(r.TTFTMS), milliString(r.TPSMilli), r.ThinkingIntensity,
		// cost = 原生币种金额（currency 指明币种），cost_usd = 美元等值（按规则锁定汇率折算）。
		money.Micro(r.CostNativeMicro).USDString(), currency, r.CostMicroUSD.USDString(), strconv.FormatBool(r.Priced)}
}

var dimensionCSVHeader = []string{"value", "requests", "failures", "input_tokens", "output_tokens",
	"reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens",
	"cost_usd", "latency_avg_ms", "ttft_avg_ms", "tps_avg", "cache_hit_rate"}

func dimensionCSVRow(r DimensionRow) []string {
	return []string{r.Value, itoa(r.Requests), itoa(r.Failures), itoa(r.InputTokens), itoa(r.OutputTokens),
		itoa(r.ReasoningTokens), itoa(r.CachedTokens), itoa(r.CacheReadTokens), itoa(r.CacheCreationTokens),
		itoa(r.TotalTokens), r.CostMicroUSD.USDString(), itoa(r.LatencyAvgMS), itoa(r.TTFTAvgMS),
		milliString(r.TPSAvgMilli), basisPointString(r.CacheHitRateBP)}
}

var keyCSVHeader = []string{"kid", "label", "principal", "caller_id", "enabled", "revoked",
	"requests", "failures", "total_tokens", "cost_usd",
	"quota_usd", "spent_usd", "daily_usd", "daily_spent_usd",
	"weekly_usd", "weekly_spent_usd", "monthly_usd", "monthly_spent_usd",
	"held_usd", "concurrent", "last_used_at"}

func keyCSVRow(k KeySummary) []string {
	last := ""
	if k.LastUsedAt != nil {
		last = k.LastUsedAt.Format(time.RFC3339)
	}
	return []string{k.KID, k.Label, k.Principal, k.CallerID,
		strconv.FormatBool(k.Enabled), strconv.FormatBool(k.Revoked),
		itoa(k.Requests), itoa(k.Failures), itoa(k.TotalTokens), k.CostMicroUSD.USDString(),
		microPtrString(k.QuotaMicroUSD), k.SpentMicroUSD.USDString(),
		microPtrString(k.DailyMicroUSD), k.DailySpent.USDString(),
		microPtrString(k.WeeklyMicroUSD), k.WeeklySpent.USDString(),
		microPtrString(k.MonthlyMicroUSD), k.MonthlySpent.USDString(),
		k.HeldMicroUSD.USDString(), itoa(k.Concurrent), last}
}

var pricingCSVHeader = []string{"id", "match_kind", "pattern", "priority", "enabled",
	"price_input", "price_output", "price_cache_read",
	"price_cache_creation", "accounting_mode", "billing_mode", "per_image_usd", "source", "models_dev_id"}

func pricingCSVRow(r store.PricingRule) []string {
	return []string{itoa(r.ID), r.MatchKind, r.Pattern, strconv.Itoa(r.Priority), strconv.FormatBool(r.Enabled),
		r.PriceInput.USDPerMillionString(), r.PriceOutput.USDPerMillionString(),
		r.PriceCacheRead.USDPerMillionString(), r.PriceCacheCreation.USDPerMillionString(),
		r.AccountingMode, r.BillingMode, r.PerImageMicroUSD.USDString(), r.Source, r.ModelsDevID}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// milliString 把千分单位整数渲染为三位小数，避免浮点。
func milliString(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d.%03d", v/1000, v%1000)
	if neg {
		return "-" + s
	}
	return s
}

// basisPointString 把万分比渲染为百分数文本（两位小数）。
func basisPointString(bp int64) string {
	neg := bp < 0
	if neg {
		bp = -bp
	}
	s := fmt.Sprintf("%d.%02d%%", bp/100, bp%100)
	if neg {
		return "-" + s
	}
	return s
}

func microPtrString(p *money.Micro) string {
	if p == nil {
		return ""
	}
	return p.USDString()
}
