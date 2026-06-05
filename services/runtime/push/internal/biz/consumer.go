// Package biz —— W3 ④(2026-06-05)KafkaConsumer 包装层。
//
// 职责:订阅一个 push topic,把 kafka 消息转为 PushFrame,在线玩家路由到 ConnectionManager.SendTo,
// 离线玩家写入 OfflineCacheRepo(redis ZSET)。
//
// 设计要点:
//   - **每个 topic 一个 consumer**,共享同一 GroupID(简化,后期可重构为单 consumer 多 topic)
//   - kafka key 必须是 strconv.FormatUint(player_id, 10)(不变量 §9);非数字 key log + ack 跳过
//     避免单条脏数据阻塞整个 partition
//   - trace_id 从 sarama.Headers["trace_id"] 取(业务 producer 没塞则空,允许)
//   - Handler 返回 nil → ack(W1-D2 简化策略,W2 battle_result 时再补 retry queue)
//
// 不变量 §1 落地:同 player_id 多 partition 路由不会发生(一致性哈希),
// 同时 ConnectionManager.Register 已自动顶号(W2 ⑤),所以 SendTo 永远落到当前唯一 stream。
package biz

import (
	"context"
	"errors"
	"strconv"

	"github.com/IBM/sarama"

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

// KafkaConsumer 包装一个 topic 的消费循环。
type KafkaConsumer struct {
	topic    string
	cm       FrameSender
	offline  data.OfflineCacheRepo
	consumer *kafkax.KeyOrderedConsumer
}

// NewKafkaConsumer 构造但不启动;调用 Start() 才开始消费。
//
// brokers / groupID / partitionCnt 由 cfg.Kafka 提供。
func NewKafkaConsumer(
	brokers []string,
	groupID string,
	topic string,
	partitionCnt int32,
	cm FrameSender,
	offline data.OfflineCacheRepo,
) (*KafkaConsumer, error) {
	if cm == nil {
		return nil, errors.New("FrameSender must not be nil")
	}
	if offline == nil {
		return nil, errors.New("OfflineCacheRepo must not be nil")
	}
	if topic == "" {
		return nil, errors.New("topic must not be empty")
	}

	kc := &KafkaConsumer{topic: topic, cm: cm, offline: offline}

	c, err := kafkax.NewKeyOrderedConsumer(kafkax.ConsumerConfig{
		Brokers:        brokers,
		Topic:          topic,
		GroupID:        groupID,
		PartitionCount: partitionCnt,
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
// 返回值约定:
//   - nil → kafkax 走 ack(常规路径:在线发送成功、离线写入成功、key 非数字跳过)
//   - errcode.ErrPushOfflineCorrupted (9301) → offline.Append 失败(redis 不可达)。
//     kafkax 配置为 ack-all,本帧仍会 commit offset,客户端按 last_seen_ms 也补不回 →
//     有损推送,由 pandora_push_offline_append_failed_total 计数器触发告警。
//
// 错误情况下不阻塞 partition(kafkax 已经统一 ack 策略,这里返 errcode 仅为可观测 + 单测断言)。
//
// W3 ④ 二次修复(Opus 审查 R2):offline.Append 失败由"只 log"升级为"metric 计数 + 返 errcode",
// 修复"redis down → 静默丢消息"的隐患。
func (k *KafkaConsumer) handle(ctx context.Context, msg *sarama.ConsumerMessage) error {
	h := plog.With(ctx)

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
		return nil
	}

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

// 让 *ConnectionManager 自动满足 FrameSender(编译期检查)。
var _ FrameSender = (*ConnectionManager)(nil)
