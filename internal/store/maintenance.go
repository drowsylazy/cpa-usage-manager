package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// snapshotTables 是备份/恢复涉及的业务表，按外键依赖顺序排列。
//
// writer_lease 与 schema_migrations 属于本实例运行时状态，不参与恢复：
// 恢复时保留本进程自己的租约与迁移记录。
var snapshotTables = []string{
	"callers",
	"plugin_keys",
	"pricing_rules",
	"reservations",
	"requests",
	"usage_rollups",
	"audit_events",
	"model_routes",
	"notify_endpoints",
	"report_configs",
	"preferences",
	"meta",
}

// BackupOptions 控制备份行为。
type BackupOptions struct {
	// MaxBytes 是允许导出的最大字节数；超出时返回错误。0 表示不限。
	MaxBytes int64
}

// BackupResult 汇报一次备份的规模。
type BackupResult struct {
	Bytes int64     `json:"bytes"`
	At    time.Time `json:"at"`
}

// BackupTo 把数据库以单文件形式写入 w。
//
// 写入前执行 WAL checkpoint(TRUNCATE)，保证主库文件自洽；随后在持有
// writeMu 的情况下复制文件，期间没有新的写事务，因此快照一致。
func (s *Store) BackupTo(ctx context.Context, w io.Writer, opts BackupOptions) (BackupResult, error) {
	if s.isClosed() {
		return BackupResult{}, ErrClosed
	}
	if err := s.Checkpoint(ctx); err != nil {
		return BackupResult{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	f, err := os.Open(s.opts.Path)
	if err != nil {
		return BackupResult{}, fmt.Errorf("打开数据库文件失败: %w", err)
	}
	defer f.Close()

	if opts.MaxBytes > 0 {
		fi, err := f.Stat()
		if err != nil {
			return BackupResult{}, fmt.Errorf("读取数据库文件大小失败: %w", err)
		}
		if fi.Size() > opts.MaxBytes {
			return BackupResult{}, fmt.Errorf("数据库 %d 字节超出备份上限 %d 字节，请改用文件级备份",
				fi.Size(), opts.MaxBytes)
		}
	}

	n, err := io.Copy(w, f)
	if err != nil {
		return BackupResult{}, fmt.Errorf("写出备份失败: %w", err)
	}
	return BackupResult{Bytes: n, At: time.Now().UTC()}, nil
}

// RestoreResult 汇报一次恢复导入的行数。
type RestoreResult struct {
	Tables map[string]int64 `json:"tables"`
	Bytes  int64            `json:"bytes"`
	At     time.Time        `json:"at"`
}

// RestoreFrom 用 src 提供的数据库快照替换当前库内容。
//
// 实现不替换文件、不重开连接池：把快照落到临时文件后 ATTACH，
// 在一个事务里清空并重灌各业务表。这样调用方持有的 *Store 句柄、
// 写租约与心跳都保持有效，恢复本身也是原子的（失败即回滚）。
func (s *Store) RestoreFrom(ctx context.Context, src io.Reader, maxBytes int64) (RestoreResult, error) {
	if !s.Writable() {
		return RestoreResult{}, ErrReadOnly
	}
	dir := filepath.Dir(s.opts.Path)
	tmp, err := os.CreateTemp(dir, ".restore-*.db")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("创建恢复临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
		// WAL/SHM 侧车文件由 SQLite 在关闭时清理，残留时一并删除。
		_ = os.Remove(tmpPath + "-wal")
		_ = os.Remove(tmpPath + "-shm")
	}()
	// 临时文件与 data_dir 同权限口径：只有属主可读写。
	_ = tmp.Chmod(0o600)

	reader := src
	if maxBytes > 0 {
		reader = io.LimitReader(src, maxBytes+1)
	}
	written, err := io.Copy(tmp, reader)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return RestoreResult{}, fmt.Errorf("落盘恢复数据失败: %w", err)
	}
	if maxBytes > 0 && written > maxBytes {
		return RestoreResult{}, fmt.Errorf("恢复数据超出上限 %d 字节", maxBytes)
	}
	if written == 0 {
		return RestoreResult{}, errors.New("恢复数据为空")
	}

	if err := validateSnapshot(ctx, tmpPath, s.opts.BusyTimeout); err != nil {
		return RestoreResult{}, err
	}

	out := RestoreResult{Tables: make(map[string]int64, len(snapshotTables)), Bytes: written, At: time.Now().UTC()}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	conn, err := s.writeDB.Conn(ctx)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("获取写连接失败: %w", err)
	}
	defer conn.Close()

	// ATTACH 不能在事务内执行，故先挂载再开事务。
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS restore_src`, tmpPath); err != nil {
		return RestoreResult{}, fmt.Errorf("挂载恢复快照失败: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `DETACH DATABASE restore_src`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("开启恢复事务失败: %w", err)
	}
	// 外键检查推迟到提交，避免受表清空/重灌顺序影响。
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		_ = tx.Rollback()
		return RestoreResult{}, fmt.Errorf("设置延迟外键检查失败: %w", err)
	}
	for i := len(snapshotTables) - 1; i >= 0; i-- {
		t := snapshotTables[i]
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.`+t); err != nil {
			_ = tx.Rollback()
			return RestoreResult{}, fmt.Errorf("清空表 %s 失败: %w", t, err)
		}
	}
	for _, t := range snapshotTables {
		res, err := tx.ExecContext(ctx, `INSERT INTO main.`+t+` SELECT * FROM restore_src.`+t)
		if err != nil {
			_ = tx.Rollback()
			return RestoreResult{}, fmt.Errorf("导入表 %s 失败: %w", t, err)
		}
		n, _ := res.RowsAffected()
		out.Tables[t] = n
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return RestoreResult{}, fmt.Errorf("提交恢复事务失败: %w", err)
	}
	// 恢复可能替换 pricing_rules / model_routes，必须失效服务层的内存快照，
	// 否则计价与路由继续用恢复前的旧规则直到下一次写操作。
	s.notifyPricingChanged()
	s.notifyRoutesChanged()
	return out, nil
}

// validateSnapshot 校验候选快照：可打开、schema 版本一致、业务表齐备。
func validateSnapshot(ctx context.Context, path string, busyTimeout time.Duration) error {
	db, err := openPool(Options{Path: path, BusyTimeout: busyTimeout, ReadOnly: true}, false)
	if err != nil {
		return fmt.Errorf("恢复数据不是可用的 SQLite 数据库: %w", err)
	}
	defer db.Close()

	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("恢复数据缺少 schema_migrations 表，疑似不是本插件的备份: %w", err)
	}
	if !version.Valid {
		return errors.New("恢复数据没有已应用的迁移记录")
	}
	if int(version.Int64) != SchemaVersion {
		return fmt.Errorf("恢复数据 schema 版本为 %d，当前插件为 %d，请使用同版本备份",
			version.Int64, SchemaVersion)
	}
	for _, t := range snapshotTables {
		var n int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&n); err != nil {
			return fmt.Errorf("恢复数据缺少表 %s: %w", t, err)
		}
	}
	return nil
}

// ResetOptions 控制统计重置的范围。
type ResetOptions struct {
	// Requests 清空逐请求明细与分钟聚合。
	Requests bool
	// Reservations 清空已终结的预占记录（在途预占始终保留）。
	Reservations bool
	// KeyCounters 归零 Key 上的累计与周期已用金额/Token（两种口径必须同步清，
	// 归零点不一致会让 Token 限额继续被旧用量占用）。
	KeyCounters bool
	// Audit 清空审计事件。
	Audit bool
}

// AllStatistics 返回「重置统计」的默认范围：明细、聚合、终结预占与 Key 计数器。
// 审计事件默认保留，因为重置本身也要留痕。
func AllStatistics() ResetOptions {
	return ResetOptions{Requests: true, Reservations: true, KeyCounters: true}
}

// ResetResult 汇报重置删除/更新的行数。
type ResetResult struct {
	Requests     int64 `json:"requests"`
	Rollups      int64 `json:"rollups"`
	Reservations int64 `json:"reservations"`
	Keys         int64 `json:"keys"`
	AuditEvents  int64 `json:"audit_events"`
}

// ResetStatistics 按范围清空统计数据。Key、caller、计价规则本身不受影响。
func (s *Store) ResetStatistics(ctx context.Context, opts ResetOptions) (ResetResult, error) {
	var out ResetResult
	err := s.Write(ctx, func(tx *sql.Tx) error {
		exec := func(dst *int64, query string) error {
			res, err := tx.ExecContext(ctx, query)
			if err != nil {
				return fmt.Errorf("重置失败 (%s): %w", query, err)
			}
			n, _ := res.RowsAffected()
			*dst = n
			return nil
		}
		if opts.Requests {
			if err := exec(&out.Requests, `DELETE FROM requests`); err != nil {
				return err
			}
			if err := exec(&out.Rollups, `DELETE FROM usage_rollups`); err != nil {
				return err
			}
		}
		if opts.Reservations {
			if err := exec(&out.Reservations,
				`DELETE FROM reservations WHERE status IN ('settled','released')`); err != nil {
				return err
			}
		}
		if opts.KeyCounters {
			if err := exec(&out.Keys, `UPDATE plugin_keys SET
				spent_micro_usd = 0,
				daily_cycle_key = '', daily_spent_micro_usd = 0,
				weekly_cycle_key = '', weekly_spent_micro_usd = 0,
				monthly_cycle_key = '', monthly_spent_micro_usd = 0,
				tokens_used = 0,
				daily_tokens_used = 0, weekly_tokens_used = 0, monthly_tokens_used = 0,
				daily_requests_used = 0, monthly_requests_used = 0,
				updated_at = `+fmt.Sprint(nowMillis())); err != nil {
				return err
			}
		}
		if opts.Audit {
			if err := exec(&out.AuditEvents, `DELETE FROM audit_events`); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// isClosed 报告 Store 是否已关闭。
func (s *Store) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
