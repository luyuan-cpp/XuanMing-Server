// Package data 是 chat 服务的数据层(私聊历史落 MySQL,2026-06-16)。
//
// 库表(deploy/mysql-init/06-social-tables.sql,pandora_social 库):
//
//	chat_private_messages  私聊消息(message_id PK = snowflake;按收发双方 + 时间倒序查历史)
//
// 只有私聊(PRIVATE)落库支持离线 PullHistory;世界 / 队伍是即时频道,不持久化。
// 表是结构化列,直接映射(CLAUDE.md §5.9 关系型表不强制 proto bytes blob)。
package data

import (
	"context"
	"database/sql"

	"github.com/luyuancpp/pandora/pkg/errcode"
	chatv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/chat/v1"
)

// PrivateRepo 是私聊历史的数据层抽象。biz 只依赖此接口,不依赖 *sql.DB。
type PrivateRepo interface {
	// SavePrivate 落一条私聊消息。message_id 由 biz 用 snowflake 预生成。
	SavePrivate(ctx context.Context, msg *chatv1.ChatMessage) error
	// ListPrivate 拉两人之间的私聊历史,按发送时间倒序。
	//   beforeMs > 0 时只取 send_time_ms < beforeMs 的(翻页游标);=0 取最新。
	//   返回结果已是客户端可见结构 ChatMessage(CLAUDE.md §14)。
	ListPrivate(ctx context.Context, playerID, peerID uint64, limit int, beforeMs int64) ([]*chatv1.ChatMessage, error)
	// DeleteMessagesBefore 删 message_id < maxMessageID 的私聊历史(保留期清理,§9.24;
	// message_id 雪花时间段单调 → 主键范围删,单批 limit 行)。返回删除行数。
	DeleteMessagesBefore(ctx context.Context, maxMessageID uint64, limit int) (int64, error)
}

// MySQLPrivateRepo 是基于 database/sql 的 PrivateRepo 实现。
type MySQLPrivateRepo struct {
	db *sql.DB
}

// NewMySQLPrivateRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_social 库)。
func NewMySQLPrivateRepo(db *sql.DB) *MySQLPrivateRepo {
	return &MySQLPrivateRepo{db: db}
}

func (r *MySQLPrivateRepo) SavePrivate(ctx context.Context, msg *chatv1.ChatMessage) error {
	const q = `INSERT INTO chat_private_messages
(message_id, sender_id, receiver_id, content, send_time_ms)
VALUES (?, ?, ?, ?, ?)`
	_, err := r.db.ExecContext(ctx, q,
		msg.GetMessageId(), msg.GetSenderId(), msg.GetTargetId(), msg.GetContent(), msg.GetSendTimeMs())
	if err != nil {
		return errcode.New(errcode.ErrInternal, "save private msg %d: %v", msg.GetMessageId(), err)
	}
	return nil
}

func (r *MySQLPrivateRepo) ListPrivate(ctx context.Context, playerID, peerID uint64, limit int, beforeMs int64) ([]*chatv1.ChatMessage, error) {
	const q = `SELECT message_id, sender_id, receiver_id, content, send_time_ms
FROM chat_private_messages
WHERE ((sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?))
  AND (? = 0 OR send_time_ms < ?)
ORDER BY send_time_ms DESC
LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, playerID, peerID, peerID, playerID, beforeMs, beforeMs, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "list private %d-%d: %v", playerID, peerID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*chatv1.ChatMessage
	for rows.Next() {
		m := &chatv1.ChatMessage{Channel: chatv1.ChatChannel_CHAT_CHANNEL_PRIVATE}
		if err := rows.Scan(&m.MessageId, &m.SenderId, &m.TargetId, &m.Content, &m.SendTimeMs); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan private msg: %v", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate private msgs: %v", err)
	}
	return out, nil
}

// DeleteMessagesBefore 删 message_id < maxMessageID 的私聊历史(保留期清理,§9.24)。
// 雪花 message_id 时间段单调 → 主键范围删,无需时间列索引;多副本并发安全(各删各的行)。
func (r *MySQLPrivateRepo) DeleteMessagesBefore(ctx context.Context, maxMessageID uint64, limit int) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM chat_private_messages WHERE message_id < ? LIMIT ?`, maxMessageID, limit)
	if err != nil {
		return 0, errcode.New(errcode.ErrInternal, "delete private history: %v", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
