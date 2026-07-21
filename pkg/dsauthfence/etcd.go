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

func (b *etcdBackend) GetRequired(ctx context.Context, key string) (RequiredState, int64, int64, bool, error) {
	resp, err := b.cli.Get(ctx, key)
	if err != nil {
		return RequiredState{}, 0, 0, false, err
	}
	if len(resp.Kvs) == 0 {
		return RequiredState{}, resp.Header.Revision, 0, false, nil
	}
	if len(resp.Kvs) != 1 {
		return RequiredState{}, resp.Header.Revision, 0, false, errors.New("required key returned multiple values")
	}
	state, err := ParseRequiredState(resp.Kvs[0].Value)
	return state, resp.Header.Revision, resp.Kvs[0].ModRevision, true, err
}

// GetCapability 线性读取 capability key 现值(默认线性一致读),返回值与租约 ID,
// 供同 Pod 崩溃重启的安全接管预检。
func (b *etcdBackend) GetCapability(ctx context.Context, key string) ([]byte, int64, int64, bool, error) {
	resp, err := b.cli.Get(ctx, key)
	if err != nil {
		return nil, 0, 0, false, err
	}
	if len(resp.Kvs) == 0 {
		return nil, 0, 0, false, nil
	}
	if len(resp.Kvs) != 1 {
		return nil, 0, 0, false, errors.New("capability key returned multiple values")
	}
	kv := resp.Kvs[0]
	return kv.Value, kv.ModRevision, kv.Lease, true, nil
}

func (b *etcdBackend) AcquireCapability(
	ctx context.Context,
	key, lockKey, requiredKey string,
	expectedRequiredValue string,
	expectedRequiredModRevision int64,
	value []byte,
	ttl int64,
	prevModRevision int64,
	prevLeaseID int64,
) (Lease, error) {
	grant, err := b.cli.Grant(ctx, ttl)
	if err != nil {
		return nil, err
	}
	// 默认要求 key 不存在;同 Pod 安全接管(prevModRevision>0)改为 ModRevision 精确 CAS,
	// 并发接管者最多一个成功,fencing 语义与全新注册完全一致。
	ownKeyCmp := clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	if prevModRevision > 0 {
		ownKeyCmp = clientv3.Compare(clientv3.ModRevision(key), "=", prevModRevision)
	}
	resp, err := b.cli.Txn(ctx).
		If(
			ownKeyCmp,
			// 激活工具持锁期间禁止新 writer 注册，封住 capability 审计到推进 CAS 的 TOCTOU。
			clientv3.Compare(clientv3.CreateRevision(lockKey), "=", 0),
			// required 的线性读与 capability 注册必须组成同一个 fencing 判定。若两者之间
			// 已发生激活/回退/删除，本次旧快照注册必须失败，由进程退出后重新启动读取。
			clientv3.Compare(clientv3.Value(requiredKey), "=", expectedRequiredValue),
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
	if prevLeaseID != 0 {
		// 接管成功后终结旧租约:Put 已把 key 挂到新租约(etcd 覆盖写自动解除旧租约附着),
		// 此刻 revoke 只会杀掉空租约的 keepalive——若旧进程理论上仍存活,其 Lost 立即触发
		// 退出,结构性保证单 writer;旧租约已自然过期时返回 NotFound,忽略即可。
		_, _ = b.cli.Revoke(ctx, clientv3.LeaseID(prevLeaseID))
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
					item.State, item.Err = ParseRequiredState(event.Kv.Value)
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
