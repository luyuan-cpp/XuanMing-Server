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
//   - Handler 返回非 nil → kafkax 按 RetryPolicy 重试,耗尽后投 DLQ(main.go 装配,2026-07-06)
//
// 不变量 §1 落地:同 player_id 多 partition 路由不会发生(一致性哈希),
// 同时 ConnectionManager.Register 已自动顶号(W2 ⑤),所以 SendTo 永远落到当前唯一 stream。
package biz

import (
	"context"
	"errors"
	"strconv"

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
// retry / dlq:业务瞬时错误(典型是 offline.Append 碰到 redis 抖动)进程内重试,
// 耗尽后投 DLQ(可回放,不再静默丢帧);dlq 为 nil 时退化为 log+ack(仅供单测/联调)。
func NewKafkaConsumer(
	brokers []string,
	groupID string,
	topic string,
	partitionCnt int32,
	cm FrameRouter,
	offline data.OfflineCacheRepo,
	retry kafkax.RetryPolicy,
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
		RetryPolicy:    retry,
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
// 返回值约定:
//   - nil → kafkax 走 ack(常规路径:在线发送成功、离线写入成功)
//   - kafkax.Poison(err) → key 非 player_id 数字(毒丸,重试无意义)直接投 DLQ 留证
//   - errcode.ErrPushOfflineCorrupted (9301) → offline.Append 失败(redis 不可达)。
//     kafkax 按 RetryPolicy 进程内重试(瞬时抖动自愈),耗尽后投 DLQ 可回放;
//     同时 pandora_push_offline_append_failed_total 计数器触发告警。
//
// W3 ④ 二次修复(Opus 审查 R2):offline.Append 失败由“只 log”升级为“metric 计数 + 返 errcode”。
// 2026-07-06 三次修复:接入 kafkax RetryPolicy + DLQ,彻底消除“redis down → ack 丢帧”。
func (k *KafkaConsumer) handle(ctx context.Context, msg *sarama.ConsumerMessage) error {
	h := plog.With(ctx)

	// 广播类 topic(chat.world / system.notify):key 为空,给全部在线玩家 Broadcast。
	// 不写离线缓存(广播无 per-player 归属;离线玩家重连后不补推全服广播,避免历史公告刷屏)。
	if k.broadcast {
		// frame 是向所有当前连接复用的只读推送帧;EventType 从 Kafka header 解析并保留 0 兼容值。
		frame := &pushv1.PushFrame{
			Topic:     msg.Topic,
			Payload:   msg.Value,
			TsMs:      msg.Timestamp.UnixMilli(),
			TraceId:   headerStr(msg.Headers, "trace_id"),
			EventType: headerUint32(msg.Headers, kafkax.HeaderEventType),
		}
		// sent 与 failed 分别记录本次广播成功、失败的在线连接数,仅部分失败需要告警。
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
		return kafkax.Poison(err)
	}

	// 分片:多 Cell 下本实例只应拥有 owner cell == 本 cell 的玩家;收到非本 cell 玩家消息
	// 说明路由漂移 / rebalance,仅告警不阻断(本地交付仍正确),转投属基础设施。router 为 nil
	// (单 Cell)→ 不告警。
	k.guardPlayerOwnership(ctx, playerID, msg.Topic)

	// 2. 构 PushFrame(payload 直接是业务 Event proto bytes;ts_ms 取 kafka 消息时间)
	// frame 保留业务 payload 原字节,只补齐 topic、时间、链路与域内事件类型等路由元数据。
	frame := &pushv1.PushFrame{
		Topic:     msg.Topic,
		Payload:   msg.Value,
		TsMs:      msg.Timestamp.UnixMilli(),
		TraceId:   headerStr(msg.Headers, "trace_id"),
		EventType: headerUint32(msg.Headers, kafkax.HeaderEventType),
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

// headerUint32 在 sarama.RecordHeader 列表里找指定 key 并解析为 uint32
// (找不到 / 解析失败 → 返 0,即该 topic 的旧事件类型,向后兼容)。
// 用于把业务 producer 塞的 event_type header 透传进 PushFrame.EventType。
// headers 是 Kafka 原始 header 列表,key 是待查 header 名;本函数不区分缺失与畸形值,
// 两者都必须收敛为兼容值 0,避免坏 header 让整条业务推送进入 poison/DLQ。
func headerUint32(headers []*sarama.RecordHeader, key string) uint32 {
	// s 是目标 header 的十进制文本;空值同时覆盖 header 缺失和值为空。
	s := headerStr(headers, key)
	if s == "" {
		return 0
	}
	// v 是限定到 32 位后的无符号事件类型,err 表示非十进制、负数或越界。
	v, err := strconv.ParseUint(s, 10, 32)
	// 解析失败不能阻断旧客户端兼容路径,按未声明 event_type 处理。
	if err != nil {
		return 0
	}
	// ParseUint 已完成 32 位范围校验,此处转换不会截断。
	return uint32(v)
}

// 让 *ConnectionManager 自动满足 FrameSender / FrameRouter(编译期检查)。
var (
	_ FrameSender = (*ConnectionManager)(nil)
	_ FrameRouter = (*ConnectionManager)(nil)
)
