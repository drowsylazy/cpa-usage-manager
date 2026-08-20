package service

import (
	"strconv"
	"strings"
	"time"
)

// QueryFilter 是面板与导出共用的筛选条件。
//
// 它既能从 URL query 解析（GET 接口），也能从 JSON body 解析（导出接口），
// 保证两条路径的语义完全一致。时间字段接受 RFC3339 或 Unix 毫秒。
type QueryFilter struct {
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	KeyID    string `json:"key_id,omitempty"`
	CallerID string `json:"caller_id,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Result   string `json:"result,omitempty"`
	Sort     string `json:"sort,omitempty"`
	Order    string `json:"order,omitempty"`
}

// Usage 把筛选条件转成 UsageFilter。无法解析的时间按「不限」处理。
func (q QueryFilter) Usage() UsageFilter {
	return UsageFilter{
		From:     ParseTime(q.From),
		To:       ParseTime(q.To),
		KeyID:    strings.TrimSpace(q.KeyID),
		CallerID: strings.TrimSpace(q.CallerID),
		Model:    strings.TrimSpace(q.Model),
		Provider: strings.TrimSpace(q.Provider),
		Result:   strings.TrimSpace(q.Result),
	}
}

// ParseTime 解析时间参数：RFC3339、日期（2006-01-02）或 Unix 毫秒。
// 解析失败返回零值，调用方按「不限」处理。
func ParseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC()
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		// 10 位当秒，13 位当毫秒。
		if n < 1e11 {
			return time.Unix(n, 0).UTC()
		}
		return time.UnixMilli(n).UTC()
	}
	return time.Time{}
}
