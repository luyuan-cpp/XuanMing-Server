// Package kafkax — Producer
//
// 来源:抽自 mmorpg/go/login/internal/kafka/key_ordered_producer.go,
// **剥业务依赖**(db_proto / consistent 内部包路径),保留:
//   - SyncProducer + idempotent 配置
//   - 一致性哈希按 key 路由 partition
//   - 内置 payloadPool 减少 GC
//
// **W1-D2 阶段**:不实现 retry queue + plainProducer + DLQ,留 W2 battle_result 时再补。
package kafkax

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IBM/sarama"
	klog "github.com/go-kratos/kratos/v2/log"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/config"
)

// ProducerMeta 是发送消息的元数据,用于 payload 回收。
type ProducerMeta struct {
	producer *KeyOrderedProducer
	payload  []byte
}

// KeyOrderedProducer 是基于 SyncProducer 的 key-ordered 幂等生产者。
// 同一 key 永远落同一个 partition,partition 内 sarama 保序。
type KeyOrderedProducer struct {
	producer     sarama.SyncProducer
	client       sarama.Client
	topic        string
	partitionCnt int
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	consistent   *Consistent
	closed       bool
	payloadPool  sync.Pool

	successCount int64
	errorCount   int64
}

// NewKeyOrderedProducer 用 config.KafkaConfig + topic 创建生产者。
func NewKeyOrderedProducer(cfg config.KafkaConfig, topic string) (*KeyOrderedProducer, error) {
	c := sarama.NewConfig()
	c.Version = sarama.V3_6_0_0
	c.Net.DialTimeout = cfg.DialTimeout.Std()
	c.Net.ReadTimeout = cfg.ReadTimeout.Std()
	c.Net.WriteTimeout = cfg.WriteTimeout.Std()
	c.Producer.Return.Successes = true
	c.Producer.Return.Errors = true
	c.Producer.Retry.Max = cfg.RetryMax
	c.Producer.Retry.Backoff = cfg.RetryBackoff.Std()
	c.Producer.RequiredAcks = sarama.WaitForAll
	c.ChannelBufferSize = cfg.ChannelBuffer
	c.Producer.Compression = cfg.ParseCompression()
	c.Producer.Idempotent = cfg.Idempotent
	c.Net.MaxOpenRequests = 1

	if err := c.Validate(); err != nil {
		klog.Errorf("[kafkax] invalid sarama config: %v", err)
		return nil, err
	}

	client, err := sarama.NewClient(cfg.Brokers, c)
	if err != nil {
		klog.Errorf("[kafkax] new client failed: %v", err)
		return nil, fmt.Errorf("new client: %w", err)
	}

	producer, err := sarama.NewSyncProducerFromClient(client)
	if err != nil {
		_ = client.Close()
		klog.Errorf("[kafkax] new sync producer failed: %v", err)
		return nil, fmt.Errorf("new sync producer: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	p := &KeyOrderedProducer{
		producer:     producer,
		client:       client,
		topic:        topic,
		partitionCnt: int(cfg.PartitionCnt),
		ctx:          ctx,
		cancel:       cancel,
		consistent:   NewConsistent(),
		payloadPool: sync.Pool{
			New: func() any { return make([]byte, 0, 1024) },
		},
	}

	// 初始 partition 注入哈希环
	for i := int32(0); i < cfg.PartitionCnt; i++ {
		p.consistent.AddPartition(i)
	}

	klog.Infof("[kafkax] producer ready: topic=%s partitions=%d idempotent=%v",
		topic, cfg.PartitionCnt, cfg.Idempotent)
	return p, nil
}

// Send 把 proto 消息序列化后,按 key 路由到 partition 发送。
//
// 调用方:`producer.Send(ctx, "M_xxx", &BattleResult{...})`
func (p *KeyOrderedProducer) Send(ctx context.Context, key string, msg proto.Message) error {
	if p.isClosed() {
		return fmt.Errorf("producer closed")
	}

	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal proto: %w", err)
	}

	partition, ok := p.consistent.GetPartition(key)
	if !ok {
		return fmt.Errorf("no partition (hash ring empty)")
	}

	pm := &sarama.ProducerMessage{
		Topic:     p.topic,
		Key:       sarama.StringEncoder(key),
		Value:     sarama.ByteEncoder(payload),
		Partition: partition,
		Timestamp: time.Now(),
	}

	_, _, err = p.producer.SendMessage(pm)
	if err != nil {
		atomic.AddInt64(&p.errorCount, 1)
		return fmt.Errorf("send: %w", err)
	}
	atomic.AddInt64(&p.successCount, 1)
	return nil
}

// SendRaw 直接发字节(不序列化)。
func (p *KeyOrderedProducer) SendRaw(ctx context.Context, key string, payload []byte) error {
	if p.isClosed() {
		return fmt.Errorf("producer closed")
	}

	partition, ok := p.consistent.GetPartition(key)
	if !ok {
		return fmt.Errorf("no partition")
	}

	pm := &sarama.ProducerMessage{
		Topic:     p.topic,
		Key:       sarama.StringEncoder(key),
		Value:     sarama.ByteEncoder(payload),
		Partition: partition,
		Timestamp: time.Now(),
	}

	if _, _, err := p.producer.SendMessage(pm); err != nil {
		atomic.AddInt64(&p.errorCount, 1)
		return fmt.Errorf("send: %w", err)
	}
	atomic.AddInt64(&p.successCount, 1)
	return nil
}

// PushToPlayers 把同一份 payload 按 player_id 路由分发到 N 个玩家(W3 ④,2026-06-05)。
//
// 这是 push 推送的统一入口,**业务服必须走本方法**,review 时只看一处:
//
//  1. 自动排除 callerPlayerID(原则 2:发起方不收自己触发的 push,看 RPC response 即可)
//     - 例外:已受理型 RPC(MatchProgressEvent 等)需要发给所有人含发起方,
//     传 callerPlayerID = 0 跳过排除
//
//  2. 每个目标 player_id 用 SendRaw 发一次,kafka key = strconv.FormatInt(playerID, 10),
//     一致性哈希保证同玩家事件落同一 partition,partition 内 sarama 保序(不变量 §9)
//
//  3. 失败 log+continue 不阻断:某玩家发失败不能影响其他玩家;返回 (sent, lastErr),
//     调用方决定是否汇报(本批 W3 ④ 只让业务侧调用方记日志,不上抛业务错误)
//
// 调用示例(team 服务广播队员变更):
//
//	memberIDs := []int64{1001, 1002, 1003}
//	payload, _ := proto.Marshal(&teamv1.TeamUpdateEvent{...})
//	producer.PushToPlayers(ctx, callerID, memberIDs, payload)
func (p *KeyOrderedProducer) PushToPlayers(
	ctx context.Context,
	callerPlayerID int64,
	toPlayerIDs []int64,
	payload []byte,
) (sent int, lastErr error) {
	for _, pid := range toPlayerIDs {
		if pid == callerPlayerID {
			// 原则 2:不发给发起方;callerPlayerID=0 时该条件永不满足 → 全发(原则 3 例外)
			continue
		}
		if err := p.SendRaw(ctx, strconv.FormatInt(pid, 10), payload); err != nil {
			klog.Warnf("[kafkax] push_to_players send_failed topic=%s player_id=%d err=%v",
				p.topic, pid, err)
			lastErr = err
			continue
		}
		sent++
	}
	return sent, lastErr
}

// Close 优雅关闭。
func (p *KeyOrderedProducer) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	p.cancel()
	if err := p.producer.Close(); err != nil {
		klog.Errorf("[kafkax] producer close: %v", err)
	}
	if err := p.client.Close(); err != nil {
		klog.Errorf("[kafkax] client close: %v", err)
	}

	klog.Infof("[kafkax] producer closed: topic=%s success=%d error=%d",
		p.topic, atomic.LoadInt64(&p.successCount), atomic.LoadInt64(&p.errorCount))
	return nil
}

// Stats 返回成功 / 失败计数(累计)。
func (p *KeyOrderedProducer) Stats() (success, errCount int64) {
	return atomic.LoadInt64(&p.successCount), atomic.LoadInt64(&p.errorCount)
}

func (p *KeyOrderedProducer) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}
