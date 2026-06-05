// W3 ④(2026-06-05)PushToPlayers helper 单测。
//
// 用 sarama/mocks.NewSyncProducer 直接注入 KeyOrderedProducer.producer 字段,
// 跳过 sarama.NewClient 真实连接 broker;**测试只能在 package kafkax 内**(访问未导出字段)。
package kafkax

import (
	"context"
	"errors"
	"testing"

	"github.com/IBM/sarama"
	"github.com/IBM/sarama/mocks"
)

// newTestProducer 构造一个不依赖 broker 的 KeyOrderedProducer,
// producer 字段用 sarama/mocks 注入,consistent 用 4 个虚拟 partition。
func newTestProducer(t *testing.T, mp sarama.SyncProducer) *KeyOrderedProducer {
	t.Helper()
	p := &KeyOrderedProducer{
		producer:     mp,
		topic:        "pandora.team.update",
		partitionCnt: 4,
		consistent:   NewConsistent(),
	}
	for i := int32(0); i < 4; i++ {
		p.consistent.AddPartition(i)
	}
	return p
}

// 用例 1:caller 在目标列表里,必须被跳过(只发剩下 2 个)。
func TestPushToPlayers_SkipsCaller(t *testing.T) {
	mp := mocks.NewSyncProducer(t, nil)
	defer func() { _ = mp.Close() }()

	// 预期 2 次 SendMessage 成功(玩家 200/300,跳过 caller=100)
	mp.ExpectSendMessageAndSucceed()
	mp.ExpectSendMessageAndSucceed()

	p := newTestProducer(t, mp)

	sent, err := p.PushToPlayers(context.Background(), 100, []uint64{100, 200, 300}, []byte("payload"))
	if err != nil {
		t.Fatalf("PushToPlayers err=%v", err)
	}
	if sent != 2 {
		t.Fatalf("sent=%d want=2", sent)
	}
}

// 用例 2:callerPlayerID=0 时不跳过任何人(原则 3 例外,匹配进度全发)。
func TestPushToPlayers_CallerZeroSendsAll(t *testing.T) {
	mp := mocks.NewSyncProducer(t, nil)
	defer func() { _ = mp.Close() }()

	mp.ExpectSendMessageAndSucceed()
	mp.ExpectSendMessageAndSucceed()
	mp.ExpectSendMessageAndSucceed()

	p := newTestProducer(t, mp)

	sent, err := p.PushToPlayers(context.Background(), 0, []uint64{1, 2, 3}, []byte("payload"))
	if err != nil {
		t.Fatalf("PushToPlayers err=%v", err)
	}
	if sent != 3 {
		t.Fatalf("sent=%d want=3", sent)
	}
}

// 用例 3:单条失败不阻断其他玩家;返回 sent + lastErr。
func TestPushToPlayers_PartialFailureContinues(t *testing.T) {
	mp := mocks.NewSyncProducer(t, nil)
	defer func() { _ = mp.Close() }()

	// 第 1 条成功,第 2 条失败,第 3 条成功
	mp.ExpectSendMessageAndSucceed()
	mp.ExpectSendMessageAndFail(errors.New("simulated broker err"))
	mp.ExpectSendMessageAndSucceed()

	p := newTestProducer(t, mp)

	sent, err := p.PushToPlayers(context.Background(), 0, []uint64{1, 2, 3}, []byte("payload"))
	if sent != 2 {
		t.Fatalf("sent=%d want=2", sent)
	}
	if err == nil {
		t.Fatal("expected lastErr non-nil")
	}
}

// 用例 4:目标全是 caller 自己 → sent=0,err=nil。
func TestPushToPlayers_AllCallerNoSend(t *testing.T) {
	mp := mocks.NewSyncProducer(t, nil)
	defer func() { _ = mp.Close() }()
	// 不 Expect 任何 Send

	p := newTestProducer(t, mp)

	sent, err := p.PushToPlayers(context.Background(), 100, []uint64{100}, []byte("payload"))
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	if sent != 0 {
		t.Fatalf("sent=%d want=0", sent)
	}
}
