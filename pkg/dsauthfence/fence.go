// Package dsauthfence 为 DS 回调授权协议提供不可回退的进程级激活栅栏。
//
// Redis 的每实例 required_writer_epoch 负责数据面的最终拒绝，本包负责控制面：
// 进程启动时线性读取 etcd 全局 required epoch，注册带租约的 capability，并持续 watch。
// 初读失败、required 回退/删除、未来 epoch 或租约丢失都会关闭 Lost；调用方必须立即退出。
package dsauthfence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// ProtocolEpochV2 是 Redis active/pending 完整凭据协议版本。
	ProtocolEpochV2 uint32 = 2
	// DefaultPrefix 是全局 required 与 capability key 的根前缀。
	DefaultPrefix = "/pandora/ds-auth/"
	// DefaultLeaseTTLSec 是 capability 租约 TTL。
	DefaultLeaseTTLSec int64 = 15
	// DefaultDialTimeout 是 etcd 启动期操作超时。
	DefaultDialTimeout = 5 * time.Second
	// EnvPodUID / EnvImageDigest 必须由 K8s Downward API 注入并由准入策略与 Pod 实体绑定。
	EnvPodUID      = "PANDORA_POD_UID"
	EnvImageDigest = "PANDORA_IMAGE_DIGEST"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Config 是单个进程注册 capability 所需的不可伪造运行时信息。
type Config struct {
	Endpoints      []string
	Prefix         string
	Service        string
	InstanceUID    string
	ImageDigest    string
	KeysetRevision string
	WriterEpoch    uint32
	LeaseTTLSec    int64
	DialTimeout    time.Duration
}

// RuntimeConfig 是业务 main 已有配置可直接映射的部分；不可变 Pod 身份只从环境读取。
type RuntimeConfig struct {
	Endpoints      []string
	Prefix         string
	Service        string
	KeysetRevision string
	LeaseTTLSec    int64
	DialTimeout    time.Duration
	WriterEpoch    uint32
}

// AcquireRuntime 从 Downward API 环境组装完整身份并启动 fence；绝不回退 hostname 或 image tag。
func AcquireRuntime(ctx context.Context, runtime RuntimeConfig) (*Holder, error) {
	return Acquire(ctx, Config{
		Endpoints:      runtime.Endpoints,
		Prefix:         runtime.Prefix,
		Service:        runtime.Service,
		InstanceUID:    os.Getenv(EnvPodUID),
		ImageDigest:    os.Getenv(EnvImageDigest),
		KeysetRevision: runtime.KeysetRevision,
		WriterEpoch:    runtime.WriterEpoch,
		LeaseTTLSec:    runtime.LeaseTTLSec,
		DialTimeout:    runtime.DialTimeout,
	})
}

// Capability 是写入 etcd lease key 的审计记录。
type Capability struct {
	Service        string `json:"service"`
	InstanceUID    string `json:"instance_uid"`
	WriterEpoch    uint32 `json:"writer_epoch"`
	ImageDigest    string `json:"image_digest"`
	KeysetRevision string `json:"keyset_revision"`
	StartedAtMs    int64  `json:"started_at_ms"`
}

// RequiredEvent 是 required key 的有序 watch 事件。
type RequiredEvent struct {
	Epoch    uint32
	Revision int64
	Deleted  bool
	Err      error
}

// Lease 是 capability 的存活权。Lost 关闭后进程不得继续处理受保护写请求。
type Lease interface {
	Lost() <-chan struct{}
	Close() error
}

// Backend 隔离 etcd 细节，允许用确定性 fake 验证所有 fail-closed 分支。
type Backend interface {
	GetRequired(context.Context, string) (epoch uint32, watchRevision int64, modRevision int64, found bool, err error)
	AcquireCapability(context.Context, string, string, string, uint32, int64, []byte, int64) (Lease, error)
	WatchRequired(context.Context, string, int64) <-chan RequiredEvent
	Close() error
}

// Holder 持有 capability 租约及进程内只增不减的 required 高水位。
type Holder struct {
	backend Backend
	lease   Lease
	cancel  context.CancelFunc

	required    atomic.Uint32
	lost        chan struct{}
	lostOnce    sync.Once
	closeOnce   sync.Once
	intentional atomic.Bool
}

// Start 使用已构造的 Backend 启动栅栏。生产调用 Acquire；测试可注入 fake。
func Start(ctx context.Context, backend Backend, cfg Config) (*Holder, error) {
	if backend == nil {
		return nil, errors.New("dsauthfence: nil backend")
	}
	normalize(&cfg)
	if err := validate(cfg); err != nil {
		_ = backend.Close()
		return nil, err
	}

	requiredKey := requiredKey(cfg.Prefix)
	readCtx, cancelRead := context.WithTimeout(ctx, cfg.DialTimeout)
	required, revision, requiredModRevision, found, err := backend.GetRequired(readCtx, requiredKey)
	cancelRead()
	if err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("dsauthfence: linearizable read required: %w", err)
	}
	if !found || required == 0 {
		_ = backend.Close()
		return nil, errors.New("dsauthfence: required writer epoch missing; explicit bootstrap is required")
	}
	if required > cfg.WriterEpoch {
		_ = backend.Close()
		return nil, fmt.Errorf("dsauthfence: required writer epoch %d exceeds supported %d", required, cfg.WriterEpoch)
	}

	capability := Capability{
		Service:        cfg.Service,
		InstanceUID:    cfg.InstanceUID,
		WriterEpoch:    cfg.WriterEpoch,
		ImageDigest:    cfg.ImageDigest,
		KeysetRevision: cfg.KeysetRevision,
		StartedAtMs:    time.Now().UnixMilli(),
	}
	payload, err := json.Marshal(capability)
	if err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("dsauthfence: marshal capability: %w", err)
	}
	leaseCtx, cancelLease := context.WithTimeout(ctx, cfg.DialTimeout)
	lease, err := backend.AcquireCapability(
		leaseCtx,
		capabilityKey(cfg.Prefix, cfg.Service, cfg.InstanceUID),
		activationLockKey(cfg.Prefix),
		requiredKey,
		required,
		requiredModRevision,
		payload,
		cfg.LeaseTTLSec,
	)
	cancelLease()
	if err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("dsauthfence: acquire capability: %w", err)
	}

	watchCtx, cancel := context.WithCancel(context.Background())
	h := &Holder{backend: backend, lease: lease, cancel: cancel, lost: make(chan struct{})}
	h.required.Store(required)
	watch := backend.WatchRequired(watchCtx, requiredKey, revision+1)
	go h.monitorLease(lease.Lost())
	go h.monitorRequired(watch, cfg.WriterEpoch)
	return h, nil
}

// RequiredEpoch 返回本进程已经观察到的全局单调高水位。
func (h *Holder) RequiredEpoch() uint32 { return h.required.Load() }

// Lost 在任何 fencing 条件失效时关闭。调用方必须停止服务并退出。
func (h *Holder) Lost() <-chan struct{} { return h.lost }

func (h *Holder) monitorLease(ch <-chan struct{}) {
	<-ch
	if !h.intentional.Load() {
		h.signalLost()
	}
}

func (h *Holder) monitorRequired(ch <-chan RequiredEvent, supported uint32) {
	for event := range ch {
		if h.intentional.Load() {
			return
		}
		if event.Err != nil || event.Deleted || event.Epoch == 0 {
			h.signalLost()
			return
		}
		seen := h.required.Load()
		if event.Epoch < seen || event.Epoch > supported {
			h.signalLost()
			return
		}
		for event.Epoch > seen && !h.required.CompareAndSwap(seen, event.Epoch) {
			seen = h.required.Load()
		}
	}
	if !h.intentional.Load() {
		// watch 静默结束也不能继续写；不以重连/旧缓存冒充授权。
		h.signalLost()
	}
}

func (h *Holder) signalLost() { h.lostOnce.Do(func() { close(h.lost) }) }

// Close 主动释放 capability 并停止 watch。幂等；主动关闭不触发 Lost。
func (h *Holder) Close() error {
	var out error
	h.closeOnce.Do(func() {
		h.intentional.Store(true)
		if h.cancel != nil {
			h.cancel()
		}
		if h.lease != nil {
			out = h.lease.Close()
		}
		if err := h.backend.Close(); out == nil {
			out = err
		}
	})
	return out
}

func normalize(cfg *Config) {
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.LeaseTTLSec <= 0 {
		cfg.LeaseTTLSec = DefaultLeaseTTLSec
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
}

func validate(cfg Config) error {
	if len(cfg.Endpoints) == 0 {
		return errors.New("dsauthfence: empty endpoints")
	}
	if cfg.Service == "" || strings.Contains(cfg.Service, "/") {
		return errors.New("dsauthfence: invalid service")
	}
	if cfg.InstanceUID == "" || strings.Contains(cfg.InstanceUID, "/") {
		return errors.New("dsauthfence: invalid instance uid")
	}
	if cfg.WriterEpoch == 0 {
		return errors.New("dsauthfence: writer epoch is zero")
	}
	if !digestPattern.MatchString(cfg.ImageDigest) {
		return errors.New("dsauthfence: image digest must be sha256:<64 lowercase hex>")
	}
	if cfg.KeysetRevision == "" {
		return errors.New("dsauthfence: empty keyset revision")
	}
	return nil
}

func cleanPrefix(prefix string) string       { return strings.TrimSuffix(prefix, "/") + "/" }
func requiredKey(prefix string) string       { return cleanPrefix(prefix) + "required-writer-epoch" }
func capabilityPrefix(prefix string) string  { return cleanPrefix(prefix) + "capabilities/" }
func activationLockKey(prefix string) string { return cleanPrefix(prefix) + "activation-lock" }
func capabilityKey(prefix, service, uid string) string {
	return capabilityPrefix(prefix) + service + "/" + uid
}

// ParseEpoch 只接受规范十进制正整数，避免 "02" 等多种字节表示破坏 CAS。
func ParseEpoch(raw []byte) (uint32, error) {
	s := string(raw)
	if s == "" || s[0] == '0' || strings.TrimSpace(s) != s {
		return 0, fmt.Errorf("invalid epoch %q", s)
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil || v == 0 {
		return 0, fmt.Errorf("invalid epoch %q", s)
	}
	return uint32(v), nil
}

// ExpectedServicesHash 对激活清单做确定性摘要，供 activation record 审计。
func ExpectedServicesHash(services map[string]int) string {
	keys := make([]string, 0, len(services))
	for key := range services {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%d\n", key, services[key])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
