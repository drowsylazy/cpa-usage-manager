// Package config 解析 cpa-usage-manager 的统一 YAML 配置。
//
// 加载顺序（后者覆盖前者）：
//  1. 内置默认值
//  2. 外部配置文件（内联 YAML 的 config_file，或环境变量 CPA_USAGE_MANAGER_CONFIG_FILE）
//  3. 宿主注入的内联 YAML
//
// 外部文件中的 config_file 字段被忽略，禁止嵌套递归加载。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// PluginID 是插件 ID，宿主按动态库文件名派生，此处须保持一致。
	PluginID = "cpa-usage-manager"

	// ConfigFileEnv 是指向外部配置文件的环境变量名。
	ConfigFileEnv = "CPA_USAGE_MANAGER_CONFIG_FILE"

	// DefaultPepperEnv 是存放 key pepper 的默认环境变量名。
	DefaultPepperEnv = "CPA_USAGE_MANAGER_KEY_PEPPERS"

	// DataDirPerm 是 data_dir 的权限（仅属主可读写执行）。
	DataDirPerm os.FileMode = 0o700
	// PepperFilePerm 是 pepper 文件的权限（仅属主可读写）。
	PepperFilePerm os.FileMode = 0o600
)

// 缺失 usage 时的结算策略。
const (
	MissingUsageSettleReserved = "settle_reserved"
	MissingUsageRelease        = "release"
)

// 未知模型（无计价规则命中）时的处理策略。
const (
	UnknownPolicyDeny    = "deny"
	UnknownPolicyAllow   = "allow"
	UnknownPolicyDefault = "default"
)

// Duration 包装 time.Duration，支持 "5s" / "1500ms" / "2h" 形式的 YAML 标量。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler。既接受带单位的字符串，也接受纯数字（按秒解释）。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		t := strings.TrimSpace(s)
		if t == "" {
			return fmt.Errorf("时长不能为空")
		}
		// 纯数字（无单位）按秒解释，便于书写 busy_timeout: 5。
		if v, err := time.ParseDuration(t); err == nil {
			*d = Duration(v)
			return nil
		}
		v, err := time.ParseDuration(t + "s")
		if err != nil {
			return fmt.Errorf("无法解析时长 %q：应为 5s / 1500ms / 2h 形式", s)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := value.Decode(&n); err == nil {
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	return fmt.Errorf("无法解析时长：应为 5s / 1500ms / 2h 形式")
}

// MarshalYAML 实现 yaml.Marshaler，输出带单位的字符串。
func (d Duration) MarshalYAML() (any, error) { return d.Std().String(), nil }

// Std 返回标准库时长。
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config 是插件的完整配置。
type Config struct {
	// ConfigFile 指向外部配置文件；仅在内联 YAML 中生效，文件内出现时被忽略。
	ConfigFile string `yaml:"config_file"`

	DataDir       string   `yaml:"data_dir"`
	DatabaseFile  string   `yaml:"database_file"`
	BusyTimeout   Duration `yaml:"busy_timeout"`
	RetentionDays int      `yaml:"retention_days"`

	Quota   QuotaConfig   `yaml:"quota"`
	Pricing PricingConfig `yaml:"pricing"`
	Backup  BackupConfig  `yaml:"backup"`

	ResponseCompression         bool `yaml:"response_compression"`
	ResponseCompressionMinBytes int  `yaml:"response_compression_min_bytes"`
}

// QuotaConfig 是额度子系统配置。
type QuotaConfig struct {
	Enabled bool `yaml:"enabled"`
	// CycleOffsetMinutes 是日/周/月额度周期相对 UTC 的固定偏移（分钟）：
	// 周期在「本地时间」的自然边界滚动（如 480=UTC+8，日限额在本地零点归零）。
	// 合法范围 -720..840（UTC-12:00..UTC+14:00），越界钳制；默认 0 保持 UTC。
	// cycle_key 是字符串，切换偏移后旧键自然滚动过期，无需数据迁移。
	CycleOffsetMinutes int              `yaml:"cycle_offset_minutes"`
	Keys               KeysConfig       `yaml:"keys"`
	Limits             LimitsConfig     `yaml:"limits"`
	Settlement         SettlementConfig `yaml:"settlement"`
	Stream             StreamConfig     `yaml:"stream"`
}

// KeysConfig 是插件 Key 与 pepper 体系配置。
type KeysConfig struct {
	PepperEnv      string `yaml:"pepper_env"`
	PepperFile     string `yaml:"pepper_file"`
	ActivePepperID string `yaml:"active_pepper_id"`
}

// LimitsConfig 是单请求预占上限配置。
type LimitsConfig struct {
	MaxTokenEstimate     int64 `yaml:"max_token_estimate"`
	DefaultOutputReserve int64 `yaml:"default_output_reserve"`
	RequireEstimate      bool  `yaml:"require_estimate"`
}

// SettlementConfig 是结算策略配置。
//
// HostUsageWait 只作用于**流式**结算的兜底分支：上游 SSE 未带 usage 时，
// 插件已经先关闭对客户端的流，再等宿主 usage.handle 认领窗口把权威 token 数送来，
// 因此这段等待不占用客户端时延。置 0 关闭等待（直接按预占估算结算）。
// 非流式路径不等待——宿主是在插件执行器返回**之后**才记账的，等待必然超时。
type SettlementConfig struct {
	MissingUsage  string   `yaml:"missing_usage"`
	HostUsageWait Duration `yaml:"host_usage_wait"`
}

// StreamConfig 是流式结算配置。
type StreamConfig struct {
	StaleReservationTimeout Duration `yaml:"stale_reservation_timeout"`
}

// PricingConfig 是计价配置。
type PricingConfig struct {
	UnknownPolicy string              `yaml:"unknown_policy"`
	ModelsDevSync ModelsDevSyncConfig `yaml:"models_dev_sync"`
}

// BackupConfig 是定时自动备份配置。
type BackupConfig struct {
	// Enabled 开启后每日在本地时刻 hour 点写出一份库快照到 dir 并轮转。
	Enabled bool `yaml:"enabled"`
	// Dir 是备份目录；相对路径相对 data_dir（0700，含敏感数据，不要放同步盘）。
	Dir string `yaml:"dir"`
	// Keep 是保留份数，超出删除最旧份。
	Keep int `yaml:"keep"`
	// Hour 是每日触发的本地小时（0..23）。启动时若当天时刻已过且尚未备份，
	// 会立即补一份（重启不漏当天的备份）。
	Hour int `yaml:"hour"`
}

// ModelsDevSyncConfig 是 models.dev 价格同步配置。
type ModelsDevSyncConfig struct {
	Enabled          bool              `yaml:"enabled"`
	ProviderPriority []string          `yaml:"provider_priority"`
	IgnoreSuffixes   []string          `yaml:"ignore_suffixes"`
	ModelMappings    map[string]string `yaml:"model_mappings"`
}

// Default 返回内置默认配置，与 DESIGN.md 第 6 节一致。
func Default() Config {
	return Config{
		DataDir:       filepath.FromSlash("./data/cpa-usage-manager"),
		DatabaseFile:  "cpa-usage-manager.db",
		BusyTimeout:   Duration(5 * time.Second),
		RetentionDays: 365,
		Quota: QuotaConfig{
			Enabled: true,
			Keys: KeysConfig{
				PepperEnv:      DefaultPepperEnv,
				PepperFile:     "key-peppers",
				ActivePepperID: "active",
			},
			Limits: LimitsConfig{
				MaxTokenEstimate:     1_000_000,
				DefaultOutputReserve: 4096,
				RequireEstimate:      false,
			},
			Settlement: SettlementConfig{
				MissingUsage:  MissingUsageSettleReserved,
				HostUsageWait: Duration(1500 * time.Millisecond),
			},
			Stream: StreamConfig{
				StaleReservationTimeout: Duration(2 * time.Hour),
			},
		},
		Pricing: PricingConfig{
			UnknownPolicy: UnknownPolicyAllow,
			ModelsDevSync: ModelsDevSyncConfig{
				Enabled:          true,
				ProviderPriority: nil,
				IgnoreSuffixes:   nil,
				ModelMappings:    nil,
			},
		},
		Backup: BackupConfig{
			Enabled: false,
			Dir:     "backups",
			Keep:    7,
			Hour:    4,
		},
		ResponseCompression:         true,
		ResponseCompressionMinBytes: 1024,
	}
}

// Load 按「默认值 → 外部文件 → 内联 YAML」三层解析配置。
// inline 为宿主注入的内联 YAML（可为空）。lookupEnv 为 nil 时使用 os.LookupEnv。
func Load(inline string, lookupEnv func(string) (string, bool)) (Config, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	cfg := Default()

	// 先探测内联 YAML 里的 config_file，用于决定外部文件路径。
	var probe struct {
		ConfigFile string `yaml:"config_file"`
	}
	if strings.TrimSpace(inline) != "" {
		if err := yaml.Unmarshal([]byte(inline), &probe); err != nil {
			return Config{}, fmt.Errorf("解析内联配置失败: %w", err)
		}
	}
	path := strings.TrimSpace(probe.ConfigFile)
	if path == "" {
		if v, ok := lookupEnv(ConfigFileEnv); ok {
			path = strings.TrimSpace(v)
		}
	}

	// 第二层：外部文件。
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
		}
		// 禁止嵌套递归：文件内的 config_file 不生效。
		cfg.ConfigFile = ""
	}

	// 第三层：内联 YAML 覆盖文件。yaml.v3 只写入实际出现的键，缺省键保留前一层的值。
	if strings.TrimSpace(inline) != "" {
		if err := yaml.Unmarshal([]byte(inline), &cfg); err != nil {
			return Config{}, fmt.Errorf("解析内联配置失败: %w", err)
		}
	}
	cfg.ConfigFile = path

	if err := cfg.normalize(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// normalize 规整路径与字符串字段。
func (c *Config) normalize() error {
	c.DataDir = strings.TrimSpace(c.DataDir)
	if c.DataDir == "" {
		c.DataDir = Default().DataDir
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return fmt.Errorf("解析 data_dir 绝对路径失败: %w", err)
	}
	c.DataDir = abs

	c.DatabaseFile = strings.TrimSpace(c.DatabaseFile)
	if c.DatabaseFile == "" {
		c.DatabaseFile = Default().DatabaseFile
	}

	c.Quota.Keys.PepperEnv = strings.TrimSpace(c.Quota.Keys.PepperEnv)
	if c.Quota.Keys.PepperEnv == "" {
		c.Quota.Keys.PepperEnv = DefaultPepperEnv
	}
	c.Quota.Keys.PepperFile = strings.TrimSpace(c.Quota.Keys.PepperFile)
	if c.Quota.Keys.PepperFile == "" {
		c.Quota.Keys.PepperFile = Default().Quota.Keys.PepperFile
	}
	c.Quota.Keys.ActivePepperID = strings.TrimSpace(c.Quota.Keys.ActivePepperID)
	if c.Quota.Keys.ActivePepperID == "" {
		c.Quota.Keys.ActivePepperID = Default().Quota.Keys.ActivePepperID
	}

	c.Quota.Settlement.MissingUsage = strings.ToLower(strings.TrimSpace(c.Quota.Settlement.MissingUsage))
	if c.Quota.Settlement.MissingUsage == "" {
		c.Quota.Settlement.MissingUsage = MissingUsageSettleReserved
	}
	// 周期偏移钳到 UTC-12:00..UTC+14:00，非严格 YAML 下越界值静默归一。
	if c.Quota.CycleOffsetMinutes < -720 {
		c.Quota.CycleOffsetMinutes = -720
	}
	if c.Quota.CycleOffsetMinutes > 840 {
		c.Quota.CycleOffsetMinutes = 840
	}
	// 备份目录必须是纯相对目录名或绝对路径，但不得指进 data_dir 上层之外的特殊位置；
	// 这里只归一缺省值与非法数值，路径合法性交由运行时 MkdirAll 校验。
	c.Backup.Dir = strings.TrimSpace(c.Backup.Dir)
	if c.Backup.Dir == "" {
		c.Backup.Dir = Default().Backup.Dir
	}
	if c.Backup.Keep < 1 {
		c.Backup.Keep = Default().Backup.Keep
	}
	if c.Backup.Hour < 0 || c.Backup.Hour > 23 {
		c.Backup.Hour = Default().Backup.Hour
	}
	c.Pricing.UnknownPolicy = strings.ToLower(strings.TrimSpace(c.Pricing.UnknownPolicy))
	if c.Pricing.UnknownPolicy == "" {
		c.Pricing.UnknownPolicy = UnknownPolicyAllow
	}
	return nil
}

// Validate 校验配置取值范围，返回中文错误。
func (c *Config) Validate() error {
	var errs []error

	if strings.ContainsAny(c.DatabaseFile, `/\`) {
		errs = append(errs, fmt.Errorf("database_file 必须是纯文件名，不能包含路径分隔符：%q", c.DatabaseFile))
	}
	if strings.ContainsAny(c.Quota.Keys.PepperFile, `/\`) {
		errs = append(errs, fmt.Errorf("quota.keys.pepper_file 必须是纯文件名，不能包含路径分隔符：%q", c.Quota.Keys.PepperFile))
	}
	if c.BusyTimeout.Std() <= 0 {
		errs = append(errs, fmt.Errorf("busy_timeout 必须为正，当前 %s", c.BusyTimeout.Std()))
	}
	if c.RetentionDays < 1 || c.RetentionDays > 3650 {
		errs = append(errs, fmt.Errorf("retention_days 须在 1..3650 之间，当前 %d", c.RetentionDays))
	}
	if c.Quota.Limits.MaxTokenEstimate < 1 {
		errs = append(errs, fmt.Errorf("quota.limits.max_token_estimate 必须为正，当前 %d", c.Quota.Limits.MaxTokenEstimate))
	}
	if c.Quota.Limits.DefaultOutputReserve < 0 {
		errs = append(errs, fmt.Errorf("quota.limits.default_output_reserve 不能为负，当前 %d", c.Quota.Limits.DefaultOutputReserve))
	}
	if c.Quota.Limits.DefaultOutputReserve > c.Quota.Limits.MaxTokenEstimate {
		errs = append(errs, fmt.Errorf("quota.limits.default_output_reserve(%d) 不能超过 max_token_estimate(%d)",
			c.Quota.Limits.DefaultOutputReserve, c.Quota.Limits.MaxTokenEstimate))
	}
	switch c.Quota.Settlement.MissingUsage {
	case MissingUsageSettleReserved, MissingUsageRelease:
	default:
		errs = append(errs, fmt.Errorf("quota.settlement.missing_usage 须为 %s 或 %s，当前 %q",
			MissingUsageSettleReserved, MissingUsageRelease, c.Quota.Settlement.MissingUsage))
	}
	if c.Quota.Settlement.HostUsageWait.Std() < 0 {
		errs = append(errs, fmt.Errorf("quota.settlement.host_usage_wait 不能为负，当前 %s", c.Quota.Settlement.HostUsageWait.Std()))
	}
	if c.Quota.Stream.StaleReservationTimeout.Std() <= 0 {
		errs = append(errs, fmt.Errorf("quota.stream.stale_reservation_timeout 必须为正，当前 %s", c.Quota.Stream.StaleReservationTimeout.Std()))
	}
	switch c.Pricing.UnknownPolicy {
	case UnknownPolicyDeny, UnknownPolicyAllow, UnknownPolicyDefault:
	default:
		errs = append(errs, fmt.Errorf("pricing.unknown_policy 须为 %s/%s/%s，当前 %q",
			UnknownPolicyDeny, UnknownPolicyAllow, UnknownPolicyDefault, c.Pricing.UnknownPolicy))
	}
	if c.ResponseCompressionMinBytes < 0 {
		errs = append(errs, fmt.Errorf("response_compression_min_bytes 不能为负，当前 %d", c.ResponseCompressionMinBytes))
	}
	return errors.Join(errs...)
}

// DatabasePath 返回 SQLite 数据库文件的完整路径。
func (c *Config) DatabasePath() string { return filepath.Join(c.DataDir, c.DatabaseFile) }

// PepperFilePath 返回 pepper 文件的完整路径。
func (c *Config) PepperFilePath() string {
	return filepath.Join(c.DataDir, c.Quota.Keys.PepperFile)
}

// EnsureDataDir 以 0700 创建 data_dir（幂等）。
func (c *Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir, DataDirPerm); err != nil {
		return fmt.Errorf("创建 data_dir %s 失败: %w", c.DataDir, err)
	}
	// MkdirAll 不会修正已存在目录的权限；data_dir 可能含有数据库和 pepper，
	// 因此每次启动都显式收紧到设计要求的 0700（Windows 上由系统忽略无关位）。
	if err := os.Chmod(c.DataDir, DataDirPerm); err != nil {
		return fmt.Errorf("设置 data_dir %s 权限失败: %w", c.DataDir, err)
	}
	return nil
}

// QuotaEnabled 报告额度子系统是否接管前端鉴权。
func (c *Config) QuotaEnabled() bool { return c.Quota.Enabled }
