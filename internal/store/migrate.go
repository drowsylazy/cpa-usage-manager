package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SchemaVersion 是本代码期望的数据库 schema 版本。
// 打开库时若发现库版本更高，说明是被更新版插件写过的库，拒绝降级使用。
const SchemaVersion = 1

// migration 是一次版本化迁移。
type migration struct {
	version int
	name    string
	// stmts 按顺序在同一事务内执行。
	stmts []string
}

// migrations 是全部迁移，按 version 升序。新增变更一律追加新版本，不得改写既有项。
var migrations = []migration{
	{
		version: 1,
		name:    "initial_schema",
		stmts: []string{
			// ---- 归属（组织/团队），不承载额度 ----
			`CREATE TABLE callers (
				id            TEXT    PRIMARY KEY,
				display_name  TEXT    NOT NULL DEFAULT '',
				enabled       INTEGER NOT NULL DEFAULT 1,
				created_at    INTEGER NOT NULL,
				updated_at    INTEGER NOT NULL
			)`,

			// ---- 插件 Key：签发策略 + 安全材料 ----
			// kid 是查找键（明文形如 cum-<kid>-<secret>）；key_hash 是 HMAC 校验值，
			// 以 kid 定位后再做常量时间比较，从而支持 pepper 轮换。
			//
			// spent_micro_usd 与三个 *_cycle_key/*_spent_micro_usd 是**持久累计器**：
			// 额度判定不从 reservations 历史重算，因为 reservations 会被 retention
			// 清理（默认 365 天），重算会让长期使用的 Key 总额度被悄悄「重置」。
			// 周期计数器带 cycle_key（UTC 期间标识），结算时发现跨期即先归零，
			// 因此无需定时任务即可自动滚动。在途金额另从 status='held' 实时汇总。
			`CREATE TABLE plugin_keys (
				kid                     TEXT    PRIMARY KEY,
				key_hash                BLOB    NOT NULL,
				encrypted_material      BLOB    NOT NULL,
				pepper_id               TEXT    NOT NULL,
				fingerprint             TEXT    NOT NULL DEFAULT '',
				principal               TEXT    NOT NULL DEFAULT '',
				caller_scope            TEXT    NOT NULL DEFAULT 'caller',
				caller_id               TEXT    NOT NULL DEFAULT 'default',
				label                   TEXT    NOT NULL DEFAULT '',
				enabled                 INTEGER NOT NULL DEFAULT 1,
				revoked_at              INTEGER,
				expires_at              INTEGER,
				quota_micro_usd         INTEGER,
				daily_micro_usd         INTEGER,
				weekly_micro_usd        INTEGER,
				monthly_micro_usd       INTEGER,
				max_concurrent_requests INTEGER NOT NULL DEFAULT 0,
				allowed_models_json     TEXT    NOT NULL DEFAULT '',
				spent_micro_usd         INTEGER NOT NULL DEFAULT 0,
				daily_cycle_key         TEXT    NOT NULL DEFAULT '',
				daily_spent_micro_usd   INTEGER NOT NULL DEFAULT 0,
				weekly_cycle_key        TEXT    NOT NULL DEFAULT '',
				weekly_spent_micro_usd  INTEGER NOT NULL DEFAULT 0,
				monthly_cycle_key       TEXT    NOT NULL DEFAULT '',
				monthly_spent_micro_usd INTEGER NOT NULL DEFAULT 0,
				created_at              INTEGER NOT NULL,
				updated_at              INTEGER NOT NULL,
				last_used_at            INTEGER,
				FOREIGN KEY (caller_id) REFERENCES callers(id) ON DELETE RESTRICT
			)`,
			`CREATE UNIQUE INDEX idx_plugin_keys_hash ON plugin_keys(key_hash)`,
			`CREATE INDEX idx_plugin_keys_caller ON plugin_keys(caller_id)`,
			`CREATE INDEX idx_plugin_keys_active ON plugin_keys(enabled, revoked_at)`,

			// ---- 统一计价表：额度结算与面板展示共用 ----
			// 各 price_* 均为「每百万 token 的 micro-USD」整数。
			`CREATE TABLE pricing_rules (
				id                   INTEGER PRIMARY KEY AUTOINCREMENT,
				match_kind           TEXT    NOT NULL CHECK (match_kind IN ('exact','glob','regexp')),
				pattern              TEXT    NOT NULL,
				priority             INTEGER NOT NULL DEFAULT 0,
				enabled              INTEGER NOT NULL DEFAULT 1,
				price_input          INTEGER NOT NULL DEFAULT 0,
				price_output         INTEGER NOT NULL DEFAULT 0,
				price_reasoning      INTEGER NOT NULL DEFAULT 0,
				price_cached         INTEGER NOT NULL DEFAULT 0,
				price_cache_read     INTEGER NOT NULL DEFAULT 0,
				price_cache_creation INTEGER NOT NULL DEFAULT 0,
				accounting_mode      TEXT    NOT NULL DEFAULT 'default',
				billing_mode         TEXT    NOT NULL DEFAULT 'token',
				per_image_micro_usd  INTEGER NOT NULL DEFAULT 0,
				source               TEXT    NOT NULL DEFAULT 'manual' CHECK (source IN ('manual','models_dev')),
				models_dev_id        TEXT    NOT NULL DEFAULT '',
				created_at           INTEGER NOT NULL,
				updated_at           INTEGER NOT NULL
			)`,
			`CREATE UNIQUE INDEX idx_pricing_rules_match ON pricing_rules(match_kind, pattern)`,
			`CREATE INDEX idx_pricing_rules_lookup ON pricing_rules(enabled, priority DESC, id)`,

			// ---- 预占与在途 ----
			`CREATE TABLE reservations (
				id                TEXT    PRIMARY KEY,
				key_id            TEXT    NOT NULL,
				caller_id         TEXT    NOT NULL DEFAULT '',
				model             TEXT    NOT NULL DEFAULT '',
				idempotency_key   TEXT,
				status            TEXT    NOT NULL CHECK (status IN ('held','settled','released')),
				held_micro_usd    INTEGER NOT NULL DEFAULT 0,
				settled_micro_usd INTEGER NOT NULL DEFAULT 0,
				reserved_tokens   INTEGER NOT NULL DEFAULT 0,
				created_at        INTEGER NOT NULL,
				expires_at        INTEGER NOT NULL,
				heartbeat_at      INTEGER NOT NULL,
				settled_at        INTEGER,
				released_at       INTEGER
			)`,
			`CREATE UNIQUE INDEX idx_reservations_idem ON reservations(idempotency_key)
				WHERE idempotency_key IS NOT NULL`,
			`CREATE INDEX idx_reservations_key_status ON reservations(key_id, status)`,
			`CREATE INDEX idx_reservations_stale ON reservations(heartbeat_at) WHERE status = 'held'`,
			`CREATE INDEX idx_reservations_settled ON reservations(key_id, settled_at)
				WHERE status = 'settled'`,

			// ---- 逐请求记录（tracker 明细与 credit-manager 账本合并）----
			// tps_milli 为 TPS×1000 的整数，避免存浮点。
			`CREATE TABLE requests (
				id                    TEXT    PRIMARY KEY,
				ts                    INTEGER NOT NULL,
				key_id                TEXT    NOT NULL DEFAULT '',
				caller_id             TEXT    NOT NULL DEFAULT '',
				model                 TEXT    NOT NULL DEFAULT '',
				provider              TEXT    NOT NULL DEFAULT '',
				source                TEXT    NOT NULL DEFAULT '',
				auth_id               TEXT    NOT NULL DEFAULT '',
				auth_label            TEXT    NOT NULL DEFAULT '',
				auth_type             TEXT    NOT NULL DEFAULT '',
				tier                  TEXT    NOT NULL DEFAULT '',
				result                TEXT    NOT NULL DEFAULT 'ok',
				input_tokens          INTEGER NOT NULL DEFAULT 0,
				output_tokens         INTEGER NOT NULL DEFAULT 0,
				reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
				cached_tokens         INTEGER NOT NULL DEFAULT 0,
				cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
				cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
				total_tokens          INTEGER NOT NULL DEFAULT 0,
				latency_ms            INTEGER NOT NULL DEFAULT 0,
				ttft_ms               INTEGER NOT NULL DEFAULT 0,
				generation_ms         INTEGER NOT NULL DEFAULT 0,
				tps_milli             INTEGER NOT NULL DEFAULT 0,
				thinking_intensity    TEXT    NOT NULL DEFAULT '',
				cost_micro_usd        INTEGER NOT NULL DEFAULT 0,
				priced                INTEGER NOT NULL DEFAULT 0,
				reservation_id        TEXT    NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX idx_requests_ts ON requests(ts DESC)`,
			`CREATE INDEX idx_requests_key_ts ON requests(key_id, ts DESC)`,
			`CREATE INDEX idx_requests_model_ts ON requests(model, ts DESC)`,
			`CREATE INDEX idx_requests_caller_ts ON requests(caller_id, ts DESC)`,

			// ---- 分钟聚合：面板快速加载 ----
			// bucket_minute 为 UTC Unix 分钟数。主键即分组维度。
			`CREATE TABLE usage_rollups (
				bucket_minute         INTEGER NOT NULL,
				model                 TEXT    NOT NULL DEFAULT '',
				key_id                TEXT    NOT NULL DEFAULT '',
				caller_id             TEXT    NOT NULL DEFAULT '',
				provider              TEXT    NOT NULL DEFAULT '',
				source                TEXT    NOT NULL DEFAULT '',
				auth_type             TEXT    NOT NULL DEFAULT '',
				tier                  TEXT    NOT NULL DEFAULT '',
				result                TEXT    NOT NULL DEFAULT 'ok',
				req_count             INTEGER NOT NULL DEFAULT 0,
				fail_count            INTEGER NOT NULL DEFAULT 0,
				input_tokens          INTEGER NOT NULL DEFAULT 0,
				output_tokens         INTEGER NOT NULL DEFAULT 0,
				reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
				cached_tokens         INTEGER NOT NULL DEFAULT 0,
				cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
				cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
				total_tokens          INTEGER NOT NULL DEFAULT 0,
				latency_sum           INTEGER NOT NULL DEFAULT 0,
				ttft_sum              INTEGER NOT NULL DEFAULT 0,
				ttft_count            INTEGER NOT NULL DEFAULT 0,
				generation_sum        INTEGER NOT NULL DEFAULT 0,
				tps_milli_sum         INTEGER NOT NULL DEFAULT 0,
				cost_micro_usd        INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (bucket_minute, model, key_id, caller_id, provider, source, auth_type, tier, result)
			) WITHOUT ROWID`,
			`CREATE INDEX idx_rollups_bucket ON usage_rollups(bucket_minute)`,
			`CREATE INDEX idx_rollups_model ON usage_rollups(model, bucket_minute)`,
			`CREATE INDEX idx_rollups_key ON usage_rollups(key_id, bucket_minute)`,

			// ---- 审计 ----
			`CREATE TABLE audit_events (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				ts          INTEGER NOT NULL,
				actor       TEXT    NOT NULL DEFAULT '',
				action      TEXT    NOT NULL,
				entity_type TEXT    NOT NULL DEFAULT '',
				entity_id   TEXT    NOT NULL DEFAULT '',
				detail_json TEXT    NOT NULL DEFAULT '{}'
			)`,
			`CREATE INDEX idx_audit_ts ON audit_events(ts DESC)`,
			`CREATE INDEX idx_audit_entity ON audit_events(entity_type, entity_id, ts DESC)`,
			`CREATE INDEX idx_audit_action ON audit_events(action, ts DESC)`,

			// ---- OAuth 认证额度快照与预测基线 ----
			`CREATE TABLE auth_quota_snapshots (
				provider      TEXT    NOT NULL,
				auth_id       TEXT    NOT NULL,
				snapshot_json TEXT    NOT NULL DEFAULT '{}',
				fetched_at    INTEGER NOT NULL,
				status        TEXT    NOT NULL DEFAULT '',
				PRIMARY KEY (provider, auth_id)
			) WITHOUT ROWID`,
			`CREATE TABLE auth_quota_window_baselines (
				provider   TEXT    NOT NULL,
				auth_id    TEXT    NOT NULL,
				window_id  TEXT    NOT NULL,
				cycle_key  TEXT    NOT NULL,
				observed   INTEGER NOT NULL DEFAULT 0,
				baseline   INTEGER NOT NULL DEFAULT 0,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (provider, auth_id, window_id, cycle_key)
			) WITHOUT ROWID`,

			// ---- 面板偏好与内部元数据 ----
			`CREATE TABLE preferences (
				k TEXT PRIMARY KEY,
				v TEXT NOT NULL DEFAULT ''
			) WITHOUT ROWID`,
			`CREATE TABLE meta (
				k TEXT PRIMARY KEY,
				v TEXT NOT NULL DEFAULT ''
			) WITHOUT ROWID`,

			// ---- 单写者租约（跨进程互斥 + handover）----
			// 单行表：id 恒为 1。handover_to 非空表示有新实例请求接管。
			`CREATE TABLE writer_lease (
				id           INTEGER PRIMARY KEY CHECK (id = 1),
				owner_id     TEXT    NOT NULL,
				pid          INTEGER NOT NULL,
				acquired_at  INTEGER NOT NULL,
				heartbeat_at INTEGER NOT NULL,
				handover_to  TEXT    NOT NULL DEFAULT ''
			)`,
		},
	},
}

// migrate 把库升级到 SchemaVersion。幂等：已应用的版本会被跳过。
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT    NOT NULL DEFAULT '',
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("创建 schema_migrations 失败: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	// 库版本高于代码版本时拒绝运行，避免旧插件写坏新 schema。
	var maxApplied int
	for v := range applied {
		if v > maxApplied {
			maxApplied = v
		}
	}
	if maxApplied > SchemaVersion {
		return fmt.Errorf("数据库 schema 版本 %d 高于本插件支持的 %d，请升级插件而不要降级使用",
			maxApplied, SchemaVersion)
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// appliedVersions 读取已应用的迁移版本集合。
func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("读取 schema_migrations 失败: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("扫描 schema_migrations 失败: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 schema_migrations 失败: %w", err)
	}
	return applied, nil
}

// applyMigration 在单个事务内执行一次迁移并记录版本。
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("迁移 %d(%s) 开启事务失败: %w", m.version, m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("迁移 %d(%s) 第 %d 条语句失败: %w", m.version, m.name, i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, nowMillis()); err != nil {
		return fmt.Errorf("迁移 %d(%s) 记录版本失败: %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("迁移 %d(%s) 提交失败: %w", m.version, m.name, err)
	}
	return nil
}

// CurrentSchemaVersion 返回库中已应用的最高迁移版本。
func (s *Store) CurrentSchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.readDB.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}
