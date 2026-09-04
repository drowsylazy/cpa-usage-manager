package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T, path, owner string) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Path:           path,
		OwnerID:        owner,
		BusyTimeout:    2 * time.Second,
		LeaseTTL:       600 * time.Millisecond,
		LeaseHeartbeat: 50 * time.Millisecond,
		HandoverWait:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open(%s) 失败: %v", owner, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenSeedsSchemaAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpa.db")
	s := openTestStore(t, path, "owner-a")

	if !s.Writable() {
		t.Fatal("正常打开应持有写租约")
	}
	if got, err := s.CurrentSchemaVersion(context.Background()); err != nil || got != SchemaVersion {
		t.Fatalf("schema 版本 = %d, err=%v, 期望 %d", got, err, SchemaVersion)
	}

	var callers, pricing, keys int
	if err := s.Read(context.Background(), func(q Querier) error {
		return q.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM callers`).Scan(&callers)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(context.Background(), func(q Querier) error {
		if err := q.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pricing_rules`).Scan(&pricing); err != nil {
			return err
		}
		return q.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM plugin_keys`).Scan(&keys)
	}); err != nil {
		t.Fatal(err)
	}
	if callers != 1 || pricing != 1 || keys != 0 {
		t.Fatalf("默认种子异常: callers=%d pricing=%d keys=%d", callers, pricing, keys)
	}

	stats, err := s.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats 失败: %v", err)
	}
	if stats.Callers != 1 || stats.PricingRules != 1 || stats.Keys != 0 || !stats.Writable {
		t.Fatalf("Stats 异常: %+v", stats)
	}
}

func TestWriteRollbackAndKV(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()

	rollbackErr := errors.New("rollback")
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO callers (id, display_name, enabled, created_at, updated_at) VALUES ('temp', 'temp', 1, 1, 1)`); err != nil {
			return err
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("Write 应返回回调错误，得到 %v", err)
	}
	var count int
	if err := s.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM callers WHERE id = 'temp'`).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("回滚后临时 caller 仍存在: %d", count)
	}

	if err := s.SetMeta(ctx, "k", "v"); err != nil {
		t.Fatalf("SetMeta 失败: %v", err)
	}
	if got, ok, err := s.GetMeta(ctx, "k"); err != nil || !ok || got != "v" {
		t.Fatalf("GetMeta = %q, ok=%v, err=%v", got, ok, err)
	}
	if _, ok, err := s.GetPreference(ctx, "missing"); err != nil || ok {
		t.Fatalf("缺失 preference 应返回 ok=false: ok=%v err=%v", ok, err)
	}
}

func TestReadOnlyOpenCannotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpa.db")
	writer := openTestStore(t, path, "owner-a")
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 writer 失败: %v", err)
	}

	reader, err := Open(context.Background(), Options{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatalf("只读打开失败: %v", err)
	}
	defer reader.Close()
	if reader.Writable() {
		t.Fatal("只读实例不应标记为 writable")
	}
	if err := reader.Write(context.Background(), func(*sql.Tx) error { return nil }); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("只读 Write 应返回 ErrReadOnly，得到 %v", err)
	}
	if _, err := reader.CurrentSchemaVersion(context.Background()); err != nil {
		t.Fatalf("只读实例无法读取 schema: %v", err)
	}
}

func TestLeaseHandover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpa.db")
	first := openTestStore(t, path, "owner-a")
	second, err := Open(context.Background(), Options{
		Path:           path,
		OwnerID:        "owner-b",
		BusyTimeout:    2 * time.Second,
		LeaseTTL:       600 * time.Millisecond,
		LeaseHeartbeat: 50 * time.Millisecond,
		HandoverWait:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("第二实例应完成 handover: %v", err)
	}
	defer second.Close()
	defer first.Close()

	deadline := time.Now().Add(time.Second)
	for first.Writable() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if first.Writable() {
		t.Fatal("handover 后旧实例仍可写")
	}
	if !second.Writable() {
		t.Fatal("handover 后新实例未持有写租约")
	}
	if err := first.Write(context.Background(), func(*sql.Tx) error { return nil }); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("旧实例 handover 后 Write 应失败，得到 %v", err)
	}
}

func TestApplyRetention(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "cpa.db"), "owner-a")
	ctx := context.Background()
	now := time.UnixMilli(2_000_000_000).UTC()
	oldTS := now.Add(-48 * time.Hour).UnixMilli()
	newTS := now.Add(-2 * time.Hour).UnixMilli()
	oldBucket := BucketMinute(time.UnixMilli(oldTS))
	newBucket := BucketMinute(time.UnixMilli(newTS))

	err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, row := range []struct {
			id, ts string
		}{
			{"old", "old"}, {"new", "new"},
		} {
			stamp := oldTS
			if row.ts == "new" {
				stamp = newTS
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO requests (id, ts) VALUES (?, ?)`, row.id, stamp); err != nil {
				return err
			}
		}
		for _, row := range []struct {
			bucket int64
			model  string
		}{
			{oldBucket, "old"}, {newBucket, "new"},
		} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO usage_rollups (bucket_minute, model) VALUES (?, ?)`, row.bucket, row.model); err != nil {
				return err
			}
		}
		for _, row := range []struct {
			id, status string
		}{
			{"old-settled", "settled"}, {"new-held", "held"},
		} {
			stamp := oldTS
			if row.status == "held" {
				stamp = newTS
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO reservations (id, key_id, status, created_at, expires_at, heartbeat_at) VALUES (?, 'missing', ?, ?, ?, ?)`, row.id, row.status, stamp, stamp, stamp); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("准备 retention 数据失败: %v", err)
	}

	got, err := s.ApplyRetention(ctx, 1, 0, now)
	if err != nil {
		t.Fatalf("ApplyRetention 失败: %v", err)
	}
	if got.Requests != 1 || got.Rollups != 1 || got.Reservations != 1 {
		t.Fatalf("清理计数异常: %+v", got)
	}

	var oldRequests, newRequests, oldHeld int
	if err := s.Read(ctx, func(q Querier) error {
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE id = 'old'`).Scan(&oldRequests); err != nil {
			return err
		}
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE id = 'new'`).Scan(&newRequests); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM reservations WHERE status = 'held'`).Scan(&oldHeld)
	}); err != nil {
		t.Fatal(err)
	}
	if oldRequests != 0 || newRequests != 1 || oldHeld != 1 {
		t.Fatalf("保留清理结果异常: old=%d new=%d held=%d", oldRequests, newRequests, oldHeld)
	}
}
