// wake_test.go — 跨 Pod 唤醒信号回归(R5 复审 P2-10)。
package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 发布端 PUBLISH 的 player_id 必须被订阅端解析并回调;坏 payload 跳过不中断订阅。
func TestWakeSignal_PublishReachesSubscriber(t *testing.T) {
	mr := miniredis.RunT(t)
	pubClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	subClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = pubClient.Close(); _ = subClient.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	woken := make(chan uint64, 8)
	go RunWakeSubscriber(ctx, subClient, func(playerID uint64) { woken <- playerID })

	// 订阅建立是异步的:重试发布直到信号被消费(miniredis 无订阅就绪回执)。
	sig := NewRedisWakeSignal(pubClient)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := sig.PublishWake(ctx, 77); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case got := <-woken:
			if got != 77 {
				t.Fatalf("want wake for 77, got %d", got)
			}
			return
		case <-time.After(50 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("wake signal did not reach subscriber")
			}
		}
	}
}
