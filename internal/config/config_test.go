package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// noEnv 是一个空的环境查找函数，确保测试不受宿主环境影响。
func noEnv(string) (string, bool) { return "", false }

func TestDefaultIsValid(t *testing.T) {
	cfg, err := Load("", noEnv)
	if err != nil {
		t.Fatalf("空内联配置应可加载: %v", err)
	}
	if !cfg.Quota.Enabled {
		t.Error("quota.enabled 默认应为 true（锁定决策）")
	}
	if cfg.Pricing.UnknownPolicy != UnknownPolicyAllow {
		t.Errorf("pricing.unknown_policy 默认应为 allow，得到 %q", cfg.Pricing.UnknownPolicy)
	}
	if cfg.DatabaseFile != "cpa-usage-manager.db" {
		t.Errorf("database_file 默认值错误: %q", cfg.DatabaseFile)
	}
	if cfg.RetentionDays != 365 {
		t.Errorf("retention_days 默认值错误: %d", cfg.RetentionDays)
	}
	if cfg.BusyTimeout.Std() != 5*time.Second {
		t.Errorf("busy_timeout 默认值错误: %s", cfg.BusyTimeout.Std())
	}
	if cfg.Quota.Settlement.HostUsageWait.Std() != 1500*time.Millisecond {
		t.Errorf("host_usage_wait 默认值错误: %s", cfg.Quota.Settlement.HostUsageWait.Std())
	}
	if cfg.Quota.Stream.StaleReservationTimeout.Std() != 2*time.Hour {
		t.Errorf("stale_reservation_timeout 默认值错误: %s", cfg.Quota.Stream.StaleReservationTimeout.Std())
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Errorf("data_dir 应规整为绝对路径，得到 %q", cfg.DataDir)
	}
}

func TestInlinePartialOverrideKeepsOtherDefaults(t *testing.T) {
	// 只覆盖一个深层字段，其余同级字段必须保留默认值。
	inline := `
retention_days: 30
quota:
  limits:
    default_output_reserve: 8192
`
	cfg, err := Load(inline, noEnv)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("retention_days = %d, 期望 30", cfg.RetentionDays)
	}
	if cfg.Quota.Limits.DefaultOutputReserve != 8192 {
		t.Errorf("default_output_reserve = %d, 期望 8192", cfg.Quota.Limits.DefaultOutputReserve)
	}
	// 同一子结构里未提及的字段不得被清零。
	if cfg.Quota.Limits.MaxTokenEstimate != 1_000_000 {
		t.Errorf("max_token_estimate 被意外覆盖为 %d", cfg.Quota.Limits.MaxTokenEstimate)
	}
	if !cfg.Quota.Enabled {
		t.Error("quota.enabled 被意外覆盖为 false")
	}
	if cfg.Quota.Keys.ActivePepperID != "active" {
		t.Errorf("active_pepper_id 被意外覆盖为 %q", cfg.Quota.Keys.ActivePepperID)
	}
}

func TestQuotaDisableIsRespected(t *testing.T) {
	// 显式置 false 必须生效（这是退回纯统计模式的开关，不能被默认值吃掉）。
	cfg, err := Load("quota:\n  enabled: false\n", noEnv)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Quota.Enabled {
		t.Error("quota.enabled: false 未生效")
	}
	if cfg.QuotaEnabled() {
		t.Error("QuotaEnabled() 应返回 false")
	}
}

func TestResponseCompressionDisableIsRespected(t *testing.T) {
	cfg, err := Load("response_compression: false\n", noEnv)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.ResponseCompression {
		t.Error("response_compression: false 未生效")
	}
}

func TestFileThenInlineLayering(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cfg.yaml")
	fileYAML := `
retention_days: 100
database_file: from-file.db
quota:
  limits:
    max_token_estimate: 555000
  settlement:
    missing_usage: release
`
	if err := os.WriteFile(file, []byte(fileYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// 内联指定 config_file，并覆盖其中一项。
	inline := "config_file: " + strings.ReplaceAll(file, `\`, `/`) + "\nretention_days: 7\n"
	cfg, err := Load(inline, noEnv)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.RetentionDays != 7 {
		t.Errorf("内联应覆盖文件：retention_days = %d, 期望 7", cfg.RetentionDays)
	}
	if cfg.DatabaseFile != "from-file.db" {
		t.Errorf("文件应覆盖默认：database_file = %q", cfg.DatabaseFile)
	}
	if cfg.Quota.Limits.MaxTokenEstimate != 555000 {
		t.Errorf("文件深层字段未生效: %d", cfg.Quota.Limits.MaxTokenEstimate)
	}
	if cfg.Quota.Settlement.MissingUsage != MissingUsageRelease {
		t.Errorf("missing_usage = %q, 期望 release", cfg.Quota.Settlement.MissingUsage)
	}
}

func TestConfigFileFromEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(file, []byte("retention_days: 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == ConfigFileEnv {
			return file, true
		}
		return "", false
	}
	cfg, err := Load("", env)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.RetentionDays != 42 {
		t.Errorf("环境变量配置文件未生效: %d", cfg.RetentionDays)
	}
}

func TestInlineConfigFileWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	inlineFile := filepath.Join(dir, "inline.yaml")
	envFile := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(inlineFile, []byte("retention_days: 11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, []byte("retention_days: 22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == ConfigFileEnv {
			return envFile, true
		}
		return "", false
	}
	cfg, err := Load("config_file: "+strings.ReplaceAll(inlineFile, `\`, `/`)+"\n", env)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.RetentionDays != 11 {
		t.Errorf("内联 config_file 应优先于环境变量，retention_days = %d", cfg.RetentionDays)
	}
}

func TestNestedConfigFileIsIgnored(t *testing.T) {
	// 禁止嵌套递归：外部文件里的 config_file 不得触发二次加载。
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.yaml")
	outer := filepath.Join(dir, "outer.yaml")
	if err := os.WriteFile(inner, []byte("retention_days: 999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outerYAML := "config_file: " + strings.ReplaceAll(inner, `\`, `/`) + "\nretention_days: 8\n"
	if err := os.WriteFile(outer, []byte(outerYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("config_file: "+strings.ReplaceAll(outer, `\`, `/`)+"\n", noEnv)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.RetentionDays != 8 {
		t.Errorf("嵌套 config_file 被错误地递归加载，retention_days = %d, 期望 8", cfg.RetentionDays)
	}
}

func TestMissingConfigFileIsError(t *testing.T) {
	_, err := Load("config_file: /definitely/not/here/cfg.yaml\n", noEnv)
	if err == nil {
		t.Fatal("指定了不存在的 config_file 应当报错")
	}
	if !strings.Contains(err.Error(), "读取配置文件") {
		t.Errorf("错误信息应说明读取失败，得到: %v", err)
	}
}

func TestMalformedInlineYAML(t *testing.T) {
	if _, err := Load("quota: [this is not a map]\n", noEnv); err == nil {
		t.Fatal("类型不匹配的内联 YAML 应当报错")
	}
	if _, err := Load("\tbad: indent\n", noEnv); err == nil {
		t.Fatal("非法 YAML 应当报错")
	}
}

func TestDurationParsing(t *testing.T) {
	cases := []struct {
		yaml string
		want time.Duration
	}{
		{"busy_timeout: 3s\n", 3 * time.Second},
		{"busy_timeout: 250ms\n", 250 * time.Millisecond},
		{"busy_timeout: 1m30s\n", 90 * time.Second},
		{"busy_timeout: 2h\n", 2 * time.Hour},
		{"busy_timeout: 7\n", 7 * time.Second}, // 无单位按秒
	}
	for _, c := range cases {
		cfg, err := Load(c.yaml, noEnv)
		if err != nil {
			t.Errorf("加载 %q 失败: %v", c.yaml, err)
			continue
		}
		if cfg.BusyTimeout.Std() != c.want {
			t.Errorf("%q → %s, 期望 %s", c.yaml, cfg.BusyTimeout.Std(), c.want)
		}
	}
	if _, err := Load("busy_timeout: nonsense\n", noEnv); err == nil {
		t.Error("非法时长应报错")
	}
}

func TestValidationRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		inline string
		msg    string
	}{
		{"retention 过小", "retention_days: 0\n", "retention_days"},
		{"retention 过大", "retention_days: 4000\n", "retention_days"},
		{"busy_timeout 非正", "busy_timeout: 0s\n", "busy_timeout"},
		{"未知 unknown_policy", "pricing:\n  unknown_policy: maybe\n", "unknown_policy"},
		{"未知 missing_usage", "quota:\n  settlement:\n    missing_usage: guess\n", "missing_usage"},
		{"max_token_estimate 非正", "quota:\n  limits:\n    max_token_estimate: 0\n", "max_token_estimate"},
		{"输出预占为负", "quota:\n  limits:\n    default_output_reserve: -1\n", "default_output_reserve"},
		{"输出预占超上限", "quota:\n  limits:\n    max_token_estimate: 100\n    default_output_reserve: 200\n", "default_output_reserve"},
		{"缓冲过小", "quota:\n  stream:\n    max_buffer_bytes: 10\n", "max_buffer_bytes"},
		{"在途超时非正", "quota:\n  stream:\n    stale_reservation_timeout: 0s\n", "stale_reservation_timeout"},
		{"database_file 含路径", "database_file: sub/dir.db\n", "database_file"},
		{"pepper_file 含路径", "quota:\n  keys:\n    pepper_file: ../escape\n", "pepper_file"},
		{"压缩阈值为负", "response_compression_min_bytes: -5\n", "response_compression_min_bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(c.inline, noEnv)
			if err == nil {
				t.Fatalf("期望校验失败，却成功了")
			}
			if !strings.Contains(err.Error(), c.msg) {
				t.Errorf("错误信息应提及 %q，得到: %v", c.msg, err)
			}
		})
	}
}

func TestValidationCollectsMultipleErrors(t *testing.T) {
	_, err := Load("retention_days: 0\nbusy_timeout: 0s\n", noEnv)
	if err == nil {
		t.Fatal("期望报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "retention_days") || !strings.Contains(msg, "busy_timeout") {
		t.Errorf("应同时报告两项错误，得到: %v", err)
	}
}

func TestPolicyCaseInsensitive(t *testing.T) {
	cfg, err := Load("pricing:\n  unknown_policy: DENY\n", noEnv)
	if err != nil {
		t.Fatalf("大写策略值应被接受: %v", err)
	}
	if cfg.Pricing.UnknownPolicy != UnknownPolicyDeny {
		t.Errorf("应规整为小写 deny，得到 %q", cfg.Pricing.UnknownPolicy)
	}
}

func TestEmptyStringsFallBackToDefaults(t *testing.T) {
	inline := `
data_dir: "  "
database_file: ""
quota:
  keys:
    pepper_env: ""
    pepper_file: ""
    active_pepper_id: ""
  settlement:
    missing_usage: ""
pricing:
  unknown_policy: ""
`
	cfg, err := Load(inline, noEnv)
	if err != nil {
		t.Fatalf("空字符串应回落默认值而非报错: %v", err)
	}
	if cfg.DatabaseFile != "cpa-usage-manager.db" {
		t.Errorf("database_file 未回落: %q", cfg.DatabaseFile)
	}
	if cfg.Quota.Keys.PepperEnv != DefaultPepperEnv {
		t.Errorf("pepper_env 未回落: %q", cfg.Quota.Keys.PepperEnv)
	}
	if cfg.Quota.Keys.PepperFile != "key-peppers" {
		t.Errorf("pepper_file 未回落: %q", cfg.Quota.Keys.PepperFile)
	}
	if cfg.Quota.Keys.ActivePepperID != "active" {
		t.Errorf("active_pepper_id 未回落: %q", cfg.Quota.Keys.ActivePepperID)
	}
	if cfg.Quota.Settlement.MissingUsage != MissingUsageSettleReserved {
		t.Errorf("missing_usage 未回落: %q", cfg.Quota.Settlement.MissingUsage)
	}
	if cfg.Pricing.UnknownPolicy != UnknownPolicyAllow {
		t.Errorf("unknown_policy 未回落: %q", cfg.Pricing.UnknownPolicy)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Errorf("data_dir 未回落为绝对路径: %q", cfg.DataDir)
	}
}

func TestPathHelpers(t *testing.T) {
	dir := t.TempDir()
	inline := "data_dir: " + strings.ReplaceAll(dir, `\`, `/`) + "\n"
	cfg, err := Load(inline, noEnv)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	wantDB := filepath.Join(dir, "cpa-usage-manager.db")
	if got := cfg.DatabasePath(); got != wantDB {
		t.Errorf("DatabasePath() = %q, 期望 %q", got, wantDB)
	}
	wantPepper := filepath.Join(dir, "key-peppers")
	if got := cfg.PepperFilePath(); got != wantPepper {
		t.Errorf("PepperFilePath() = %q, 期望 %q", got, wantPepper)
	}
}

func TestEnsureDataDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "cpa")
	cfg, err := Load("data_dir: "+strings.ReplaceAll(target, `\`, `/`)+"\n", noEnv)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if err := cfg.EnsureDataDir(); err != nil {
		t.Fatalf("EnsureDataDir 失败: %v", err)
	}
	info, err := os.Stat(cfg.DataDir)
	if err != nil {
		t.Fatalf("data_dir 未创建: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("data_dir 不是目录")
	}
	// 幂等。
	if err := cfg.EnsureDataDir(); err != nil {
		t.Fatalf("重复调用应幂等: %v", err)
	}
}

func TestModelsDevSyncConfig(t *testing.T) {
	inline := `
pricing:
  models_dev_sync:
    enabled: true
    provider_priority: [anthropic, openai]
    ignore_suffixes: ["-preview", "-latest"]
    model_mappings:
      my-alias: claude-sonnet-4
`
	cfg, err := Load(inline, noEnv)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	s := cfg.Pricing.ModelsDevSync
	if !s.Enabled {
		t.Error("enabled 应为 true")
	}
	if len(s.ProviderPriority) != 2 || s.ProviderPriority[0] != "anthropic" {
		t.Errorf("provider_priority = %v", s.ProviderPriority)
	}
	if len(s.IgnoreSuffixes) != 2 || s.IgnoreSuffixes[1] != "-latest" {
		t.Errorf("ignore_suffixes = %v", s.IgnoreSuffixes)
	}
	if s.ModelMappings["my-alias"] != "claude-sonnet-4" {
		t.Errorf("model_mappings = %v", s.ModelMappings)
	}
}

func TestDesignDocExampleLoads(t *testing.T) {
	// DESIGN.md 第 6 节的完整示例必须能原样加载通过。
	inline := `
data_dir: ./data/cpa-usage-manager
database_file: cpa-usage-manager.db
busy_timeout: 5s
retention_days: 365

quota:
  enabled: true
  keys:
    pepper_env: CPA_USAGE_MANAGER_KEY_PEPPERS
    pepper_file: key-peppers
    active_pepper_id: active
  limits:
    max_token_estimate: 1000000
    default_output_reserve: 4096
    require_estimate: false
  settlement:
    missing_usage: settle_reserved
    host_usage_wait: 1500ms
  stream:
    max_buffer_bytes: 4194304
    stale_reservation_timeout: 2h

pricing:
  unknown_policy: allow
  models_dev_sync:
    enabled: true
    provider_priority: []
    ignore_suffixes: []
    model_mappings: {}

response_compression: true
response_compression_min_bytes: 1024
`
	cfg, err := Load(inline, noEnv)
	if err != nil {
		t.Fatalf("DESIGN.md 示例配置加载失败: %v", err)
	}
	if cfg.Quota.Stream.MaxBufferBytes != 4194304 {
		t.Errorf("max_buffer_bytes = %d", cfg.Quota.Stream.MaxBufferBytes)
	}
	if cfg.Quota.Limits.RequireEstimate {
		t.Error("require_estimate 应为 false")
	}
}
