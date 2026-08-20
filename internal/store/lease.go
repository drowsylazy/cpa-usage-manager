package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// 写租约（writer_lease）实现跨进程单写者。
//
// 协议：
//   - 租约是单行记录，owner_id 标识持有者，heartbeat_at 为最近心跳时间。
//   - 心跳超过 LeaseTTL 未更新即视为失效，新实例可直接接管。
//   - 若租约仍活跃，新实例把自己写入 handover_to 请求接管，然后等待；
//     在位者的心跳协程发现 handover_to 指向他人时主动让出并转入只读。
//   - 让出后新实例接管，原实例的写操作返回 ErrReadOnly。
//
// 这样宿主热重载插件（新旧实例短暂并存）时不会长时间不可写，
// 也不会出现两个实例同时写库。

// ErrLeaseHeld 表示租约被其他活跃实例持有且接管超时。
var ErrLeaseHeld = errors.New("store: 写租约被其他实例持有，接管超时")

// leaseRow 是租约表的一行。
type leaseRow struct {
	ownerID     string
	pid         int64
	acquiredAt  int64
	heartbeatAt int64
	handoverTo  string
	exists      bool
}

// acquireLease 获取写租约，必要时请求并等待接管。
func (s *Store) acquireLease(ctx context.Context) error {
	deadline := time.Now().Add(s.opts.HandoverWait)
	requested := false

	for {
		taken, incumbent, err := s.tryTakeLease(ctx)
		if err != nil {
			return err
		}
		if taken {
			return nil
		}

		// 租约活跃：首次循环时登记接管请求。
		if !requested {
			if err := s.requestHandover(ctx, incumbent.ownerID); err != nil {
				return err
			}
			requested = true
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%w（在位者 owner=%s pid=%d，最近心跳 %s 前）",
				ErrLeaseHeld, incumbent.ownerID, incumbent.pid,
				time.Since(time.UnixMilli(incumbent.heartbeatAt)).Truncate(time.Millisecond))
		}

		// 等待在位者让出，或等到 TTL 过期后强行接管。
		wait := s.opts.LeaseHeartbeat / 2
		if wait <= 0 {
			wait = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// tryTakeLease 尝试一次性取得租约。
// 返回 taken=false 时 incumbent 为当前在位者信息。
func (s *Store) tryTakeLease(ctx context.Context) (taken bool, incumbent leaseRow, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return false, leaseRow{}, fmt.Errorf("开启租约事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := readLease(ctx, tx)
	if err != nil {
		return false, leaseRow{}, err
	}
	now := nowMillis()
	ttl := s.opts.LeaseTTL.Milliseconds()

	switch {
	case !cur.exists:
		// 空库首次获取。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO writer_lease (id, owner_id, pid, acquired_at, heartbeat_at, handover_to)
			 VALUES (1, ?, ?, ?, ?, '')`,
			s.opts.OwnerID, int64(os.Getpid()), now, now); err != nil {
			return false, leaseRow{}, fmt.Errorf("写入租约失败: %w", err)
		}
	case cur.ownerID == s.opts.OwnerID:
		// 已是自己持有（重入），刷新心跳并清除接管请求。
		if _, err := tx.ExecContext(ctx,
			`UPDATE writer_lease SET heartbeat_at = ?, handover_to = '' WHERE id = 1`,
			now); err != nil {
			return false, leaseRow{}, fmt.Errorf("刷新租约失败: %w", err)
		}
	case now-cur.heartbeatAt > ttl:
		// 在位者心跳超时（进程已死或卡住），直接接管。
		// WHERE 带上原 owner_id 与心跳值，确保并发下只有一个实例能接管。
		res, err := tx.ExecContext(ctx,
			`UPDATE writer_lease
			 SET owner_id = ?, pid = ?, acquired_at = ?, heartbeat_at = ?, handover_to = ''
			 WHERE id = 1 AND owner_id = ? AND heartbeat_at = ?`,
			s.opts.OwnerID, int64(os.Getpid()), now, now, cur.ownerID, cur.heartbeatAt)
		if err != nil {
			return false, leaseRow{}, fmt.Errorf("接管失效租约失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// 被别的实例抢先，下一轮重试。
			return false, cur, nil
		}
	default:
		// 租约活跃，不能接管。
		return false, cur, nil
	}

	if err := tx.Commit(); err != nil {
		return false, leaseRow{}, fmt.Errorf("提交租约事务失败: %w", err)
	}
	return true, leaseRow{}, nil
}

// requestHandover 登记接管请求，请在位者让出租约。
func (s *Store) requestHandover(ctx context.Context, incumbentOwner string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.writeDB.ExecContext(ctx,
		`UPDATE writer_lease SET handover_to = ? WHERE id = 1 AND owner_id = ?`,
		s.opts.OwnerID, incumbentOwner)
	if err != nil {
		return fmt.Errorf("登记接管请求失败: %w", err)
	}
	return nil
}

// releaseLease 主动让出租约（仅当自己持有）。
func (s *Store) releaseLease(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// 把心跳置 0 使租约立即视为失效，后继实例无需等待 TTL。
	_, err := s.writeDB.ExecContext(ctx,
		`UPDATE writer_lease SET heartbeat_at = 0, handover_to = '' WHERE id = 1 AND owner_id = ?`,
		s.opts.OwnerID)
	if err != nil {
		return fmt.Errorf("让出租约失败: %w", err)
	}
	return nil
}

// startHeartbeat 启动心跳协程：定期刷新心跳，并响应接管请求。
func (s *Store) startHeartbeat() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	s.leaseCancel = cancel
	s.leaseDone = done
	s.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(s.opts.LeaseHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !s.beat(ctx) {
					// 租约已丢失，停止心跳。
					return
				}
			}
		}
	}()
}

// beat 执行一次心跳。返回 false 表示租约已丢失，应停止心跳。
func (s *Store) beat(ctx context.Context) bool {
	s.writeMu.Lock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		s.writeMu.Unlock()
		// 单次失败不放弃租约，等下个周期重试。
		return true
	}
	cur, readErr := readLease(ctx, tx)
	if readErr != nil {
		_ = tx.Rollback()
		s.writeMu.Unlock()
		return true
	}

	// 租约已被他人接管。
	if !cur.exists || cur.ownerID != s.opts.OwnerID {
		_ = tx.Rollback()
		s.writeMu.Unlock()
		s.loseLease()
		return false
	}

	// 有其他实例请求接管：主动让出。
	if cur.handoverTo != "" && cur.handoverTo != s.opts.OwnerID {
		_, err := tx.ExecContext(ctx,
			`UPDATE writer_lease SET heartbeat_at = 0, handover_to = '' WHERE id = 1 AND owner_id = ?`,
			s.opts.OwnerID)
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			_ = tx.Rollback()
			s.writeMu.Unlock()
			return true
		}
		s.writeMu.Unlock()
		s.loseLease()
		return false
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE writer_lease SET heartbeat_at = ? WHERE id = 1 AND owner_id = ?`,
		nowMillis(), s.opts.OwnerID)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		_ = tx.Rollback()
	}
	s.writeMu.Unlock()
	return true
}

// loseLease 把本实例转为只读并触发回调。
func (s *Store) loseLease() {
	s.mu.Lock()
	s.writable = false
	fn := s.onLeaseLost
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// readLease 读取租约行。
func readLease(ctx context.Context, q Querier) (leaseRow, error) {
	var r leaseRow
	err := q.QueryRowContext(ctx,
		`SELECT owner_id, pid, acquired_at, heartbeat_at, handover_to FROM writer_lease WHERE id = 1`).
		Scan(&r.ownerID, &r.pid, &r.acquiredAt, &r.heartbeatAt, &r.handoverTo)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseRow{exists: false}, nil
	}
	if err != nil {
		return leaseRow{}, fmt.Errorf("读取写租约失败: %w", err)
	}
	r.exists = true
	return r, nil
}

// LeaseInfo 是对外暴露的租约状态。
type LeaseInfo struct {
	OwnerID     string    `json:"owner_id"`
	PID         int64     `json:"pid"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	HandoverTo  string    `json:"handover_to,omitempty"`
	IsSelf      bool      `json:"is_self"`
}

// LeaseInfo 读取当前租约状态（用于 health/系统页展示）。
func (s *Store) LeaseInfo(ctx context.Context) (LeaseInfo, bool, error) {
	r, err := readLease(ctx, s.readDB)
	if err != nil {
		return LeaseInfo{}, false, err
	}
	if !r.exists {
		return LeaseInfo{}, false, nil
	}
	return LeaseInfo{
		OwnerID:     r.ownerID,
		PID:         r.pid,
		AcquiredAt:  time.UnixMilli(r.acquiredAt).UTC(),
		HeartbeatAt: time.UnixMilli(r.heartbeatAt).UTC(),
		HandoverTo:  r.handoverTo,
		IsSelf:      r.ownerID == s.opts.OwnerID,
	}, true, nil
}
