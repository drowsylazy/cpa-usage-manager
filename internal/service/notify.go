package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// ---------- 设置（preferences: notify_settings） ----------

// NotifySettings 是告警通知的全局设置。
type NotifySettings struct {
	// Enabled 是额度告警的总开关；关闭时扫描直接跳过。
	Enabled bool `json:"enabled"`
	// WarnPct 是告警阈值百分比：任一启用档的余量占比降到该值以下即告警（1..95）。
	WarnPct int `json:"warn_pct"`
	// ErrorAlerts 开启后，报告发送失败、存储降级等系统错误会经通知端点上报。
	ErrorAlerts bool `json:"error_alerts"`
	// SingleCostAlert 开启后，单请求结算的费用或计费 Token 达到阈值即推送
	// （发现「误把批量任务打到昂贵模型」最快的一类信号；按 Key 每小时限一条）。
	SingleCostAlert bool `json:"single_cost_alert"`
	// 单请求费用阈值（micro-USD），0=不按费用判定。
	SingleCostMicroUSD int64 `json:"single_cost_micro_usd"`
	// 单请求计费 Token 阈值，0=不按 Token 判定。
	SingleTokenThreshold int64 `json:"single_token_threshold"`

	// ErrorRateAlert 开启后，滑动窗口内失败请求占比超过阈值即推送
	// （服务整体坏没坏的最基本信号；恢复后自动重新武装）。
	ErrorRateAlert bool `json:"error_rate_alert"`
	// 错误率判定窗口（分钟，1..60）。
	ErrorRateWindowMin int `json:"error_rate_window_min"`
	// 错误率阈值百分比（1..100），窗口内 失败/总数 ≥ 该值触发。
	ErrorRatePct int `json:"error_rate_pct"`

	// ExpireWarnDays 是密钥临期预警天数（0=关闭；>0 时启用中密钥将在
	// 到期前 N 天推送一次，与过期告警互相独立）。
	ExpireWarnDays int `json:"expire_warn_days"`
}

func defaultNotifySettings() NotifySettings {
	return NotifySettings{Enabled: false, WarnPct: 20, ErrorRateWindowMin: 10, ErrorRatePct: 50}
}

const notifySettingsKey = "notify_settings"

// GetNotifySettings 读取设置；未配置过时返回默认值。
func (s *Service) GetNotifySettings(ctx context.Context) (NotifySettings, error) {
	out := defaultNotifySettings()
	raw, ok, err := s.st.GetPreference(ctx, notifySettingsKey)
	if err != nil || !ok {
		return out, err
	}
	if e := json.Unmarshal([]byte(raw), &out); e != nil {
		return defaultNotifySettings(), nil
	}
	out.normalize()
	return out, nil
}

func (n *NotifySettings) normalize() {
	if n.WarnPct < 1 {
		n.WarnPct = 20
	}
	if n.WarnPct > 95 {
		n.WarnPct = 95
	}
	if n.SingleCostMicroUSD < 0 {
		n.SingleCostMicroUSD = 0
	}
	if n.SingleTokenThreshold < 0 {
		n.SingleTokenThreshold = 0
	}
	if n.ErrorRateWindowMin < 1 {
		n.ErrorRateWindowMin = 10
	}
	if n.ErrorRateWindowMin > 60 {
		n.ErrorRateWindowMin = 60
	}
	if n.ErrorRatePct < 1 {
		n.ErrorRatePct = 50
	}
	if n.ErrorRatePct > 100 {
		n.ErrorRatePct = 100
	}
	if n.ExpireWarnDays < 0 {
		n.ExpireWarnDays = 0
	}
	if n.ExpireWarnDays > 90 {
		n.ExpireWarnDays = 90
	}
}

// SaveNotifySettings 校验并保存设置。
func (s *Service) SaveNotifySettings(ctx context.Context, in NotifySettings, actor string) error {
	in.normalize()
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if err := s.st.SetPreference(ctx, notifySettingsKey, string(raw)); err != nil {
		return err
	}
	s.notifyCfgMu.Lock()
	s.notifyCfg = nil
	s.notifyCfgMu.Unlock()
	s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "notify.settings_save",
		EntityType: "notify", EntityID: "settings",
		Detail: map[string]any{"enabled": in.Enabled, "warn_pct": in.WarnPct}})
	return nil
}

// ---------- 端点 CRUD（URL 加密存储） ----------

func (s *Service) activePepper() ([]byte, error) {
	p, ok := s.peppers.Items[s.peppers.Active]
	if !ok {
		return nil, fmt.Errorf("active pepper %q 不存在", s.peppers.Active)
	}
	return p.Value, nil
}

func (s *Service) encryptURL(url string) ([]byte, error) {
	pepper, err := s.activePepper()
	if err != nil {
		return nil, err
	}
	return encrypt(pepper, []byte(url))
}

func (s *Service) decryptURL(urlEnc []byte) (string, error) {
	pepper, err := s.activePepper()
	if err != nil {
		return "", err
	}
	b, err := decrypt(pepper, urlEnc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ListNotifyEndpoints 返回全部端点（URL 已解密，供面板展示/编辑）。
func (s *Service) ListNotifyEndpoints(ctx context.Context) ([]store.NotifyEndpoint, error) {
	list, err := s.st.ListNotifyEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		u, e := s.decryptURL(list[i].URLEnc)
		if e != nil {
			list[i].URL = ""
			list[i].LastError = "URL 解密失败：" + e.Error()
			continue
		}
		list[i].URL = u
	}
	return list, nil
}

// SaveNotifyEndpoint 新增（id==0）或更新一条端点。url 必须是 shoutrrr 可解析的服务 URL。
func (s *Service) SaveNotifyEndpoint(ctx context.Context, id int64, label, url string, enabled bool, actor string) (int64, error) {
	label = strings.TrimSpace(label)
	url = strings.TrimSpace(url)
	if url == "" {
		return 0, errors.New("缺少通知 URL")
	}
	if len(url) > 2048 {
		return 0, errors.New("通知 URL 过长")
	}
	enc, err := s.encryptURL(url)
	if err != nil {
		return 0, err
	}
	if id > 0 {
		if err := s.st.UpdateNotifyEndpoint(ctx, id, label, enc, enabled); err != nil {
			return 0, err
		}
		s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "notify.endpoint_save",
			EntityType: "notify_endpoint", EntityID: fmt.Sprint(id),
			Detail: map[string]any{"label": label, "enabled": enabled}})
		return id, nil
	}
	newID, err := s.st.InsertNotifyEndpoint(ctx, label, enc, enabled, time.Now())
	if err != nil {
		return 0, err
	}
	s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "notify.endpoint_save",
		EntityType: "notify_endpoint", EntityID: fmt.Sprint(newID),
		Detail: map[string]any{"label": label, "enabled": enabled}})
	return newID, nil
}

// DeleteNotifyEndpoint 删除端点。
func (s *Service) DeleteNotifyEndpoint(ctx context.Context, id int64, actor string) error {
	if err := s.st.DeleteNotifyEndpoint(ctx, id); err != nil {
		return err
	}
	s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "notify.endpoint_delete",
		EntityType: "notify_endpoint", EntityID: fmt.Sprint(id)})
	return nil
}

// TestNotifyEndpoint 发送测试消息。endpointID>0 时对已存端点测试（并记录结果），
// 否则直接测试传入的 draft URL。
func (s *Service) TestNotifyEndpoint(ctx context.Context, endpointID int64, url, actor string) error {
	if endpointID > 0 {
		e, err := s.st.GetNotifyEndpoint(ctx, endpointID)
		if err != nil {
			return err
		}
		u, err := s.decryptURL(e.URLEnc)
		if err != nil {
			return err
		}
		url = u
	}
	url = strings.TrimSpace(url)
	if url == "" {
		return errors.New("缺少通知 URL")
	}
	err := shoutrrrSend(url, "CPA Usage Manager 测试消息", "通知通道配置成功。")
	if endpointID > 0 {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		_ = s.st.UpdateNotifyEndpointResult(ctx, endpointID, time.Now().UTC(), msg)
	}
	detail := map[string]any{"ok": err == nil}
	if endpointID > 0 {
		detail["endpoint_id"] = endpointID
	}
	s.st.AppendAudit(ctx, store.AuditEvent{Actor: actor, Action: "notify.test",
		EntityType: "notify", EntityID: "test", Detail: detail})
	return err
}

// ---------- 错误上报 ----------

const notifyErrorStateKey = "notify_error_state"

// errorAlertCooldown 是同一来源错误的上报最小间隔。错误往往随扫描周期
// 反复出现（每分钟一轮），不冷却会变成轰炸；冷却按「尝试时间」推进，
// 失败也计入，避免对着挂掉的端点每分钟重试。
const errorAlertCooldown = time.Hour

// NotifyErrorEvent 把一条系统错误经全部启用端点上报（受设置里的
// 「错误上报」开关控制，同来源一小时内最多一条）。异步场景请自带 ctx 超时。
func (s *Service) NotifyErrorEvent(ctx context.Context, source, text string) {
	cfg, err := s.GetNotifySettings(ctx)
	if err != nil || !cfg.ErrorAlerts {
		return
	}
	endpoints, err := s.st.ListNotifyEndpoints(ctx)
	if err != nil {
		return
	}
	var targets []store.NotifyEndpoint
	for _, e := range endpoints {
		if e.Enabled {
			targets = append(targets, e)
		}
	}
	if len(targets) == 0 {
		return
	}

	state := map[string]int64{}
	if raw, ok, err := s.st.GetPreference(ctx, notifyErrorStateKey); err == nil && ok {
		_ = json.Unmarshal([]byte(raw), &state)
	}
	now := time.Now().UTC()
	if now.Unix()-state[source] < int64(errorAlertCooldown.Seconds()) {
		return
	}

	body := fmt.Sprintf("来源：%s\n%s\n时间：%s", source, text, now.Format("2006-01-02 15:04 UTC"))
	for _, e := range targets {
		if ctx.Err() != nil {
			return
		}
		url, err := s.decryptURL(e.URLEnc)
		if err != nil {
			continue
		}
		msg := ""
		if err := shoutrrrSend(url, "CPA Usage Manager 错误上报", body); err != nil {
			msg = err.Error()
		}
		_ = s.st.UpdateNotifyEndpointResult(ctx, e.ID, now, msg)
	}
	state[source] = now.Unix()
	if raw, err := json.Marshal(state); err == nil {
		_ = s.st.SetPreference(ctx, notifyErrorStateKey, string(raw))
	}
}

// ---------- 单请求异常告警 ----------

const notifySingleStateKey = "notify_single_state"

// singleAlertCooldown 是同一 Key 单请求异常告警的最小间隔：批量任务误打到
// 贵模型往往连续几十笔超标，逐笔推送等于轰炸；每小时提醒一次足以把人引来。
const singleAlertCooldown = time.Hour

// notifySettingsCached 是 GetNotifySettings 的 60s TTL 缓存。Settle 是
// 每请求热路径，不能每次都读 preferences；跨进程改写的陈旧窗口由 TTL 兜底
// （与 routeSnapshot 同一策略）。
func (s *Service) notifySettingsCached(ctx context.Context) (NotifySettings, error) {
	s.notifyCfgMu.Lock()
	if s.notifyCfg != nil && time.Since(s.notifyCfgAt) < 60*time.Second {
		out := *s.notifyCfg
		s.notifyCfgMu.Unlock()
		return out, nil
	}
	s.notifyCfgMu.Unlock()
	cfg, err := s.GetNotifySettings(ctx)
	if err != nil {
		return cfg, err
	}
	s.notifyCfgMu.Lock()
	s.notifyCfg, s.notifyCfgAt = &cfg, time.Now()
	s.notifyCfgMu.Unlock()
	return cfg, nil
}

// maybeNotifySingleUsage 是 Settle 结算路径的钩子：命中阈值即异步推送，
// 判定本身只做两次整数比较（设置走 60s TTL 缓存），不拖慢结算。
func (s *Service) maybeNotifySingleUsage(kid, model string, cost money.Micro, tokens int64) {
	cfg, err := s.notifySettingsCached(context.Background())
	if err != nil || !cfg.SingleCostAlert {
		return
	}
	overCost := cfg.SingleCostMicroUSD > 0 && int64(cost) >= cfg.SingleCostMicroUSD
	overTok := cfg.SingleTokenThreshold > 0 && tokens >= cfg.SingleTokenThreshold
	if !overCost && !overTok {
		return
	}
	go s.sendSingleUsageAlert(kid, model, cost, tokens)
}

// sendSingleUsageAlert 经全部启用端点推送单请求异常（按 Key 每小时限一条）。
func (s *Service) sendSingleUsageAlert(kid, model string, cost money.Micro, tokens int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !s.st.Writable() {
		return
	}
	endpoints, err := s.st.ListNotifyEndpoints(ctx)
	if err != nil {
		return
	}
	var targets []store.NotifyEndpoint
	for _, e := range endpoints {
		if e.Enabled {
			targets = append(targets, e)
		}
	}
	if len(targets) == 0 {
		return
	}
	state := map[string]int64{}
	if raw, ok, err := s.st.GetPreference(ctx, notifySingleStateKey); err == nil && ok {
		_ = json.Unmarshal([]byte(raw), &state)
	}
	now := time.Now().UTC()
	if now.Unix()-state[kid] < int64(singleAlertCooldown.Seconds()) {
		return
	}

	label := shortKID(kid)
	if k, err := s.st.GetKey(ctx, kid); err == nil && k.Label != "" {
		label = k.Label + "（" + shortKID(kid) + "）"
	}
	body := fmt.Sprintf("密钥 %s\n模型：%s\n单请求费用：$%s\n单请求计费 Token：%d\n时间：%s",
		label, model, microUSD(int64(cost)), tokens, now.Format("2006-01-02 15:04 UTC"))
	for _, e := range targets {
		if ctx.Err() != nil {
			return
		}
		url, err := s.decryptURL(e.URLEnc)
		if err != nil {
			continue
		}
		msg := ""
		if err := shoutrrrSend(url, "CPA 用量异常提醒", body); err != nil {
			msg = err.Error()
		}
		_ = s.st.UpdateNotifyEndpointResult(ctx, e.ID, now, msg)
	}
	state[kid] = now.Unix()
	if raw, err := json.Marshal(state); err == nil {
		_ = s.st.SetPreference(ctx, notifySingleStateKey, string(raw))
	}
}

// ---------- 告警扫描 ----------

const notifyStateKey = "notify_state"

// shoutrrrSend 经 shoutrrr 向单个服务 URL 发送消息；测试中可替换。
//
// 只传 title 不传 level：各服务的 Send 都严格校验参数键，level 不是
// lark/telegram/ntfy 等主流服务的合法键（会报 not a valid config key），
// 严重度改由标题文案区分。
var shoutrrrSend = func(url, title, body string) error {
	sender, err := shoutrrr.NewSenderWithOptions(
		log.New(io.Discard, "", 0),
		types.SenderOptions{HTTPClient: &http.Client{Timeout: 15 * time.Second, Transport: sharedTransport}},
		url)
	if err != nil {
		return err
	}
	params := types.Params{}
	params.SetTitle(title)
	errs := sender.Send(body, &params)
	var failed []string
	for _, e := range errs {
		if e != nil {
			failed = append(failed, e.Error())
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// ntCap 是一个参与告警评估的额度档。只收录上限为正数的档；
// 0=禁用是有意关停而非事件，不产生告警。
type ntCap struct {
	id    string // 状态标志键：usd_total / tok_daily / ...
	name  string // 人话名称：「金额总额」「Token 日限额」
	limit int64
	used  int64
	usd   bool
}

// notifyState 记录每个 Key 各档的「已通知」标志，实现边沿触发：
// 只在条件从否变是的瞬间发送一次，余量回升后自动重新武装。
type notifyState map[string]map[string]bool

// RunNotifySweep 扫描全部启用中的 Key，对越线/过期的新事件向所有启用端点发送告警。
// 返回发送的消息数。非租约持有者直接跳过（多进程下由持有者统一发送）。
func (s *Service) RunNotifySweep(ctx context.Context) (int, error) {
	if !s.st.Writable() {
		return 0, nil
	}
	cfg, err := s.GetNotifySettings(ctx)
	if err != nil {
		return 0, err
	}
	endpoints, err := s.st.ListNotifyEndpoints(ctx)
	if err != nil {
		return 0, err
	}
	hasEnabledEndpoint := false
	for _, e := range endpoints {
		if e.Enabled {
			hasEnabledEndpoint = true
			break
		}
	}
	if !cfg.Enabled || !hasEnabledEndpoint {
		return 0, nil
	}

	now := time.Now().UTC()
	keys, _, err := s.st.ListKeys(ctx, store.KeyFilter{OnlyActive: true})
	if err != nil {
		return 0, err
	}
	heldUSD, heldTok, err := s.heldBy(ctx)
	if err != nil {
		return 0, err
	}

	prev := s.loadNotifyState(ctx)
	state := notifyState{}
	type pending struct {
		kid, label string
		lines      []string
		severity   types.MessageLevel
	}
	var batch []pending
	for _, k := range keys {
		caps := notifyCaps(k, now, heldUSD[k.KID], heldTok[k.KID])
		flags := map[string]bool{}
		var lines []string
		severity := types.Warning
		for _, c := range caps {
			remain := c.limit - c.used
			exhausted := remain <= 0
			warn := !exhausted && remain*100 <= c.limit*int64(cfg.WarnPct)
			if !exhausted && !warn {
				continue
			}
			if prev[k.KID][c.id] {
				flags[c.id] = true // 条件仍成立且已通知过，不重复发
				continue
			}
			flags[c.id] = true
			pct := 0
			if c.limit > 0 {
				pct = int(remain * 100 / c.limit)
			}
			if exhausted {
				severity = types.Error
				if c.usd {
					lines = append(lines, fmt.Sprintf("%s 已用尽（$%s / $%s）", c.name, microUSD(c.used), microUSD(c.limit)))
				} else {
					lines = append(lines, fmt.Sprintf("%s 已用尽（%d / %d token）", c.name, c.used, c.limit))
				}
			} else {
				if c.usd {
					lines = append(lines, fmt.Sprintf("%s 余 $%s / $%s（%d%%，低于阈值 %d%%）", c.name, microUSD(remain), microUSD(c.limit), pct, cfg.WarnPct))
				} else {
					lines = append(lines, fmt.Sprintf("%s 余 %d / %d token（%d%%，低于阈值 %d%%）", c.name, remain, c.limit, pct, cfg.WarnPct))
				}
			}
		}
		expired := k.Expired(now)
		if expired {
			flags["expired"] = true
			if !prev[k.KID]["expired"] {
				severity = types.Error
				lines = append(lines, "密钥已过期（"+k.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")+"）")
			}
		} else if cfg.ExpireWarnDays > 0 && k.ExpiresAt != nil {
			// 临期预警：到期前 N 天推一次；过期告警接管后本档自然消失。
			days := int(time.Until(*k.ExpiresAt).Hours()/24 + 0.5)
			if days < cfg.ExpireWarnDays {
				flags["expiring"] = true
				if !prev[k.KID]["expiring"] {
					lines = append(lines, fmt.Sprintf("密钥将于 %s 到期（剩 %d 天）",
						k.ExpiresAt.UTC().Format("2006-01-02"), days))
				}
			}
		}
		state[k.KID] = flags
		if len(lines) > 0 {
			batch = append(batch, pending{kid: k.KID, label: k.Label, lines: lines, severity: severity})
		}
	}
	// 已删除/停用的 Key 不再保留状态行，避免 preferences 无限膨胀。
	alive := map[string]bool{}
	for _, k := range keys {
		alive[k.KID] = true
	}
	for kid := range state {
		if kid == "__global__" {
			continue // 全局错误率状态行不随 Key 存亡清理
		}
		if !alive[kid] {
			delete(state, kid)
		}
	}

	sent := 0
	if len(batch) > 0 {
		for _, p := range batch {
			if ctx.Err() != nil {
				break
			}
			body := fmt.Sprintf("密钥「%s」（%s）\n%s", p.label, shortKID(p.kid), strings.Join(p.lines, "\n"))
			title := "CPA 用量告警"
			if p.severity == types.Error {
				title = "CPA 用量告警（严重）"
			}
			for _, e := range endpoints {
				if !e.Enabled || ctx.Err() != nil {
					continue
				}
				url, err := s.decryptURL(e.URLEnc)
				if err != nil {
					_ = s.st.UpdateNotifyEndpointResult(ctx, e.ID, time.Now().UTC(), "URL 解密失败："+err.Error())
					continue
				}
				err = shoutrrrSend(url, title, body)
				msg := ""
				if err != nil {
					msg = err.Error()
				} else {
					sent++
				}
				_ = s.st.UpdateNotifyEndpointResult(ctx, e.ID, time.Now().UTC(), msg)
			}
		}
	}
	// 全局错误率告警：滑动窗口（分钟聚合）内失败占比超阈值，边沿触发。
	// 全局而非按 Key —— 「服务坏了」通常是上游/路由整体故障。
	if cfg.ErrorRateAlert {
		reqs, fails, ferr := s.st.RecentFailureRate(ctx, now, cfg.ErrorRateWindowMin)
		if ferr == nil && reqs > 0 {
			stateKey := "__global__"
			tripped := fails*100 >= reqs*int64(cfg.ErrorRatePct)
			if tripped && !prev[stateKey]["errrate"] {
				pct := fails * 100 / reqs
				title := "CPA 错误率告警（严重）"
				body := fmt.Sprintf("最近 %d 分钟内 %d 笔请求失败 %d 笔（%d%%，阈值 %d%%）",
					cfg.ErrorRateWindowMin, reqs, fails, pct, cfg.ErrorRatePct)
				for _, e := range endpoints {
					if !e.Enabled || ctx.Err() != nil {
						continue
					}
					if url, err := s.decryptURL(e.URLEnc); err == nil {
						if err := shoutrrrSend(url, title, body); err == nil {
							sent++
						}
					}
				}
			}
			stateEntry := map[string]bool{"errrate": tripped}
			state[stateKey] = stateEntry // 全局状态行不参与 alive 清理
		}
	}

	// 状态未变化时跳过写事务：扫描每分钟一轮，绝大多数轮次没有任何
	// 边沿事件，逐轮全量改写 preferences 是纯粹的写放大。
	if !notifyStatesEqual(prev, state) {
		if err := s.saveNotifyState(ctx, state); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// notifyStatesEqual 比较两份告警状态是否完全一致。
func notifyStatesEqual(a, b notifyState) bool {
	if len(a) != len(b) {
		return false
	}
	for kid, flags := range a {
		bFlags, ok := b[kid]
		if !ok || len(flags) != len(bFlags) {
			return false
		}
		for capID, v := range flags {
			if bFlags[capID] != v {
				return false
			}
		}
	}
	return true
}

// notifyCaps 从 Key 行构建参与评估的额度档，周期计数器跨期视为归零
// （与 analytics.go Balance 的口径一致：used = 本期已用 + 在途预占）。
func notifyCaps(k store.PluginKey, now time.Time, heldUSD, heldTok int64) []ntCap {
	cy := store.CyclesFor(now)
	cycle := func(stored, current string, v int64) int64 {
		if stored != current {
			return 0
		}
		return v
	}
	var out []ntCap
	addUSD := func(id, name string, limit *money.Micro, used int64) {
		if limit == nil || *limit <= 0 {
			return
		}
		out = append(out, ntCap{id: id, name: name, limit: int64(*limit), used: used, usd: true})
	}
	addTok := func(id, name string, limit *int64, used int64) {
		if limit == nil || *limit <= 0 {
			return
		}
		out = append(out, ntCap{id: id, name: name, limit: *limit, used: used})
	}
	spent := int64(k.SpentMicroUSD)
	addUSD("usd_total", "金额总额", k.QuotaMicroUSD, spent+heldUSD)
	addUSD("usd_daily", "金额日限额", k.DailyMicroUSD, cycle(k.DailyCycleKey, cy.Daily, int64(k.DailySpentMicroUSD))+heldUSD)
	addUSD("usd_weekly", "金额周限额", k.WeeklyMicroUSD, cycle(k.WeeklyCycleKey, cy.Weekly, int64(k.WeeklySpentMicroUSD))+heldUSD)
	addUSD("usd_monthly", "金额月限额", k.MonthlyMicroUSD, cycle(k.MonthlyCycleKey, cy.Monthly, int64(k.MonthlySpentMicroUSD))+heldUSD)
	addTok("tok_total", "Token 总额", k.TokenLimit, k.TokensUsed+heldTok)
	addTok("tok_daily", "Token 日限额", k.DailyTokenLimit, cycle(k.DailyCycleKey, cy.Daily, k.DailyTokensUsed)+heldTok)
	addTok("tok_weekly", "Token 周限额", k.WeeklyTokenLimit, cycle(k.WeeklyCycleKey, cy.Weekly, k.WeeklyTokensUsed)+heldTok)
	addTok("tok_monthly", "Token 月限额", k.MonthlyTokenLimit, cycle(k.MonthlyCycleKey, cy.Monthly, k.MonthlyTokensUsed)+heldTok)
	addTok("req_daily", "请求次数日限额", k.DailyRequestsLimit, cycle(k.DailyCycleKey, cy.Daily, k.DailyRequestsUsed))
	addTok("req_monthly", "请求次数月限额", k.MonthlyRequestsLimit, cycle(k.MonthlyCycleKey, cy.Monthly, k.MonthlyRequestsUsed))
	return out
}

// heldBy 汇总全部在途预占：key_id → (held micro-usd, reserved tokens)。
func (s *Service) heldBy(ctx context.Context) (map[string]int64, map[string]int64, error) {
	usd := map[string]int64{}
	tok := map[string]int64{}
	err := s.st.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT key_id, COALESCE(SUM(held_micro_usd),0), COALESCE(SUM(reserved_tokens),0)
			 FROM reservations WHERE status='held' GROUP BY key_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kid string
			var u, t int64
			if err := rows.Scan(&kid, &u, &t); err != nil {
				return err
			}
			usd[kid] = u
			tok[kid] = t
		}
		return rows.Err()
	})
	return usd, tok, err
}

func (s *Service) loadNotifyState(ctx context.Context) notifyState {
	raw, ok, err := s.st.GetPreference(ctx, notifyStateKey)
	if err != nil || !ok {
		return notifyState{}
	}
	var st notifyState
	if json.Unmarshal([]byte(raw), &st) != nil {
		return notifyState{}
	}
	return st
}

func (s *Service) saveNotifyState(ctx context.Context, st notifyState) error {
	if len(st) == 0 {
		_ = s.st.SetPreference(ctx, notifyStateKey, "")
		return nil
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.st.SetPreference(ctx, notifyStateKey, string(raw))
}

// shortKID 把完整 kid 缩成首尾片段，与面板芯片同风格。
func shortKID(kid string) string {
	if len(kid) <= 12 {
		return kid
	}
	return kid[:6] + "…" + kid[len(kid)-4:]
}

// microUSD 格式化 micro-USD 为美元字符串（money.Micro.USDString 的字符串入参版）。
func microUSD(v int64) string { return money.Micro(v).USDString() }
