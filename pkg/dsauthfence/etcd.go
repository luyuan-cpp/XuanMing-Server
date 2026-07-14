package dsauthfence

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type etcdBackend struct{ cli *clientv3.Client }

type etcdLease struct {
	cli         *clientv3.Client
	id          clientv3.LeaseID
	cancel      context.CancelFunc
	lost        chan struct{}
	lostOnce    sync.Once
	closeOnce   sync.Once
	intentional atomic.Bool
}

// Acquire 连接 etcd 并启动生产栅栏。所有启动期读取均为默认线性一致读。
func Acquire(ctx context.Context, cfg Config) (*Holder, error) {
	normalize(&cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	cli, err := newEtcdClient(cfg.Endpoints, cfg.DialTimeout, cfg.Prefix, cfg.Security)
	if err != nil {
		return nil, fmt.Errorf("dsauthfence: dial etcd: %w", err)
	}
	return Start(ctx, &etcdBackend{cli: cli}, cfg)
}

func (b *etcdBackend) GetRequired(ctx context.Context, key string) (uint32, int64, int64, bool, error) {
	resp, err := b.cli.Get(ctx, key)
	if err != nil {
		return 0, 0, 0, false, err
	}
	if len(resp.Kvs) == 0 {
		return 0, resp.Header.Revision, 0, false, nil
	}
	if len(resp.Kvs) != 1 {
		return 0, resp.Header.Revision, 0, false, errors.New("required key returned multiple values")
	}
	epoch, err := ParseEpoch(resp.Kvs[0].Value)
	return epoch, resp.Header.Revision, resp.Kvs[0].ModRevision, true, err
}

func (b *etcdBackend) AcquireCapability(
	ctx context.Context,
	key, lockKey, requiredKey string,
	expectedRequired uint32,
	expectedRequiredModRevision int64,
	value []byte,
	ttl int64,
) (Lease, error) {
	grant, err := b.cli.Grant(ctx, ttl)
	if err != nil {
		return nil, err
	}
	resp, err := b.cli.Txn(ctx).
		If(
			clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
			// 激活工具持锁期间禁止新 writer 注册，封住 capability 审计到推进 CAS 的 TOCTOU。
			clientv3.Compare(clientv3.CreateRevision(lockKey), "=", 0),
			// required 的线性读与 capability 注册必须组成同一个 fencing 判定。若两者之间
			// 已发生激活/回退/删除，本次旧快照注册必须失败，由进程退出后重新启动读取。
			clientv3.Compare(clientv3.Value(requiredKey), "=", fmt.Sprint(expectedRequired)),
			clientv3.Compare(clientv3.ModRevision(requiredKey), "=", expectedRequiredModRevision),
		).
		Then(clientv3.OpPut(key, string(value), clientv3.WithLease(grant.ID))).
		Commit()
	if err != nil || !resp.Succeeded {
		_, _ = b.cli.Revoke(context.Background(), grant.ID)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("capability registration fenced by duplicate key, activation lock, or required epoch change: %s", key)
	}
	kaCtx, cancel := context.WithCancel(context.Background())
	ka, err := b.cli.KeepAlive(kaCtx, grant.ID)
	if err != nil {
		cancel()
		_, _ = b.cli.Revoke(context.Background(), grant.ID)
		return nil, err
	}
	l := &etcdLease{cli: b.cli, id: grant.ID, cancel: cancel, lost: make(chan struct{})}
	go func() {
		for range ka {
		}
		if !l.intentional.Load() {
			l.lostOnce.Do(func() { close(l.lost) })
		}
	}()
	return l, nil
}

func (b *etcdBackend) WatchRequired(ctx context.Context, key string, revision int64) <-chan RequiredEvent {
	out := make(chan RequiredEvent)
	go func() {
		defer close(out)
		for response := range b.cli.Watch(ctx, key, clientv3.WithRev(revision)) {
			if err := response.Err(); err != nil {
				out <- RequiredEvent{Err: err}
				return
			}
			for _, event := range response.Events {
				item := RequiredEvent{Revision: event.Kv.ModRevision, Deleted: event.Type == clientv3.EventTypeDelete}
				if !item.Deleted {
					item.Epoch, item.Err = ParseEpoch(event.Kv.Value)
				}
				select {
				case out <- item:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (b *etcdBackend) Close() error        { return b.cli.Close() }
func (l *etcdLease) Lost() <-chan struct{} { return l.lost }

func (l *etcdLease) Close() error {
	var out error
	l.closeOnce.Do(func() {
		l.intentional.Store(true)
		l.cancel()
		_, out = l.cli.Revoke(context.Background(), l.id)
	})
	return out
}
