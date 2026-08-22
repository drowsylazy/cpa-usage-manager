// Package service 内的 models.dev 价格同步实现。
//
// models.dev 提供一份公开的 JSON 价格簿（无需密钥）。同步流程：
// 拉取 → 按 provider_priority / ignore_suffixes / model_mappings 变换
// → 生成 exact 规则 → 交给 store.ReplaceModelsDevRules 合并。
// 手工规则永不被同步覆盖，这一保证在 store 层。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/money"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// ModelsDevURL 是默认的价格簿地址。
const ModelsDevURL = "https://models.dev/api.json"

// modelsDevMaxBytes 限制单次拉取的响应体大小（价格簿约数 MB 量级）。
const modelsDevMaxBytes = 32 << 20

// modelsDevMetaKey 是记录上次同步结果的 meta 键。
const modelsDevMetaKey = "models_dev_sync"

// ModelsDevProvider 是价格簿里的一个提供方。
type ModelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Models map[string]ModelsDevModel `json:"models"`
}

// ModelsDevModel 是价格簿里的一个模型。价格字段一律以 json.Number
// 承载，交由 money 包做纯整数解析，避免 float64 引入精度误差。
type ModelsDevModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Cost struct {
		Input       json.Number `json:"input"`
		Output      json.Number `json:"output"`
		Reasoning   json.Number `json:"reasoning"`
		CacheRead   json.Number `json:"cache_read"`
		CacheWrite  json.Number `json:"cache_write"`
		InputCached json.Number `json:"input_cached"`
	} `json:"cost"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
}

// ModelsDevSyncResult 汇报一次同步的结果。
type ModelsDevSyncResult struct {
	Fetched   int       `json:"fetched"`
	Candidate int       `json:"candidate"`
	Applied   int       `json:"applied"`
	Skipped   int       `json:"skipped"`
	Removed   int       `json:"removed"`
	Providers []string  `json:"providers"`
	Source    string    `json:"source"`
	At        time.Time `json:"at"`
	// Warnings 收集无法解析的条目，不阻断整体同步。
	Warnings []string `json:"warnings,omitempty"`
}

// ModelsDevSyncer 拉取并应用 models.dev 价格。
type ModelsDevSyncer struct {
	URL    string
	Client *http.Client
}

// NewModelsDevSyncer 构造同步器。client 为 nil 时使用带超时的默认客户端。
func NewModelsDevSyncer(url string, client *http.Client) *ModelsDevSyncer {
	if strings.TrimSpace(url) == "" {
		url = ModelsDevURL
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &ModelsDevSyncer{URL: url, Client: client}
}

// Fetch 拉取原始价格簿。
func (m *ModelsDevSyncer) Fetch(ctx context.Context) (map[string]ModelsDevProvider, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取 models.dev 价格簿失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取 models.dev 价格簿失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 models.dev 响应失败: %w", err)
	}
	return decodeModelsDev(body)
}

// decodeModelsDev 解析价格簿 JSON。
func decodeModelsDev(body []byte) (map[string]ModelsDevProvider, error) {
	var out map[string]ModelsDevProvider
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析 models.dev 价格簿失败: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("models.dev 价格簿为空")
	}
	return out, nil
}

// SyncModelsDev 执行一次完整同步：拉取 → 变换 → 合并入库 → 写审计与 meta。
func (s *Service) SyncModelsDev(ctx context.Context, syncer *ModelsDevSyncer, actor string) (ModelsDevSyncResult, error) {
	cfg := s.cfg.Pricing.ModelsDevSync
	if !cfg.Enabled {
		return ModelsDevSyncResult{}, errors.New("models.dev 同步已在配置中关闭（pricing.models_dev_sync.enabled=false）")
	}
	if syncer == nil {
		syncer = NewModelsDevSyncer("", nil)
	}
	raw, err := syncer.Fetch(ctx)
	if err != nil {
		return ModelsDevSyncResult{}, err
	}
	rules, res := transformModelsDev(raw, cfg.ProviderPriority, cfg.IgnoreSuffixes, cfg.ModelMappings)
	res.Source = syncer.URL

	now := time.Now().UTC()
	applied, skipped, removed, err := s.st.ReplaceModelsDevRules(ctx, rules, now)
	if err != nil {
		return ModelsDevSyncResult{}, err
	}
	res.Applied, res.Skipped, res.Removed, res.At = applied, skipped, removed, now

	if b, mErr := json.Marshal(res); mErr == nil {
		// meta 写失败不影响同步结果本身。
		_ = s.st.SetMeta(ctx, modelsDevMetaKey, string(b))
	}
	_ = s.st.AppendAudit(ctx, store.AuditEvent{
		Actor: actor, Action: "pricing.sync", EntityType: "pricing", EntityID: "models_dev",
		Detail: map[string]any{
			"applied": applied, "skipped": skipped, "removed": removed,
			"candidate": res.Candidate, "providers": res.Providers,
		},
	})
	return res, nil
}

// LastModelsDevSync 读取上次同步的结果摘要。
func (s *Service) LastModelsDevSync(ctx context.Context) (ModelsDevSyncResult, bool, error) {
	v, ok, err := s.st.GetMeta(ctx, modelsDevMetaKey)
	if err != nil || !ok {
		return ModelsDevSyncResult{}, false, err
	}
	var out ModelsDevSyncResult
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return ModelsDevSyncResult{}, false, nil
	}
	return out, true, nil
}

// ModelsDevCandidate 是搜索结果里的一条可添加模型，字段与
// store.PricingRule 的 JSON 契约对齐，前端可直接作为 POST /pricing 的请求体。
type ModelsDevCandidate struct {
	ProviderID         string      `json:"provider_id"`
	ProviderName       string      `json:"provider_name"`
	ModelID            string      `json:"model_id"`
	Name               string      `json:"name"`
	Pattern            string      `json:"pattern"`
	Label              string      `json:"label,omitempty"`
	PriceInput         money.Price `json:"price_input"`
	PriceOutput        money.Price `json:"price_output"`
	PriceCacheRead     money.Price `json:"price_cache_read"`
	PriceCacheCreation money.Price `json:"price_cache_creation"`
	MatchKind          string      `json:"match_kind"`
	Source             string      `json:"source"`
	ModelsDevID        string      `json:"models_dev_id"`
}

const modelsDevCatalogTTL = 10 * time.Minute

// catalog 返回价格簿目录，TTL 内复用缓存，避免每次搜索都整本拉取。
func (s *Service) catalog(ctx context.Context, syncer *ModelsDevSyncer) (map[string]ModelsDevProvider, error) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if s.catalogRaw != nil && time.Since(s.catalogAt) < modelsDevCatalogTTL {
		return s.catalogRaw, nil
	}
	if syncer == nil {
		syncer = NewModelsDevSyncer("", nil)
	}
	raw, err := syncer.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	s.catalogRaw, s.catalogAt = raw, time.Now()
	return raw, nil
}

// SearchModelsDev 按关键词在 models.dev 目录中查找模型，返回可直接添加的计价候选。
// 只读操作，不写库、不受 pricing.models_dev_sync.enabled 开关限制。
func (s *Service) SearchModelsDev(ctx context.Context, syncer *ModelsDevSyncer, query string, limit int) ([]ModelsDevCandidate, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return []ModelsDevCandidate{}, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	raw, err := s.catalog(ctx, syncer)
	if err != nil {
		return nil, err
	}
	type hit struct {
		c     ModelsDevCandidate
		score int
	}
	var hits []hit
	for pid, p := range raw {
		for mid, m := range p.Models {
			id := strings.TrimSpace(mid)
			if id == "" {
				id = strings.TrimSpace(m.ID)
			}
			if id == "" {
				continue
			}
			hay := strings.ToLower(pid + "/" + id + " " + m.Name + " " + p.Name + " " + p.ID)
			idx := strings.Index(hay, q)
			if idx < 0 {
				continue
			}
			rule, rErr := ruleForModel(pid, id, m)
			if rErr != nil {
				continue
			}
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = id
			}
			hits = append(hits, hit{
				c: ModelsDevCandidate{
					ProviderID:         pid,
					ProviderName:       p.Name,
					ModelID:            id,
					Name:               name,
					Pattern:            id,
					Label:              name,
					PriceInput:         rule.PriceInput,
					PriceOutput:        rule.PriceOutput,
					PriceCacheRead:     rule.PriceCacheRead,
					PriceCacheCreation: rule.PriceCacheCreation,
					MatchKind:          "exact",
					Source:             string(store.PricingSourceModelsDev),
					ModelsDevID:        pid + "/" + id,
				},
				score: idx,
			})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score < hits[j].score
		}
		return hits[i].c.ModelsDevID < hits[j].c.ModelsDevID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]ModelsDevCandidate, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.c)
	}
	return out, nil
}

// transformModelsDev 把价格簿变换为计价规则。
//
//   - providerPriority 决定同名模型的取用顺序；未列出的提供方排在其后，按 ID 字典序。
//   - ignoreSuffixes 命中的模型 ID 直接跳过。
//   - mappings 形如 {本地模型名: models.dev 模型 ID}，为本地改名的模型补一条同价规则。
func transformModelsDev(raw map[string]ModelsDevProvider, providerPriority, ignoreSuffixes []string,
	mappings map[string]string) ([]store.PricingRule, ModelsDevSyncResult) {

	res := ModelsDevSyncResult{}
	order := providerOrder(raw, providerPriority)
	res.Providers = order

	// modelID → 已选中的规则。先到先得，因此 order 决定优先级。
	chosen := make(map[string]store.PricingRule)
	// modelID → 该模型在 models.dev 上的规范 ID，供 mappings 复用。
	byDevID := make(map[string]store.PricingRule)

	for _, providerID := range order {
		p := raw[providerID]
		for modelID, m := range p.Models {
			res.Fetched++
			id := strings.TrimSpace(modelID)
			if id == "" {
				id = strings.TrimSpace(m.ID)
			}
			if id == "" {
				continue
			}
			if hasAnySuffix(id, ignoreSuffixes) {
				continue
			}
			rule, err := ruleForModel(providerID, id, m)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s/%s: %v", providerID, id, err))
				continue
			}
			if _, exists := byDevID[id]; !exists {
				byDevID[id] = rule
			}
			if _, exists := chosen[id]; exists {
				continue
			}
			chosen[id] = rule
		}
	}

	// 显式映射：本地模型名 → models.dev 模型 ID。
	for local, devID := range mappings {
		local = strings.TrimSpace(local)
		devID = strings.TrimSpace(devID)
		if local == "" || devID == "" {
			continue
		}
		src, ok := byDevID[devID]
		if !ok {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("映射 %s → %s：models.dev 中没有该模型", local, devID))
			continue
		}
		mapped := src
		mapped.Pattern = local
		mapped.ModelsDevID = devID
		// 显式映射优先级高于自动生成的同名规则。
		mapped.Priority = 10
		chosen[local] = mapped
	}

	out := make([]store.PricingRule, 0, len(chosen))
	for _, r := range chosen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	res.Candidate = len(out)
	return out, res
}

// ruleForModel 把一个模型条目转成一条 exact 计价规则。
func ruleForModel(providerID, modelID string, m ModelsDevModel) (store.PricingRule, error) {
	input, err := priceFromNumber(m.Cost.Input)
	if err != nil {
		return store.PricingRule{}, fmt.Errorf("input 价格无法解析: %w", err)
	}
	output, err := priceFromNumber(m.Cost.Output)
	if err != nil {
		return store.PricingRule{}, fmt.Errorf("output 价格无法解析: %w", err)
	}
	// models.dev 的 reasoning 价格本插件不落库：推理 token 由 usageparse.Billable()
	// 并入输出按输出价计（引擎无独立推理档），存了也不会参与计算。绝大多数提供方
	// 二者同价；若某模型确实不同价，这里会按输出价计（既有限制，非本次引入）。
	cacheRead, err := priceFromNumber(m.Cost.CacheRead)
	if err != nil {
		return store.PricingRule{}, fmt.Errorf("cache_read 价格无法解析: %w", err)
	}
	cacheWrite, err := priceFromNumber(m.Cost.CacheWrite)
	if err != nil {
		return store.PricingRule{}, fmt.Errorf("cache_write 价格无法解析: %w", err)
	}
	cached, err := priceFromNumber(m.Cost.InputCached)
	if err != nil {
		return store.PricingRule{}, fmt.Errorf("input_cached 价格无法解析: %w", err)
	}
	// 缓存读价的取值顺序：cache_read（Claude 口径）→ input_cached（OpenAI 口径）
	// → 输入价兜底。input_cached 原先落进从不参与计算的 price_cached，等于被丢弃，
	// OpenAI 系模型的缓存读会按输入价高估；现在归到缓存读档，计价更准。
	if cacheRead == 0 && cached > 0 {
		cacheRead = cached
	}
	// 两个字段都没给时按输入价计（多数提供方缓存读更便宜，取输入价是保守侧）。
	if cacheRead == 0 && input > 0 && m.Cost.CacheRead.String() == "" {
		cacheRead = input
	}

	return store.PricingRule{
		MatchKind:          store.MatchExact,
		Pattern:            modelID,
		Priority:           0,
		Enabled:            true,
		PriceInput:         input,
		PriceOutput:        output,
		PriceCacheRead:     cacheRead,
		PriceCacheCreation: cacheWrite,
		AccountingMode:     store.AccountingModeDefault,
		BillingMode:        store.BillingModeToken,
		Source:             store.PricingSourceModelsDev,
		ModelsDevID:        providerID + "/" + modelID,
	}, nil
}

// priceFromNumber 把「每百万 token 的 USD」十进制文本转为 money.Price。
// 空值视为 0（免费或未标价）。
func priceFromNumber(n json.Number) (money.Price, error) {
	t := strings.TrimSpace(n.String())
	if t == "" || t == "null" {
		return 0, nil
	}
	p, err := money.PriceFromUSDPerMillion(t)
	if err != nil {
		return 0, err
	}
	if p < 0 {
		return 0, fmt.Errorf("价格为负: %s", t)
	}
	return p, nil
}

// providerOrder 计算提供方取用顺序：配置里列出的在前（保持配置顺序），
// 其余按 ID 字典序排在后面。
func providerOrder(raw map[string]ModelsDevProvider, priority []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range priority {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if _, ok := raw[id]; !ok {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	rest := make([]string, 0, len(raw))
	for id := range raw {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// hasAnySuffix 报告 s 是否以 suffixes 中任意一项结尾（大小写不敏感）。
func hasAnySuffix(s string, suffixes []string) bool {
	if len(suffixes) == 0 {
		return false
	}
	low := strings.ToLower(s)
	for _, suffix := range suffixes {
		t := strings.ToLower(strings.TrimSpace(suffix))
		if t != "" && strings.HasSuffix(low, t) {
			return true
		}
	}
	return false
}
