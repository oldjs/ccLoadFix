package sql

import (
	"context"
	"database/sql"
	"fmt"

	"ccLoad/internal/model"
)

// ============================================================================
// URLRuntimeState 持久化（路由热数据：EWMA 延迟、亲和、warm、URL 级冷却等）
//
// 写入策略：全量替换。每次刷盘把内存里所有有效 entries 全量同步到 DB：
//   BEGIN; DELETE FROM url_runtime_state; INSERT 全部 entries; COMMIT
// 业务唯一性由调用方（内存 map）保证，DB 不依赖主键去重。
// ============================================================================

// URLRuntimeStateReplaceAll 全量替换 url_runtime_state 表内容
// entries 为空时也会清空表（用于"全部状态都过期了"的场景）
func (s *SQLStore) URLRuntimeStateReplaceAll(ctx context.Context, entries []model.URLRuntimeState) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		// 先清空旧数据
		if _, err := tx.ExecContext(ctx, "DELETE FROM url_runtime_state"); err != nil {
			return fmt.Errorf("delete url_runtime_state: %w", err)
		}
		if len(entries) == 0 {
			return nil
		}
		// 批量插入新数据，按驱动选 placeholder 数量上限
		// 一次最多塞 200 行，避开 SQLite 的 ?-参数上限和 MySQL 单语句长度限制
		const batchSize = 200
		const cols = 9 // channel_id, url, model, kind, ewma_ms, expires_at, consecutive_fails, payload, updated_at
		for start := 0; start < len(entries); start += batchSize {
			end := min(start+batchSize, len(entries))
			chunk := entries[start:end]

			placeholders := make([]byte, 0, len(chunk)*(cols*2+3))
			args := make([]any, 0, len(chunk)*cols)
			for i, e := range chunk {
				if i > 0 {
					placeholders = append(placeholders, ',')
				}
				placeholders = append(placeholders, "(?,?,?,?,?,?,?,?,?)"...)
				args = append(args,
					e.ChannelID, e.URL, e.Model, e.Kind,
					e.EWMAms, e.ExpiresAt, e.ConsecutiveFails, e.Payload,
					e.UpdatedAt,
				)
			}
			query := "INSERT INTO url_runtime_state " +
				"(channel_id, url, model, kind, ewma_ms, expires_at, consecutive_fails, payload, updated_at) VALUES " +
				string(placeholders)
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("insert url_runtime_state batch: %w", err)
			}
		}
		return nil
	})
}

// URLRuntimeStateLoadAll 一次性读取全部 entries（启动恢复用）
// 不过滤过期数据，由调用方按当前 now 与 TTL 决定保留哪些
func (s *SQLStore) URLRuntimeStateLoadAll(ctx context.Context) ([]model.URLRuntimeState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT channel_id, url, model, kind, ewma_ms, expires_at, consecutive_fails, payload, updated_at
		FROM url_runtime_state
	`)
	if err != nil {
		return nil, fmt.Errorf("query url_runtime_state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.URLRuntimeState
	for rows.Next() {
		var e model.URLRuntimeState
		if err := rows.Scan(
			&e.ChannelID, &e.URL, &e.Model, &e.Kind,
			&e.EWMAms, &e.ExpiresAt, &e.ConsecutiveFails, &e.Payload,
			&e.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan url_runtime_state: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate url_runtime_state: %w", err)
	}
	return out, nil
}

// ChannelAffinityReplaceAll 全量替换 channel_affinity_state 表内容
func (s *SQLStore) ChannelAffinityReplaceAll(ctx context.Context, entries []model.ChannelAffinityState) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM channel_affinity_state"); err != nil {
			return fmt.Errorf("delete channel_affinity_state: %w", err)
		}
		if len(entries) == 0 {
			return nil
		}
		const batchSize = 500
		const cols = 3
		for start := 0; start < len(entries); start += batchSize {
			end := min(start+batchSize, len(entries))
			chunk := entries[start:end]

			placeholders := make([]byte, 0, len(chunk)*(cols*2+3))
			args := make([]any, 0, len(chunk)*cols)
			for i, e := range chunk {
				if i > 0 {
					placeholders = append(placeholders, ',')
				}
				placeholders = append(placeholders, "(?,?,?)"...)
				args = append(args, e.Model, e.ChannelID, e.UpdatedAt)
			}
			query := "INSERT INTO channel_affinity_state (model, channel_id, updated_at) VALUES " + string(placeholders)
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("insert channel_affinity_state batch: %w", err)
			}
		}
		return nil
	})
}

// ChannelAffinityLoadAll 一次性读取全部 entries（启动恢复用）
func (s *SQLStore) ChannelAffinityLoadAll(ctx context.Context) ([]model.ChannelAffinityState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT model, channel_id, updated_at FROM channel_affinity_state
	`)
	if err != nil {
		return nil, fmt.Errorf("query channel_affinity_state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ChannelAffinityState
	for rows.Next() {
		var e model.ChannelAffinityState
		if err := rows.Scan(&e.Model, &e.ChannelID, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan channel_affinity_state: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel_affinity_state: %w", err)
	}
	return out, nil
}
