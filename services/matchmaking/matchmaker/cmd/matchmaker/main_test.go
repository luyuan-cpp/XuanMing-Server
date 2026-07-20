package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pconfig "github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/kafkax"
)

type fakeMatchProducer struct {
	pushCalls int
	closed    bool
}

func (f *fakeMatchProducer) PushToPlayers(_ context.Context, _ uint64, to []uint64, _ []byte) (int, error) {
	f.pushCalls++
	return len(to), nil
}

func (f *fakeMatchProducer) Close() error {
	f.closed = true
	return nil
}

func TestInitializeMatchPublicationAllowsExplicitNoKafkaDevMode(t *testing.T) {
	called := false
	got, err := initializeMatchPublication(pconfig.KafkaConfig{Brokers: []string{"", "  "}},
		func(pconfig.KafkaConfig, string) (rawMatchProducer, error) {
			called = true
			return nil, nil
		})
	if err != nil {
		t.Fatalf("initializeMatchPublication: %v", err)
	}
	if called {
		t.Fatal("空 broker 配置不应调用 producer factory")
	}
	if got.producer != nil || got.pusher != nil || got.disabledReason == "" {
		t.Fatalf("显式无 Kafka 模式返回异常: %+v", got)
	}
}

func TestInitializeMatchPublicationRejectsConfiguredKafkaFailure(t *testing.T) {
	wantErr := errors.New("broker unavailable")
	got, err := initializeMatchPublication(pconfig.KafkaConfig{Brokers: []string{"kafka:9092"}},
		func(pconfig.KafkaConfig, string) (rawMatchProducer, error) {
			return nil, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v, want wrapped %v", err, wantErr)
	}
	if got.producer != nil || got.pusher != nil {
		t.Fatalf("初始化失败不能返回可用 publication: %+v", got)
	}
	if !strings.Contains(err.Error(), kafkax.TopicMatchProgress) {
		t.Fatalf("错误必须标明 topic, got %q", err)
	}
}

func TestInitializeMatchPublicationRejectsNilProducer(t *testing.T) {
	_, err := initializeMatchPublication(pconfig.KafkaConfig{Brokers: []string{"kafka:9092"}},
		func(pconfig.KafkaConfig, string) (rawMatchProducer, error) {
			return nil, nil
		})
	if err == nil || !strings.Contains(err.Error(), "factory returned nil") {
		t.Fatalf("nil producer 应拒绝启动, got %v", err)
	}
}

func TestInitializeMatchPublicationWiresPusher(t *testing.T) {
	producer := &fakeMatchProducer{}
	var gotTopic string
	got, err := initializeMatchPublication(pconfig.KafkaConfig{Brokers: []string{"kafka:9092"}},
		func(_ pconfig.KafkaConfig, topic string) (rawMatchProducer, error) {
			gotTopic = topic
			return producer, nil
		})
	if err != nil {
		t.Fatalf("initializeMatchPublication: %v", err)
	}
	if gotTopic != kafkax.TopicMatchProgress {
		t.Fatalf("topic=%q, want %q", gotTopic, kafkax.TopicMatchProgress)
	}
	if got.producer != producer || got.pusher == nil {
		t.Fatalf("producer/pusher 未正确装配: %+v", got)
	}

	// 原则 3 例外:match 进度 callerPlayerID=0 → 发给所有人(含发起方)。
	if sent, pushErr := got.pusher.PushMatchProgress(context.Background(), 0, []uint64{1001, 2002}, []byte("ready")); pushErr != nil || sent != 2 {
		t.Fatalf("PushMatchProgress sent=%d err=%v", sent, pushErr)
	}
	if producer.pushCalls != 1 {
		t.Fatalf("PushToPlayers 未透传: calls=%d", producer.pushCalls)
	}
}
