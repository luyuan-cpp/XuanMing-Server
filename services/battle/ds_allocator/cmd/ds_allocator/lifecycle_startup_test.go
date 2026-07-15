package main

import (
	"context"
	"errors"
	"testing"

	pconfig "github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/conf"
)

type fakeRawLifecycleProducer struct {
	key     string
	payload []byte
	closed  bool
}

func (f *fakeRawLifecycleProducer) SendRaw(_ context.Context, key string, payload []byte) error {
	f.key = key
	f.payload = append([]byte(nil), payload...)
	return nil
}

func (f *fakeRawLifecycleProducer) Close() error {
	f.closed = true
	return nil
}

func requiredLifecycleConfig() conf.Config {
	cfg := conf.Config{Mode: conf.ModeAgones}
	cfg.DSAuth.AuthorityMode = "redis"
	cfg.DSAuth.Mode = "enforce"
	return cfg
}

func TestInitializeLifecyclePublicationRequiredMissingFailsClosed(t *testing.T) {
	cfg := requiredLifecycleConfig()
	called := false
	_, err := initializeLifecyclePublication(cfg, func(pconfig.KafkaConfig, string) (rawLifecycleProducer, error) {
		called = true
		return &fakeRawLifecycleProducer{}, nil
	})
	if err == nil {
		t.Fatal("required lifecycle publication without brokers must fail startup")
	}
	if called {
		t.Fatal("factory must not be called without a configured broker")
	}
}

func TestInitializeLifecyclePublicationRequiredInitFailureFailsClosed(t *testing.T) {
	cfg := requiredLifecycleConfig()
	cfg.Kafka.Brokers = []string{"kafka:9092"}
	_, err := initializeLifecyclePublication(cfg, func(pconfig.KafkaConfig, string) (rawLifecycleProducer, error) {
		return nil, errors.New("dial failed")
	})
	if err == nil {
		t.Fatal("required lifecycle producer initialization failure must fail startup")
	}
}

func TestInitializeLifecyclePublicationLocalOffMayWarnAndContinue(t *testing.T) {
	cfg := conf.Config{Mode: conf.ModeLocal}
	cfg.DSAuth.AuthorityMode = "legacy"
	cfg.DSAuth.Mode = "off"
	cfg.Kafka.Brokers = []string{"kafka:9092"}
	got, err := initializeLifecyclePublication(cfg, func(pconfig.KafkaConfig, string) (rawLifecycleProducer, error) {
		return nil, errors.New("developer broker is offline")
	})
	if err != nil {
		t.Fatalf("explicit local/off degradation should not fail startup: %v", err)
	}
	if got.pusher != nil || got.producer != nil || got.disabledReason == "" {
		t.Fatalf("local/off degradation must carry an explicit warning reason: %+v", got)
	}
}

func TestInitializeLifecyclePublicationReady(t *testing.T) {
	cfg := requiredLifecycleConfig()
	cfg.Kafka.Brokers = []string{"kafka:9092"}
	producer := &fakeRawLifecycleProducer{}
	got, err := initializeLifecyclePublication(cfg, func(_ pconfig.KafkaConfig, topic string) (rawLifecycleProducer, error) {
		if topic != kafkax.TopicDSLifecycle {
			t.Fatalf("topic = %q, want %q", topic, kafkax.TopicDSLifecycle)
		}
		return producer, nil
	})
	if err != nil {
		t.Fatalf("initialize lifecycle publication: %v", err)
	}
	if got.pusher == nil || got.producer != producer {
		t.Fatalf("publisher was not installed: %+v", got)
	}
	if err := got.pusher.PublishLifecycle(context.Background(), &dsv1.DSLifecycleEvent{MatchId: 42}); err != nil {
		t.Fatalf("publish through adapter: %v", err)
	}
	if producer.key != "42" || len(producer.payload) == 0 {
		t.Fatalf("adapter did not key/marshal event: key=%q payload=%d", producer.key, len(producer.payload))
	}
}
