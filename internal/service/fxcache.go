package service

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/drowsylazy/cpa-usage-manager/internal/fx"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// fxMetaKey 是汇率缓存在 meta 表中的键。
const fxMetaKey = "fx_usd_cny"

// MetaFXCache 用 meta 表实现 fx.Cache，避免 store 反向依赖 fx。
type MetaFXCache struct{ st *store.Store }

// NewMetaFXCache 构造基于 meta 表的汇率缓存。
func NewMetaFXCache(st *store.Store) *MetaFXCache { return &MetaFXCache{st: st} }

// LoadRate 读取缓存的汇率。缓存内容损坏时按「无缓存」处理。
func (c *MetaFXCache) LoadRate(ctx context.Context) (fx.Rate, bool, error) {
	if c == nil || c.st == nil {
		return fx.Rate{}, false, nil
	}
	v, ok, err := c.st.GetMeta(ctx, fxMetaKey)
	if err != nil || !ok {
		return fx.Rate{}, false, err
	}
	var r fx.Rate
	if err := json.Unmarshal([]byte(v), &r); err != nil || !r.USDToCNY.Valid() {
		return fx.Rate{}, false, nil
	}
	return r, true, nil
}

// SaveRate 写入汇率缓存。兜底值不落盘，以免掩盖真实汇率的缺失。
func (c *MetaFXCache) SaveRate(ctx context.Context, r fx.Rate) error {
	if c == nil || c.st == nil || r.Fallback {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return c.st.SetMeta(ctx, fxMetaKey, string(b))
}

// FX 返回懒初始化的汇率服务。汇率只用于面板展示，不参与结算。
func (s *Service) FX() *fx.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fxSvc == nil {
		client := &http.Client{Timeout: fx.DefaultTimeout}
		s.fxSvc = fx.NewService(NewMetaFXCache(s.st), fx.DefaultTTL, fx.DefaultProviders(client)...)
	}
	return s.fxSvc
}

// ExchangeRate 返回当前 USD→CNY 汇率（永不失败，最坏退化到兜底值）。
func (s *Service) ExchangeRate(ctx context.Context) fx.Rate {
	return s.FX().Get(ctx)
}

// RefreshExchangeRate 强制拉取一次上游汇率。
func (s *Service) RefreshExchangeRate(ctx context.Context) (fx.Rate, error) {
	return s.FX().Refresh(ctx)
}

// Preferences 返回面板偏好（主题、语言、列显隐等），由前端自定义键。
func (s *Service) Preferences(ctx context.Context) (map[string]string, error) {
	return s.st.ListPreferences(ctx)
}

// SavePreferences 批量写入面板偏好。
func (s *Service) SavePreferences(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	return s.st.SetPreferences(ctx, kv)
}

// ReleaseStale 释放超时未心跳的在途预占，返回释放条数。
// 由宿主定时调用（或面板系统页手动触发），避免僵死预占长期占额。
func (s *Service) ReleaseStale(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return s.st.ReleaseStaleReservations(ctx, now)
}
