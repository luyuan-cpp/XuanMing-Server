package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	klog "github.com/go-kratos/kratos/v2/log"

	"github.com/luyuancpp/pandora/pkg/config"
	datav1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/data_service/v1"
	"github.com/luyuancpp/pandora/services/data/data_service/internal/conf"
)

type authorityReadStore struct {
	data      *datav1.PlayerData
	readCalls int
}

func (s *authorityReadStore) Read(context.Context, uint64) (*datav1.PlayerData, bool, error) {
	s.readCalls++
	return s.data, true, nil
}

func (*authorityReadStore) Write(context.Context, *datav1.PlayerData, []string) (uint32, error) {
	return 0, errors.New("unexpected Write")
}

type failingReadCache struct {
	getErr   error
	setErr   error
	getCalls int
	setCalls int
}

func (c *failingReadCache) Get(context.Context, uint64) (*datav1.PlayerData, bool, error) {
	c.getCalls++
	return nil, false, c.getErr
}

func (c *failingReadCache) Set(context.Context, *datav1.PlayerData, time.Duration) error {
	c.setCalls++
	return c.setErr
}

func (*failingReadCache) Del(context.Context, uint64) error { return nil }

func TestReadPlayer_CacheFailuresDoNotMaskAuthorityResult(t *testing.T) {
	store := &authorityReadStore{data: &datav1.PlayerData{
		PlayerId: 7,
		Version:  3,
		Nickname: "mysql-authority",
	}}
	cache := &failingReadCache{
		getErr: errors.New("redis read timeout"),
		setErr: errors.New("redis backfill timeout"),
	}
	uc := NewDataUsecase(store, cache, conf.DataConf{
		CacheTTL: config.Duration(time.Minute),
	}, klog.DefaultLogger)

	got, found, err := uc.ReadPlayer(context.Background(), 7)
	if err != nil {
		t.Fatalf("cache failures must not mask MySQL success: %v", err)
	}
	if !found || got.GetNickname() != "mysql-authority" || got.GetVersion() != 3 {
		t.Fatalf("authority result lost: found=%v data=%+v", found, got)
	}
	if cache.getCalls != 1 || store.readCalls != 1 || cache.setCalls != 1 {
		t.Fatalf("call chain get/store/backfill=%d/%d/%d want=1/1/1",
			cache.getCalls, store.readCalls, cache.setCalls)
	}
}
