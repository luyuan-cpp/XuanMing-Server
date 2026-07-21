// consumer_test.go — player.update 消费 handler 的 event_type 防御单测(实时成长)。
//
// pandora.player.update 是单事件类型 topic(经验事件走独立 topic pandora.player.experience,
// 金丝雀混跑安全,见 kafkax.TopicPlayerUpdate 注释)。本消费者仍防御性校验 header:
// 合法非 0 → 跳过;存在但非法 → 毒丸进 DLQ(绝不能降级按 MMR 解码)。
package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/IBM/sarama"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/kafkax"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
	"github.com/luyuancpp/pandora/services/account/player/internal/conf"
)

func TestPlayerUpdateHandler_SkipsNonLegacyEventType(t *testing.T) {
	repo := newFakeRepo()
	uc := NewPlayerUsecase(repo, conf.PlayerConf{})
	handler := uc.PlayerUpdateHandler()

	evt := &playerv1.PlayerExperienceEvent{PlayerId: 7, Level: 3, ExpInLevel: 50, LevelsGained: 1}
	payload, err := proto.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg := &sarama.ConsumerMessage{
		Value: payload,
		Headers: []*sarama.RecordHeader{{
			Key:   []byte(kafkax.HeaderEventType),
			Value: []byte("1"),
		}},
	}
	if err := handler(context.Background(), msg); err != nil {
		t.Fatalf("experience event must be skipped silently, got %v", err)
	}
	if len(repo.idem) != 0 {
		t.Fatalf("experience event must not trigger UpdateMMR, idem=%v", repo.idem)
	}
}

func TestPlayerUpdateHandler_LegacyEventStillConsumed(t *testing.T) {
	repo := newFakeRepo()
	uc := NewPlayerUsecase(repo, conf.PlayerConf{})
	handler := uc.PlayerUpdateHandler()

	evt := &playerv1.PlayerUpdateEvent{PlayerId: 7, MatchId: 42, MmrDelta: 25, Reason: "win"}
	payload, err := proto.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// 无 header(旧 producer)→ event_type=0 → 正常消费。
	if err := handler(context.Background(), &sarama.ConsumerMessage{Value: payload}); err != nil {
		t.Fatalf("legacy event: %v", err)
	}
	if len(repo.idem) != 1 {
		t.Fatalf("legacy event must apply UpdateMMR once, idem=%v", repo.idem)
	}
}

func TestPlayerUpdateHandler_MalformedEventTypeIsPoison(t *testing.T) {
	repo := newFakeRepo()
	uc := NewPlayerUsecase(repo, conf.PlayerConf{})
	handler := uc.PlayerUpdateHandler()

	// 恰好能按 PlayerUpdateEvent 解码的 payload + 非法 header:必须判毒丸,
	// 不能降级当旧事件消费(降级 = 未知语义消息改写 MMR)。
	evt := &playerv1.PlayerUpdateEvent{PlayerId: 7, MatchId: 42, MmrDelta: 25, Reason: "win"}
	payload, err := proto.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, bad := range []string{"abc", "-1", "99999999999999999999"} {
		msg := &sarama.ConsumerMessage{
			Value: payload,
			Headers: []*sarama.RecordHeader{{
				Key:   []byte(kafkax.HeaderEventType),
				Value: []byte(bad),
			}},
		}
		err := handler(context.Background(), msg)
		var poison *kafkax.PoisonError
		if err == nil || !errors.As(err, &poison) {
			t.Fatalf("malformed header %q must be poison, got %v", bad, err)
		}
	}
	if len(repo.idem) != 0 {
		t.Fatalf("malformed header must not trigger UpdateMMR, idem=%v", repo.idem)
	}
}
