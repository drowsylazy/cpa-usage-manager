// Package store 是 cpa-usage-manager 的单一 SQLite 存储层。
//
// 锁定决策：
//   - 单一数据库文件，不使用 bbolt
//   - WAL 模式 + 单写者（写连接池上限 1）+ 多读连接
//   - 跨进程写者互斥通过 writer_lease 表实现，支持 handover 租约
//   - 所有时间戳为 UTC Unix 毫秒；所有金额为整数 micro-USD
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册驱动名 "sqlite"
)

// 驱动与连接参数。
const (
	driverName = "sqlite"

	// maxReadConns 是读连接池上限。WAL 下读不阻塞写。
	// 本插件唯一读方是单用户管理面板，读并发极低；页缓存预算见 buildDSN。
	// 实际打开上限多 1，用于常驻锚定连接（见 Store.anchor）。
	maxReadConns = 4

	// retryAttempts 是瞬时故障（磁盘 I/O、忙锁、临时文件打不开）的总尝试次数。
	retryAttempts = 5

	// DefaultCallerID 是空库自动创建的默认归属。
	DefaultCallerID = "default"
)

// retryBackoff 是第 2..N 次尝试前的等待时长。
var retryBackoff = []time.Duration{
	20 * time.Millisecond,
	60 * time.Millisecond,
	150 * time.Millisecond,
	400 * time.Millisecond,
}

// 常见错误。
var (
	// ErrNotFound 表示目标记录不存在。
	ErrNotFound = errors.New("store: 记录不存在")
	// ErrReadOnly 表示本实例未持有写租约，不能执行写操作。
	ErrReadOnly = errors.New("store: 未持有写租约，当前为只读模式")
	// ErrClosed 表示 Store 已关闭。
	ErrClosed = errors.New("store: 已关闭")
	// ErrConflict 表示唯一约束冲突。
	ErrConflict = errors.New("store: 记录冲突")
	// ErrQuotaExceeded 表示总额或周期额度不足。
	ErrQuotaExceeded = errors.New("store: 额度不足")
	// ErrConcurrencyExceeded 表示在途请求数已达上限。
	ErrConcurrencyExceeded = errors.New("store: 并发额度不足")
	// ErrTransient 表示重试若干次后仍失败的瞬时故障（外部占用/忙锁/磁盘异常）。
	// 上层可据此给出可操作提示，而不必解析 SQLite 英文错误串。
	ErrTransient = errors.New("store: 数据库暂时不可用")
)

// Options 是打开 Store 的参数。
type Options struct {
	// Path 是数据库文件路径。必填。
	Path string
	// BusyTimeout 是 SQLite 忙等超时。
	BusyTimeout time.Duration
	// ReadOnly 为 true 时不获取写租约，仅供备份/只读工具使用。
	ReadOnly bool

	// OwnerID 是本实例的写者标识；为空时自动生成。
	OwnerID string
	// LeaseTTL 是写租约的有效期：心跳超过该时长即视为失效可被接管。
	LeaseTTL time.Duration
	// LeaseHeartbeat 是心跳间隔，须显著小于 LeaseTTL。
	LeaseHeartbeat time.Duration
	// HandoverWait 是请求接管时等待原持有者让出的最长时间。
	HandoverWait time.Duration

	// SkipSeed 为 true 时不写入默认 caller 与免费计价规则（测试用）。
	SkipSeed bool
}

// 租约默认参数。
const (
	DefaultLeaseTTL       = 15 * time.Second
	DefaultLeaseHeartbeat = 4 * time.Second
	DefaultHandoverWait   = 20 * time.Second
)

func (o *Options) applyDefaults() {
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = 5 * time.Second
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = DefaultLeaseTTL
	}
	if o.LeaseHeartbeat <= 0 {
		o.LeaseHeartbeat = DefaultLeaseHeartbeat
	}
	if o.LeaseHeartbeat >= o.LeaseTTL {
		// 心跳必须远快于 TTL，否则租约会在正常运行中被误判失效。
		o.LeaseHeartbeat = o.LeaseTTL / 3
		if o.LeaseHeartbeat <= 0 {
			o.LeaseHeartbeat = time.Second
		}
	}
	if o.HandoverWait <= 0 {
		o.HandoverWait = DefaultHandoverWait
	}
	if o.OwnerID == "" {
		o.OwnerID = newOwnerID()
	}
}

// Store 是存储层句柄。可并发使用。
type Store struct {
	opts Options

	// writeDB 的连接池上限为 1，实现进程内单写者。
	writeDB *sql.DB
	// readDB 允许多连接并发读。
	readDB *sql.DB
	// anchor 是一条常驻只读连接，全生命周期不归还连接池。
	//
	// SQLite 在「最后一条连接关闭」时会 checkpoint 并删除 -wal/-shm。
	// Windows 上杀毒/同步盘会短暂持有这两个文件句柄，此时的创建/删除竞态
	// 表现为 SQLITE_IOERR（面板上就是「disk I/O error」）。保持一条连接常驻
	// 可让 WAL 与 shm 在插件存活期内始终存在，从根上消除该竞态。
	anchor *sql.Conn

	// writeMu 串行化写事务。连接池上限为 1 已能保证串行，
	// 此锁额外保证「读-改-写」复合操作的原子性边界清晰。
	writeMu sync.Mutex

	// retries 累计瞬时故障重试次数，用于 health/系统页自检。
	retries atomic.Int64

	mu       sync.RWMutex
	writable bool
	closed   bool

	leaseCancel context.CancelFunc
	leaseDone   chan struct{}

	// onLeaseLost 在租约被接管时回调（用于上层告警）。
	onLeaseLost func()

	// pricingHookMu / onPricingChanged 是计价规则变更回调（服务层失效快照用）。
	pricingHookMu    sync.Mutex
	onPricingChanged func()

	// routesHookMu / onRoutesChanged 是模型路由表变更回调（服务层失效路由快照用）。
	routesHookMu    sync.Mutex
	onRoutesChanged func()
}

// execHotTx 是写事务语句执行的统一收口（热路径语句都经它执行）。
//
// 曾在此接入跨事务的预编译缓存，实测否决：database/sql 的 tx.Stmt 绑定
// 对 modernc 驱动仍会逐次重编译，收益为零；而惰性 Prepare 更会与单写
// 连接池自锁——事务占用唯一连接时，PrepareContext 取不到空闲连接直接
// 挂死。收口保留为未来绕过 database/sql 直连驱动时的唯一改动点。
func (s *Store) execHotTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, query, args...)
}

// Open 打开（必要时创建并迁移）数据库。
func Open(ctx context.Context, opts Options) (*Store, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("store: Path 不能为空")
	}
	opts.applyDefaults()

	if dir := filepath.Dir(opts.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
		}
	}

	writeDB, err := openPool(opts, true)
	if err != nil {
		return nil, err
	}
	readDB, err := openPool(opts, false)
	if err != nil {
		_ = writeDB.Close()
		return nil, err
	}

	s := &Store{opts: opts, writeDB: writeDB, readDB: readDB}

	// 锚定连接必须在迁移之前建立：迁移期间 writeDB 会独占文件，
	// 若此时读池尚无连接，任何一次读都会重新打开文件（并可能撞上外部占用）。
	if anchor, err := readDB.Conn(context.Background()); err == nil {
		s.anchor = anchor
	}

	// 迁移必须在获取租约之前完成：租约表本身由迁移创建。
	if !opts.ReadOnly {
		if err := migrate(ctx, writeDB); err != nil {
			_ = s.closePools()
			return nil, err
		}
	} else {
		// 只读模式下不迁移，但要确认 schema 已存在且版本兼容。
		if err := assertSchemaReadable(ctx, readDB); err != nil {
			_ = s.closePools()
			return nil, err
		}
	}

	if opts.ReadOnly {
		return s, nil
	}

	if err := s.acquireLease(ctx); err != nil {
		_ = s.closePools()
		return nil, err
	}
	s.setWritable(true)
	s.startHeartbeat()

	if !opts.SkipSeed {
		if err := s.seed(ctx); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

// openPool 构造一个连接池。write=true 时上限 1 连接并使用 immediate 事务锁。
func openPool(opts Options, write bool) (*sql.DB, error) {
	dsn := buildDSN(opts, write)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	if write {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		// 多 1 条用于常驻锚定连接，不挤占并发读额度。
		db.SetMaxOpenConns(maxReadConns + 1)
		db.SetMaxIdleConns(maxReadConns + 1)
	}
	// 连接长期复用：既避免反复初始化 pragma，也避免空闲回收把最后一条连接关掉
	// 触发 WAL/shm 的删除与重建（Windows 上这是 SQLITE_IOERR 的主要来源）。
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return db, nil
}

// buildDSN 构造 modernc.org/sqlite 的 DSN。
func buildDSN(opts Options, write bool) string {
	q := url.Values{}
	// busy_timeout 以毫秒为单位。
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", opts.BusyTimeout.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	// NORMAL 在 WAL 下兼顾安全与吞吐；崩溃最多丢失最近若干事务，
	// 对用量统计可接受，且额度预占本身有过期释放兜底。
	q.Add("_pragma", "synchronous(NORMAL)")
	// 排序/分组的溢出中间结果留在内存：趋势与维度聚合会 ORDER BY/GROUP BY，
	// 一旦进程 TEMP 目录不可写（权限、磁盘满、被同步盘接管）就会报 I/O 错误。
	q.Add("_pragma", "temp_store(MEMORY)")
	if write {
		// 写事务直接取写锁，避免读→写升级导致的 SQLITE_BUSY 死锁。
		q.Set("_txlock", "immediate")
		// 8MiB 页缓存，减少写放大与临时溢出。
		q.Add("_pragma", "cache_size(-8000)")
		// 放宽 WAL 自动 checkpoint 阈值（默认 1000 页 ≈ 4MB）：每分钟心跳与
		// 结算写入下，默认值会让 checkpoint 停顿更频繁地打断写事务。
		q.Add("_pragma", "wal_autocheckpoint(4000)")
		// 兜底限制 -wal 文件尺寸：checkpoint 长期无法推进时防止磁盘占用失控。
		q.Add("_pragma", "journal_size_limit(67108864)")
	} else {
		q.Add("_pragma", "query_only(1)")
		// 2MiB 页缓存/连接。cache_size 是**每连接**的：管理面读并发极低，
		// 冷页有操作系统文件缓存兜底；把常驻内存预算让给宿主进程。
		q.Add("_pragma", "cache_size(-2000)")
	}
	if opts.ReadOnly {
		q.Set("mode", "ro")
	}
	return "file:" + filepath.ToSlash(opts.Path) + "?" + q.Encode()
}

// assertSchemaReadable 在只读模式下确认 schema 存在且版本不高于本代码。
func assertSchemaReadable(ctx context.Context, db *sql.DB) error {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return fmt.Errorf("只读模式下读取 schema 版本失败（库可能尚未初始化）: %w", err)
	}
	if v.Valid && int(v.Int64) > SchemaVersion {
		return fmt.Errorf("数据库 schema 版本 %d 高于本插件支持的 %d", v.Int64, SchemaVersion)
	}
	return nil
}

// Close 释放租约并关闭连接池。幂等。
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.leaseCancel
	done := s.leaseDone
	writable := s.writable
	s.writable = false
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		// 等待心跳协程退出，确保不会在连接关闭后继续写。
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	if writable && !s.opts.ReadOnly {
		// 主动让出租约，使后继实例无需等待 TTL 过期。
		ctx, cancelRel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.releaseLease(ctx)
		cancelRel()
	}
	return s.closePools()
}

func (s *Store) closePools() error {
	var errs []error
	if s.anchor != nil {
		if err := s.anchor.Close(); err != nil {
			errs = append(errs, fmt.Errorf("关闭锚定连接: %w", err))
		}
		s.anchor = nil
	}
	if s.readDB != nil {
		if err := s.readDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("关闭读连接池: %w", err))
		}
	}
	if s.writeDB != nil {
		if err := s.writeDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("关闭写连接池: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Writable 报告本实例当前是否持有写租约。
func (s *Store) Writable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writable && !s.closed
}

// OwnerID 返回本实例的写者标识。
func (s *Store) OwnerID() string { return s.opts.OwnerID }

// SetLeaseLostHandler 注册租约丢失时的回调（在心跳协程中调用，须快速返回）。
func (s *Store) SetLeaseLostHandler(fn func()) {
	s.mu.Lock()
	s.onLeaseLost = fn
	s.mu.Unlock()
}

func (s *Store) setWritable(v bool) {
	s.mu.Lock()
	s.writable = v
	s.mu.Unlock()
}

// ReadDB 暴露只读连接池，供同包内查询构造器使用。
func (s *Store) ReadDB() *sql.DB { return s.readDB }

// Write 在写事务中执行 fn。事务使用 immediate 锁，串行执行。
// fn 返回错误则回滚。未持有写租约时返回 ErrReadOnly。
// 瞬时故障（杀毒/同步盘短暂占用库文件、忙锁）按退避重试，见 retryTransient。
func (s *Store) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.RLock()
	closed, writable := s.closed, s.writable
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if !writable {
		return ErrReadOnly
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	return s.retryTransient(ctx, func() error { return writeOnce(ctx, s.writeDB, fn) })
}

func writeOnce(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启写事务失败: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交写事务失败: %w", err)
	}
	return nil
}

// Read 在只读连接上执行 fn。瞬时故障按退避重试，见 retryTransient。
func (s *Store) Read(ctx context.Context, fn func(q Querier) error) error {
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	return s.retryTransient(ctx, func() error { return fn(s.readDB) })
}

// retryTransient 在瞬时故障下按退避重试 fn，重试耗尽后把原始错误包进
// ErrTransient 并附上可操作的成因说明。
//
// 单次重试不足以覆盖真实场景：杀毒软件实时扫描一个刚写入的 -wal 文件
// 通常持有数十到数百毫秒，同步盘的上传窗口更长。
func (s *Store) retryTransient(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if attempt > 0 {
			s.retries.Add(1)
			wait := retryBackoff[minInt(attempt-1, len(retryBackoff)-1)]
			select {
			case <-ctx.Done():
				return err
			case <-time.After(wait):
			}
		}
		err = fn()
		if !isTransientErr(err) {
			return err
		}
	}
	return fmt.Errorf("%w（%s；已重试 %d 次）: %w",
		ErrTransient, transientCause(err), retryAttempts-1, err)
}

// Retries 返回累计的瞬时故障重试次数（进程内计数，重开库后归零）。
func (s *Store) Retries() int64 { return s.retries.Load() }

// isTransientErr 报告错误是否值得重试。
//
// Windows 上杀毒软件或同步盘短暂占用库文件时会得到 SQLITE_IOERR 系列
// 扩展码（如 522 SHORT_READ）；跨进程写者交接、宿主热重载会短暂出现忙锁；
// 临时目录不可用会报 "unable to open database file"。这些稍候重试通常即可恢复。
// 「database disk image is malformed」不在其列：重试不会让损坏的库变好。
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, needle := range []string{
		"disk I/O error",
		"SQLITE_IOERR",
		"database is locked",
		"database table is locked",
		"SQLITE_BUSY",
		"SQLITE_LOCKED",
		"unable to open database file",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// transientCause 把 SQLite 的英文错误映射为面向用户的成因说明。
func transientCause(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "locked") || strings.Contains(msg, "SQLITE_BUSY"):
		return "数据库被其他进程长时间占用；检查是否有第二个宿主实例共用同一 data_dir"
	case strings.Contains(msg, "unable to open database file"):
		return "无法打开数据库或临时文件；检查 data_dir 权限、磁盘剩余空间与 TEMP 目录"
	default:
		return "数据目录可能被杀毒软件/同步盘占用，或磁盘空间异常；建议把 data_dir 加入杀毒软件排除列表，并移出同步盘"
	}
}

// IsCorrupted 报告错误是否指向数据库文件损坏（重试无用，需备份后重建）。
func IsCorrupted(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database disk image is malformed") ||
		strings.Contains(msg, "SQLITE_CORRUPT") ||
		strings.Contains(msg, "file is not a database")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Querier 抽象 *sql.DB 与 *sql.Tx 的公共查询接口。
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// seed 在空库中写入默认归属与全模型免费计价规则。
//
// 锁定约定：自动建 default caller 与免费规则，但绝不自动签发任何 Key。
func (s *Store) seed(ctx context.Context) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		now := nowMillis()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO callers (id, display_name, enabled, created_at, updated_at)
			 VALUES (?, ?, 1, ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
			DefaultCallerID, "默认归属", now, now); err != nil {
			return fmt.Errorf("写入默认 caller 失败: %w", err)
		}
		// 全模型免费兜底规则：优先级最低，任何显式规则都能覆盖它。
		// 它的存在让 unknown_policy=allow 时未配价模型不至于报错。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pricing_rules
			   (match_kind, pattern, priority, enabled, source, created_at, updated_at)
			 VALUES ('glob', '*', ?, 1, 'manual', ?, ?)
			 ON CONFLICT(match_kind, pattern) DO NOTHING`,
			fallbackRulePriority, now, now); err != nil {
			return fmt.Errorf("写入兜底计价规则失败: %w", err)
		}
		return nil
	})
}

// fallbackRulePriority 是兜底免费规则的优先级，取足够小的值确保总是最后匹配。
const fallbackRulePriority = -1 << 30

// KV 读写 -----------------------------------------------------------------

// GetMeta 读取 meta 表中的值。不存在时返回 ok=false。
func (s *Store) GetMeta(ctx context.Context, key string) (string, bool, error) {
	return s.getKV(ctx, "meta", key)
}

// SetMeta 写入 meta 表。
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.setKV(ctx, "meta", key, value)
}

// GetPreference 读取面板偏好。
func (s *Store) GetPreference(ctx context.Context, key string) (string, bool, error) {
	return s.getKV(ctx, "preferences", key)
}

// SetPreference 写入面板偏好。
func (s *Store) SetPreference(ctx context.Context, key, value string) error {
	return s.setKV(ctx, "preferences", key, value)
}

// ListPreferences 返回全部面板偏好。
func (s *Store) ListPreferences(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string)
	err := s.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT k, v FROM preferences ORDER BY k`)
		if err != nil {
			return fmt.Errorf("读取偏好失败: %w", err)
		}
		defer rows.Close()
		clear(out)
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return fmt.Errorf("扫描偏好失败: %w", err)
			}
			out[k] = v
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("遍历偏好失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetPreferences 批量写入面板偏好。
func (s *Store) SetPreferences(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO preferences (k, v) VALUES (?, ?)
			 ON CONFLICT(k) DO UPDATE SET v = excluded.v`)
		if err != nil {
			return fmt.Errorf("准备偏好写入失败: %w", err)
		}
		defer stmt.Close()
		for k, v := range kv {
			if _, err := stmt.ExecContext(ctx, k, v); err != nil {
				return fmt.Errorf("写入偏好 %s 失败: %w", k, err)
			}
		}
		return nil
	})
}

// getKV 从指定 KV 表读取。table 只能是包内常量，不接受外部输入。
func (s *Store) getKV(ctx context.Context, table, key string) (string, bool, error) {
	var v string
	err := s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT v FROM `+table+` WHERE k = ?`, key).Scan(&v)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取 %s[%s] 失败: %w", table, key, err)
	}
	return v, true, nil
}

func (s *Store) setKV(ctx context.Context, table, key, value string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO `+table+` (k, v) VALUES (?, ?)
			 ON CONFLICT(k) DO UPDATE SET v = excluded.v`, key, value)
		if err != nil {
			return fmt.Errorf("写入 %s[%s] 失败: %w", table, key, err)
		}
		return nil
	})
}

// 维护操作 ---------------------------------------------------------------

// RetentionResult 汇报一次保留清理删除的行数。
// Deduped 是同一次维护里被合并掉的历史重复请求行数（见 DedupeRequests）。
type RetentionResult struct {
	Requests     int64 `json:"requests"`
	Rollups      int64 `json:"rollups"`
	Reservations int64 `json:"reservations"`
	Deduped      int   `json:"deduped"`
}

// ApplyRetention 按保留天数清理 requests 与 usage_rollups，
// 并回收早已终结的 reservations。plugin_keys / callers /
// pricing_rules / audit_events 长期保留，不在此清理。
func (s *Store) ApplyRetention(ctx context.Context, retentionDays int, now time.Time) (RetentionResult, error) {
	if retentionDays < 1 {
		return RetentionResult{}, fmt.Errorf("retention_days 须为正，得到 %d", retentionDays)
	}
	cutoff := now.UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	cutoffMillis := cutoff.UnixMilli()
	cutoffMinute := cutoff.Unix() / 60

	var res RetentionResult
	// 大保留期调整（如首次启用或天数骤减）可能面对数十万行待删数据。删除
	// 按批拆成多个独立写事务：单个巨型 DELETE 会把唯一写连接占满整个事务
	// 期，阻塞 beat 与一切写路径并推高 WAL；分批后每批之间读池与心跳都有
	// 机会插进来。保留期清理幂等，中断后下次 Maintain 会继续删完。
	var err error
	res.Requests, err = s.deleteBatchesTx(ctx,
		`DELETE FROM requests WHERE id IN (SELECT id FROM requests WHERE ts < ? LIMIT ?)`,
		cutoffMillis)
	if err != nil {
		return res, fmt.Errorf("清理 requests 失败: %w", err)
	}

	// usage_rollups 是 WITHOUT ROWID 表，按复合主键元组分批。
	res.Rollups, err = s.deleteBatchesTx(ctx,
		`DELETE FROM usage_rollups WHERE (bucket_minute, model, key_id, caller_id, provider, source, auth_type, tier, result) IN
			(SELECT bucket_minute, model, key_id, caller_id, provider, source, auth_type, tier, result FROM usage_rollups WHERE bucket_minute < ? LIMIT ?)`,
		cutoffMinute)
	if err != nil {
		return res, fmt.Errorf("清理 usage_rollups 失败: %w", err)
	}

	// 已结算/已释放且早于截止时间的预占没有查询价值，一并回收。
	res.Reservations, err = s.deleteBatchesTx(ctx,
		`DELETE FROM reservations WHERE id IN (SELECT id FROM reservations WHERE status IN ('settled','released') AND created_at < ? LIMIT ?)`,
		cutoffMillis)
	if err != nil {
		return res, fmt.Errorf("清理 reservations 失败: %w", err)
	}
	return res, nil
}

// retentionBatchSize 是保留期清理的每批删除行数，在语句代价与事务时长
// 之间折中：每批一个独立写事务，短事务不阻塞并发路径。
const retentionBatchSize = 20000

// deleteBatchesTx 循环执行带 LIMIT 子查询的批量删除，每批一个独立写事务，
// 返回累计删除行数。query 必须含恰好两个占位符：删除谓词参数与批大小。
func (s *Store) deleteBatchesTx(ctx context.Context, query string, arg any) (int64, error) {
	var total int64
	for {
		var n int64
		err := s.Write(ctx, func(tx *sql.Tx) error {
			r, e := tx.ExecContext(ctx, query, arg, retentionBatchSize)
			if e != nil {
				return e
			}
			n, _ = r.RowsAffected()
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < retentionBatchSize {
			return total, nil
		}
	}
}

// Vacuum 执行 VACUUM 回收空间。耗时较长，应由维护接口显式触发。
func (s *Store) Vacuum(ctx context.Context) error {
	if !s.Writable() {
		return ErrReadOnly
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// VACUUM 不能在事务中执行。
	if _, err := s.writeDB.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("VACUUM 失败: %w", err)
	}
	return nil
}

// Checkpoint 把 WAL 内容合并回主库文件，备份前调用可保证单文件自洽。
func (s *Store) Checkpoint(ctx context.Context) error {
	if !s.Writable() {
		return ErrReadOnly
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.writeDB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("WAL checkpoint 失败: %w", err)
	}
	return nil
}

// Stats 汇报库的基本规模，用于面板系统页与 health 接口。
type Stats struct {
	SchemaVersion int   `json:"schema_version"`
	Keys          int64 `json:"keys"`
	Callers       int64 `json:"callers"`
	PricingRules  int64 `json:"pricing_rules"`
	Requests      int64 `json:"requests"`
	Rollups       int64 `json:"rollups"`
	AuditEvents   int64 `json:"audit_events"`
	HeldReserves  int64 `json:"held_reservations"`
	FileBytes     int64 `json:"file_bytes"`
	Writable      bool  `json:"writable"`
	// IORetries 是本进程累计的瞬时故障重试次数。持续增长说明数据目录
	// 正被外部程序（杀毒/同步盘）干扰，即使请求最终成功也值得处理。
	IORetries int64 `json:"io_retries"`
}

// Stats 读取库规模统计。
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	out := Stats{Writable: s.Writable(), IORetries: s.Retries()}
	v, err := s.CurrentSchemaVersion(ctx)
	if err != nil {
		return Stats{}, err
	}
	out.SchemaVersion = v

	// 七个计数合并为一条查询单次往返：系统页一次打开不再付七次读池调度
	// 与七次全表扫描的往返开销（各 COUNT 仍分别为一次全表扫描，代价不变）。
	err = s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM plugin_keys),
			(SELECT COUNT(*) FROM callers),
			(SELECT COUNT(*) FROM pricing_rules),
			(SELECT COUNT(*) FROM requests),
			(SELECT COUNT(*) FROM usage_rollups),
			(SELECT COUNT(*) FROM audit_events),
			(SELECT COUNT(*) FROM reservations WHERE status = 'held')`).
			Scan(&out.Keys, &out.Callers, &out.PricingRules, &out.Requests,
				&out.Rollups, &out.AuditEvents, &out.HeldReserves)
	})
	if err != nil {
		return Stats{}, fmt.Errorf("统计失败: %w", err)
	}
	if fi, err := os.Stat(s.opts.Path); err == nil {
		out.FileBytes = fi.Size()
	}
	// 重试计数取最新值：上面的统计本身也可能触发重试。
	out.IORetries = s.Retries()
	return out, nil
}

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.opts.Path }

// 时间工具 ---------------------------------------------------------------

// nowMillis 返回当前 UTC Unix 毫秒。
func nowMillis() int64 { return time.Now().UTC().UnixMilli() }

// BucketMinute 把时间归一到 UTC Unix 分钟数（分钟聚合的桶键）。
func BucketMinute(t time.Time) int64 { return t.UTC().Unix() / 60 }

// MinuteTime 把分钟桶键还原为时间。
func MinuteTime(bucket int64) time.Time { return time.Unix(bucket*60, 0).UTC() }

// newOwnerID 生成本实例的写者标识：主机名 + 进程号 + 启动时间。
func newOwnerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d/%d/%s", host, os.Getpid(), time.Now().UTC().UnixNano(), runtime.GOOS)
}

// isUniqueViolation 判断错误是否为唯一约束冲突。
// modernc 驱动不导出错误码常量，按错误文本判定（与 CGO 版行为一致）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
