// consumer_test.go — player.update 消费 handler 的 event_type 路由单测(实时成长)。
//
// 关键回归:pandora.player.update 同 topic 现在承载多种事件(0=MMR 旧事件,1=经验),
// MMR 消费者必须按 kafka header 跳过非 0 事件 —— 否则 PlayerExperienceEvent 按
// PlayerUpdateEvent 反序列化会因 wire type 冲突判毒丸进 DLQ。
package biz

import (
	"context"
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
