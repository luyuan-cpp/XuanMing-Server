package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pconfig "github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/kafkax"
)

type fakeTeamProducer struct {
	updateCalls int
	eventCalls  int
	eventType   uint32
	closed      bool
}

func (f *fakeTeamProducer) PushToPlayers(_ context.Context, _ uint64, to []uint64, _ []byte) (int, error) {
	f.updateCalls++
	return len(to), nil
}

func (f *fakeTeamProducer) PushToPlayersWithEventType(
	_ context.Context,
	_ uint64,
	to []uint64,
	_ []byte,
	eventType uint32,
) (int, error) {
	f.eventCalls++
	f.eventType = eventType
	return len(to), nil
}

func (f *fakeTeamProducer) Close() error {
	f.closed = true
	return nil
}

func TestInitializeTeamPublicationAllowsExplicitNoKafkaDevMode(t *testing.T) {
	called := false
	got, err := initializeTeamPublication(pconfig.KafkaConfig{Brokers: []string{"", "  "}},
		func(pconfig.KafkaConfig, string) (rawTeamProducer, error) {
			called = true
			return nil, nil
		})
	if err != nil {
		t.Fatalf("initializeTeamPublication: %v", err)
	}
	if called {
		t.Fatal("空 broker 配置不应调用 producer factory")
	}
	if got.producer != nil || got.pusher != nil || got.disabledReason == "" {
		t.Fatalf("显式无 Kafka 模式返回异常: %+v", got)
	}
}

func TestInitializeTeamPublicationRejectsConfiguredKafkaFailure(t *testing.T) {
	wantErr := errors.New("broker unavailable")
	got, err := initializeTeamPublication(pconfig.KafkaConfig{Brokers: []string{"kafka:9092"}},
		func(pconfig.KafkaConfig, string) (rawTeamProducer, error) {
			return nil, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v, want wrapped %v", err, wantErr)
	}
	if got.producer != nil || got.pusher != nil {
		t.Fatalf("初始化失败不能返回可用 publication: %+v", got)
	}
	if !strings.Contains(err.Error(), kafkax.TopicTeamUpdate) {
		t.Fatalf("错误必须标明 topic, got %q", err)
	}
}

func TestInitializeTeamPublicationRejectsNilProducer(t *testing.T) {
	_, err := initializeTeamPublication(pconfig.KafkaConfig{Brokers: []string{"kafka:9092"}},
		func(pconfig.KafkaConfig, string) (rawTeamProducer, error) {
			return nil, nil
		})
	if err == nil || !strings.Contains(err.Error(), "factory returned nil") {
		t.Fatalf("nil producer 应拒绝启动, got %v", err)
	}
}

func TestInitializeTeamPublicationWiresDedicatedEventType(t *testing.T) {
	producer := &fakeTeamProducer{}
	var gotTopic string
	got, err := initializeTeamPublication(pconfig.KafkaConfig{Brokers: []string{"kafka:9092"}},
		func(_ pconfig.KafkaConfig, topic string) (rawTeamProducer, error) {
			gotTopic = topic
			return producer, nil
		})
	if err != nil {
		t.Fatalf("initializeTeamPublication: %v", err)
	}
	if gotTopic != kafkax.TopicTeamUpdate {
		t.Fatalf("topic=%q, want %q", gotTopic, kafkax.TopicTeamUpdate)
	}
	if got.producer != producer || got.pusher == nil {
		t.Fatalf("producer/pusher 未正确装配: %+v", got)
	}

	const inviteEventType = uint32(1)
	if sent, pushErr := got.pusher.PushTeamEvent(context.Background(), 1001, []uint64{2002}, []byte("invite"), inviteEventType); pushErr != nil || sent != 1 {
		t.Fatalf("PushTeamEvent sent=%d err=%v", sent, pushErr)
	}
	if producer.eventCalls != 1 || producer.eventType != inviteEventType {
		t.Fatalf("event_type 未透传: calls=%d type=%d", producer.eventCalls, producer.eventType)
	}
}
