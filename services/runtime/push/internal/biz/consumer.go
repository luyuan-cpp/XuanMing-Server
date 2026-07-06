// Package biz —— W3 ④(2026-06-05)KafkaConsumer 包装层。
//
// 职责:订阅一个 push topic,把 kafka 消息转为 PushFrame,在线玩家路由到 ConnectionManager.SendTo,
// 离线玩家写入 OfflineCacheRepo(redis ZSET)。
//
// 设计要点:
//   - **每个 topic 一个 consumer**,共享同一 GroupID(简化,后期可重构为单 consumer 多 topic)
//   - kafka key 必须是 strconv.FormatUint(player_id, 10)(不变量 §9);非数字 key 作为毒丸进 DLQ 留证,
//     避免单条脏数据静默丢失或阻塞整个 partition
//   - trace_id 从 sarama.Headers["trace_id"] 取(业务 producer 没塞则空,允许)
//   - Handler 返回 nil → ack;返回 error 时按 kafkax RetryPolicy / DLQ 处理
//
// 不变量 §1 落地:同 player_id 多 partition 路由不会发生(一致性哈希),
// 同时 ConnectionManager.Register 已自动顶号(W2 ⑤),所以 SendTo 永远落到当前唯一 stream。
package biz

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/IBM/sarama"

	"github.com/luyuancpp/pandora/pkg/cellroute"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/data"
)

// FrameSender 抽象 ConnectionManager.SendTo(便于 consumer 单测注入)。
type FrameSender interface {
	SendTo(playerID uint64, frame *pushv1.PushFrame) (online bool, err error)
}

// FrameBroadcaster 抽象 ConnectionManager.Broadcast(广播类 topic 用,便于单测注入)。
type FrameBroadcaster interface {
	Broadcast(frame *pushv1.PushFrame) (sent int, failed int)
}

// FrameRouter 是 SendTo + Broadcast 的并集;ConnectionManager 同时满足两者。
// KafkaConsumer 按 topic 是否广播类选择 SendTo / Broadcast。
type FrameRouter interface {
	FrameSender
	FrameBroadcaster
}

// Kafka 消费失败处理:业务瞬时错误(offline.Append 失败等)进程内重试
// retryMaxAttempts 次(间隔 retryBackoff)后进 DLQ,与 battle_result 同策略。
const (
	retryMaxAttempts = 3
	retryBackoff     = 500 * time.Millisecond
)

// KafkaConsumer 包装一个 topic 的消费循环。
type KafkaConsumer struct {
	topic     string
	broadcast bool // 广播类 topic(chat.world / system.notify):走 cm.Broadcast,不按 player_id key 解析
	cm        FrameRouter
	offline   data.OfflineCacheRepo
	consumer  *kafkax.KeyOrderedConsumer

	// router / selfRegion / selfCell:本 push 实例所在 cell 的身份 + 确定性路由器
	// (scale-cellular-20m.md §4.2)。router 为 nil = 单 Cell,本实例拥有全部玩家,handle 不做
	// 归属漂移告警(行为不变)。分片部署时由 main 经 SetCellOwnership 注入。nil-safe。
	router     *cellroute.Router
	selfRegion uint32
	selfCell   uint32
}

// NewKafkaConsumer 构造但不启动;调用 Start() 才开始消费。
//
// brokers / groupID / partitionCnt 由 cfg.Kafka 提供。
// 广播类 topic(kafkax.IsBroadcastTopic)在 handle 里走 cm.Broadcast,不依赖 player_id key。
// dlq 非 nil 时:业务瞬时错误(如 offline.Append 失败)重试 retryMaxAttempts 次后投 DLQ,
// DLQ 投递失败不 ack 重放(at-least-once,不丢帧);毒丸 key 直接进 DLQ 留证。
func NewKafkaConsumer(
	brokers []string,
	groupID string,
	topic string,
	partitionCnt int32,
	cm FrameRouter,
	offline data.OfflineCacheRepo,
	dlq kafkax.DLQProducer,
) (*KafkaConsumer, error) {
	if cm == nil {
		return nil, errors.New("FrameRouter must not be nil")
	}
	if offline == nil {
		return nil, errors.New("OfflineCacheRepo must not be nil")
	}
	if topic == "" {
		return nil, errors.New("topic must not be empty")
	}

	kc := &KafkaConsumer{topic: topic, broadcast: kafkax.IsBroadcastTopic(topic), cm: cm, offline: offline}

	c, err := kafkax.NewKeyOrderedConsumer(kafkax.ConsumerConfig{
		Brokers:        brokers,
		Topic:          topic,
		GroupID:        groupID,
		PartitionCount: partitionCnt,
		RetryPolicy:    kafkax.RetryPolicy{MaxRetries: retryMaxAttempts, Backoff: retryBackoff},
		DLQ:            dlq,
	}, kc.handle)
	if err != nil {
		return nil, err
	}
	kc.consumer = c
	return kc, nil
}

// Start 启动消费循环。
func (k *KafkaConsumer) Start() { k.consumer.Start() }

// Close 优雅关闭。
func (k *KafkaConsumer) Close() error { return k.consumer.Close() }

// handle 是单条 kafka 消息的处理逻辑;暴露为方法便于单测直接调。
//
// 返回值约定(配合 kafkax RetryPolicy + DLQ):
//   - nil → ack(常规路径:在线发送成功、离线写入成功)
//   - kafkax.Poison(err) → key 非 player_id 数字(毒丸,重试无意义)直接投 DLQ 留证
//   - errcode.ErrPushOfflineCorrupted (9301) → offline.Append 失败(redis 不可达)。
//     kafkax 按 RetryPolicy 进程内重试,耗尽后投 DLQ;DLQ 投递失败不 ack 重放
//     (at-least-once,不再是有损推送)。pandora_push_offline_append_failed_total 仍计数告警。
func (k *KafkaConsumer) handle(ctx context.Context, msg *sarama.ConsumerMessage) error {
	h := plog.With(ctx)

	// 广播类 topic(chat.world / system.notify):key 为空,给全部在线玩家 Broadcast。
	// 不写离线缓存(广播无 per-player 归属;离线玩家重连后不补推全服广播,避免历史公告刷屏)。
	if k.broadcast {
		frame := &pushv1.PushFrame{
			Topic:   msg.Topic,
			Payload: msg.Value,
			TsMs:    msg.Timestamp.UnixMilli(),
			TraceId: headerStr(msg.Headers, "trace_id"),
		}
		sent, failed := k.cm.Broadcast(frame)
		if failed > 0 {
			h.Warnw("msg", "push_broadcast_partial_failed",
				"topic", msg.Topic, "sent", sent, "failed", failed)
		}
		return nil
	}

	// 1. 取 player_id(不变量 §9:key 必须是 player_id 序列化字符串)
	playerID, err := strconv.ParseUint(string(msg.Key), 10, 64)
	if err != nil {
		h.Warnw(
			"msg", "kafka_push_invalid_key",
			"topic", msg.Topic,
			"partition", msg.Partition,
			"offset", msg.Offset,
			"key", string(msg.Key),
			"err", err,
		)
		// 毒丸:key 不是 player_id,重试无意义 → 投 DLQ 留证(配了 DLQ 时),不静默丢。
		return kafkax.Poison(err)
	}

	// 分片:多 Cell 下本实例只应拥有 owner cell == 本 cell 的玩家;收到非本 cell 玩家消息
	// 说明路由漂移 / rebalance,仅告警不阻断(本地交付仍正确),转投属基础设施。router 为 nil
	// (单 Cell)→ 不告警。
	k.guardPlayerOwnership(ctx, playerID, msg.Topic)

	// 2. 构 PushFrame(payload 直接是业务 Event proto bytes;ts_ms 取 kafka 消息时间)
	frame := &pushv1.PushFrame{
		Topic:   msg.Topic,
		Payload: msg.Value,
		TsMs:    msg.Timestamp.UnixMilli(),
		TraceId: headerStr(msg.Headers, "trace_id"),
	}

	// 3. 路由:在线 → SendTo;离线或 send 失败 → 写 ZSET
	online, sendErr := k.cm.SendTo(playerID, frame)
	if online && sendErr == nil {
		// 成功交付,无需写 offline
		return nil
	}

	// 在线但 stream.Send 失败:帧未交付,写 offline 让客户端重连后通过 last_seen_ms 补推。
	// (client 端用 ts_ms + trace_id 做幂等判重,不依赖 push 侧规避双投递)
	if online && sendErr != nil {
		h.Warnw("msg", "push_send_failed_fallback_offline",
			"topic", msg.Topic, "player_id", playerID, "err", sendErr)
	}

	// offline:append 到 redis ZSET(在线 send 失败 / 玩家真离线均走此路径)
	if err := k.offline.Append(ctx, playerID, frame); err != nil {
		OfflineAppendFailed.WithLabelValues(msg.Topic).Inc()
		h.Errorw(
			"msg", "push_offline_append_failed",
			"topic", msg.Topic,
			"player_id", playerID,
			"code", errcode.ErrPushOfflineCorrupted,
			"err", err,
		)
		return errcode.New(errcode.ErrPushOfflineCorrupted, "offline append failed: %v", err)
	}
	return nil
}

// headerStr 在 sarama.RecordHeader 列表里找指定 key,返字符串(找不到返空)。
func headerStr(headers []*sarama.RecordHeader, key string) string {
	for _, h := range headers {
		if string(h.Key) == key {
			return string(h.Value)
		}
	}
	return ""
}

// 让 *ConnectionManager 自动满足 FrameSender / FrameRouter(编译期检查)。
var (
	_ FrameSender = (*ConnectionManager)(nil)
	_ FrameRouter = (*ConnectionManager)(nil)
)
