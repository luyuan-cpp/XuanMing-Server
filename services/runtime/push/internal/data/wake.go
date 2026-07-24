// wake.go — 跨 Pod 投递唤醒信号(R5 复审 P2-10,2026-07-22)。
//
// 背景:投递缓冲写入(AssignAndBuffer)与连接写者可能不在同一 Pod(滚动重叠/多副本)。
// 本 Pod 写入走进程内唤醒零等待;跨 Pod 此前只有 30s 兜底轮询——消息不丢,但可能延迟
// 近 30s,与 push p99 <200ms 验收口径不符。本文件用 Redis pub/sub 把唤醒信号跨 Pod
// 广播:消费侧写完缓冲后**无条件** PUBLISH 一条 player_id(复审 P1-5:不以本地 slot 抑制,
// 陈旧残留会误抑真持有者);各 Pod 订阅同一 channel,收到后对本地连接管理器做一次 SendTo
// (本地无此玩家 = 廉价 no-op)。
//
// 契约:
//   - 信号是 **best-effort 加速器**:publish 失败 / 订阅断连期间丢的信号由既有 30s
//     兜底轮询收敛,交付正确性不依赖本通道(帧本体恒在投递缓冲);
//   - fire-and-forget 语义,无 ACK、无重放;
//   - channel 是集群级广播:每 Pod 都收到全量信号,按本地连接表过滤。信号体只有
//     十进制 player_id(~10 字节),40 万 CCU 峰值消息率下带宽与处理成本可承受;
//     若未来信号量成为瓶颈,按 player_id 分片多 channel 再扩展(§15 不预设)。
package data

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// wakeChannel 是跨 Pod 唤醒信号的 Redis pub/sub channel(infra.md 键位登记)。
const wakeChannel = "pandora:push:wake"

// RedisWakeSignal 是唤醒信号发布端。
type RedisWakeSignal struct {
	rdb redis.UniversalClient
}

// NewRedisWakeSignal 构造(与投递缓冲同一 Redis)。
func NewRedisWakeSignal(rdb redis.UniversalClient) *RedisWakeSignal {
	return &RedisWakeSignal{rdb: rdb}
}

// PublishWake 广播一条玩家唤醒信号(best-effort;失败由调用方记日志,不重试——
// 兜底轮询保证最终交付)。
func (w *RedisWakeSignal) PublishWake(ctx context.Context, playerID uint64) error {
	return w.rdb.Publish(ctx, wakeChannel, strconv.FormatUint(playerID, 10)).Err()
}

// RunWakeSubscriber 订阅唤醒信号并对每条信号调用 onWake(playerID)。
// 阻塞运行直到 ctx 取消;订阅断连(Redis 抖动/failover)按固定 1s 退避重建订阅——
// 断连窗口内丢失的信号由兜底轮询收敛,重建后无需回放。
// onWake 在订阅 goroutine 上执行,必须快速且线程安全(ConnectionManager.SendTo 满足)。
func RunWakeSubscriber(ctx context.Context, rdb redis.UniversalClient, onWake func(playerID uint64)) {
	h := plog.With(ctx)
	for ctx.Err() == nil {
		sub := rdb.Subscribe(ctx, wakeChannel)
		ch := sub.Channel()
		h.Infow("msg", "push_wake_subscriber_started", "channel", wakeChannel)
		for msg := range ch {
			playerID, perr := strconv.ParseUint(msg.Payload, 10, 64)
			if perr != nil || playerID == 0 {
				h.Warnw("msg", "push_wake_bad_payload", "payload", msg.Payload)
				continue
			}
			onWake(playerID)
		}
		_ = sub.Close()
		if ctx.Err() != nil {
			return
		}
		h.Warnw("msg", "push_wake_subscriber_disconnected_retry", "backoff", "1s")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
