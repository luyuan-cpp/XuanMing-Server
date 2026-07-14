package dsauthfence

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// LiveCapability 是 capability 与 etcd lease 元数据的组合。
type LiveCapability struct {
	Capability  Capability
	LeaseID     int64
	Key         string
	ModRevision int64
}

// RequiredSnapshot 是 required writer 的一次线性读结果。Value 相同不足以做推进 CAS：
// key 曾 1→2→1 时只有 ModRevision 能识别 ABA。
type RequiredSnapshot struct {
	Epoch       uint32
	ModRevision int64
}

// ActivationLock 冻结 capability 集合，封住 audit→CAS 的检查使用竞态。
type ActivationLock struct {
	client  *ActivationClient
	leaseID clientv3.LeaseID
	token   string
	cancel  context.CancelFunc
}

// ActivationClient 只供审计/推进工具使用；业务进程不得调用推进 API。
type ActivationClient struct {
	cli    *clientv3.Client
	prefix string
}

// NewActivationClient 构造只操作 DS auth 命名空间的客户端。

func NewActivationClient(endpoints []string, prefix string, timeout time.Duration) (*ActivationClient, error) {
	return NewActivationClientWithSecurity(endpoints, prefix, timeout, ClientSecurity{})
}

// NewActivationClientWithSecurity 构造带 mTLS/auth/ACL 负向证明的激活客户端。
func NewActivationClientWithSecurity(endpoints []string, prefix string, timeout time.Duration, security ClientSecurity) (*ActivationClient, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("dsauthfence: empty endpoints")
	}
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	cli, err := newEtcdClient(endpoints, timeout, prefix, security)
	if err != nil {
		return nil, err
	}
	return &ActivationClient{cli: cli, prefix: prefix}, nil
}

func (c *ActivationClient) Close() error { return c.cli.Close() }

// AcquireLock 以 lease + create-only CAS 获取激活锁。业务 capability 注册事务会同时断言锁不存在。
func (c *ActivationClient) AcquireLock(ctx context.Context, ttl int64) (*ActivationLock, error) {
	if ttl <= 0 {
		ttl = 30
	}
	grant, err := c.cli.Grant(ctx, ttl)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		_, _ = c.cli.Revoke(context.Background(), grant.ID)
		return nil, err
	}
	token := hex.EncodeToString(raw)
	resp, err := c.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(activationLockKey(c.prefix)), "=", 0)).
		Then(clientv3.OpPut(activationLockKey(c.prefix), token, clientv3.WithLease(grant.ID))).
		Commit()
	if err != nil || !resp.Succeeded {
		_, _ = c.cli.Revoke(context.Background(), grant.ID)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("activation lock is held")
	}
	keepCtx, cancel := context.WithCancel(context.Background())
	keep, err := c.cli.KeepAlive(keepCtx, grant.ID)
	if err != nil {
		cancel()
		_, _ = c.cli.Revoke(context.Background(), grant.ID)
		return nil, err
	}
	lock := &ActivationLock{client: c, leaseID: grant.ID, token: token, cancel: cancel}
	go func() {
		for range keep {
		}
	}()
	return lock, nil
}

// Close 释放激活锁。
func (l *ActivationLock) Close() error {
	l.cancel()
	_, err := l.client.cli.Revoke(context.Background(), l.leaseID)
	return err
}

// BootstrapRequired 仅允许在 key 不存在时创建初始 epoch；不会覆盖或回退。
func (c *ActivationClient) BootstrapRequired(ctx context.Context, epoch uint32) error {
	if epoch != 1 {
		return fmt.Errorf("bootstrap epoch must be immutable baseline 1, got %d", epoch)
	}
	key := requiredKey(c.prefix)
	resp, err := c.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, strconv.FormatUint(uint64(epoch), 10))).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return fmt.Errorf("required epoch already exists; bootstrap refused")
	}
	return nil
}

// Required 线性读取当前 required epoch。
func (c *ActivationClient) Required(ctx context.Context) (uint32, error) {
	snapshot, err := c.RequiredSnapshot(ctx)
	return snapshot.Epoch, err
}

// RequiredSnapshot 线性读取 required 的值与同一次读取观察到的 ModRevision。
func (c *ActivationClient) RequiredSnapshot(ctx context.Context) (RequiredSnapshot, error) {
	resp, err := c.cli.Get(ctx, requiredKey(c.prefix))
	if err != nil {
		return RequiredSnapshot{}, err
	}
	if len(resp.Kvs) != 1 {
		return RequiredSnapshot{}, fmt.Errorf("required epoch missing")
	}
	epoch, err := ParseEpoch(resp.Kvs[0].Value)
	if err != nil {
		return RequiredSnapshot{}, err
	}
	if resp.Kvs[0].ModRevision <= 0 {
		return RequiredSnapshot{}, fmt.Errorf("required epoch has invalid mod revision")
	}
	return RequiredSnapshot{Epoch: epoch, ModRevision: resp.Kvs[0].ModRevision}, nil
}

// Capabilities 列出仍有 lease 的实时能力；坏记录使审计失败，绝不跳过。
func (c *ActivationClient) Capabilities(ctx context.Context) ([]LiveCapability, error) {
	resp, err := c.cli.Get(ctx, capabilityPrefix(c.prefix), clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]LiveCapability, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var capability Capability
		if err := json.Unmarshal(kv.Value, &capability); err != nil {
			return nil, fmt.Errorf("decode capability %s: %w", kv.Key, err)
		}
		if kv.Lease == 0 {
			return nil, fmt.Errorf("capability %s has no lease", kv.Key)
		}
		out = append(out, LiveCapability{Capability: capability, LeaseID: kv.Lease, Key: string(kv.Key), ModRevision: kv.ModRevision})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// AuditPolicy 是推进前 capability 快照必须满足的精确条件。
type AuditPolicy struct {
	Prefix               string
	RequiredServices     map[string]int
	RequiredInstances    map[string]map[string]struct{}
	TargetEpoch          uint32
	KeysetRevision       string
	EtcdIdentityRevision string
	AllowedDigests       map[string]struct{}
	ExpectedDigests      map[string]string
	RequiredFeatures     map[string]map[string]struct{}
}

// AuditCapabilities 验证每个预期服务的实时副本数与不可变身份。
func AuditCapabilities(capabilities []LiveCapability, policy AuditPolicy) []string {
	counts := make(map[string]int)
	findings := make([]string, 0)
	seenUID := make(map[string]struct{})
	prefix := policy.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	for service, expectedFeatures := range policy.RequiredFeatures {
		if _, ok := policy.RequiredServices[service]; !ok {
			findings = append(findings, fmt.Sprintf("feature policy 包含未声明 writer %s", service))
		}
		for feature := range expectedFeatures {
			if err := validateFeatures([]string{feature}); err != nil {
				findings = append(findings, fmt.Sprintf("%s feature policy 非 canonical", service))
			}
		}
	}
	for service, digest := range policy.ExpectedDigests {
		if _, ok := policy.RequiredServices[service]; !ok {
			findings = append(findings, fmt.Sprintf("digest policy 包含未声明 writer %s", service))
		}
		if !digestPattern.MatchString(digest) {
			findings = append(findings, fmt.Sprintf("%s expected image_digest 非 immutable digest", service))
		}
	}
	for service := range policy.RequiredServices {
		if _, ok := policy.ExpectedDigests[service]; !ok {
			findings = append(findings, fmt.Sprintf("%s 缺服务级 expected image_digest", service))
		}
	}
	for _, live := range capabilities {
		capability := live.Capability
		if live.Key != capabilityKey(prefix, capability.Service, capability.InstanceUID) {
			findings = append(findings, fmt.Sprintf("capability key 与 payload 身份不一致: %s", live.Key))
		}
		if live.LeaseID == 0 {
			findings = append(findings, fmt.Sprintf("%s/%s 无 lease", capability.Service, capability.InstanceUID))
		}
		if capability.WriterEpoch != policy.TargetEpoch {
			findings = append(findings, fmt.Sprintf("%s/%s writer_epoch=%d != target=%d", capability.Service, capability.InstanceUID, capability.WriterEpoch, policy.TargetEpoch))
		}
		if !digestPattern.MatchString(capability.ImageDigest) {
			findings = append(findings, fmt.Sprintf("%s/%s image_digest 非 immutable digest", capability.Service, capability.InstanceUID))
		}
		if len(policy.AllowedDigests) > 0 {
			if _, ok := policy.AllowedDigests[capability.ImageDigest]; !ok {
				findings = append(findings, fmt.Sprintf("%s/%s image_digest 不在本次激活清单", capability.Service, capability.InstanceUID))
			}
		}
		if expectedDigest, ok := policy.ExpectedDigests[capability.Service]; !ok || capability.ImageDigest != expectedDigest {
			findings = append(findings, fmt.Sprintf("%s/%s image_digest=%q, service expected=%q", capability.Service, capability.InstanceUID, capability.ImageDigest, expectedDigest))
		}
		if policy.KeysetRevision == "" || capability.KeysetRevision != policy.KeysetRevision {
			findings = append(findings, fmt.Sprintf("%s/%s keyset_revision=%q, expected=%q", capability.Service, capability.InstanceUID, capability.KeysetRevision, policy.KeysetRevision))
		}
		if policy.EtcdIdentityRevision != "" && capability.EtcdIdentityRevision != policy.EtcdIdentityRevision {
			findings = append(findings, fmt.Sprintf("%s/%s etcd_identity_revision=%q, expected=%q", capability.Service, capability.InstanceUID, capability.EtcdIdentityRevision, policy.EtcdIdentityRevision))
		}
		if err := validateFeatures(capability.Features); err != nil {
			findings = append(findings, fmt.Sprintf("%s/%s capability features 非 canonical", capability.Service, capability.InstanceUID))
		}
		featureSet := make(map[string]struct{}, len(capability.Features))
		for _, feature := range capability.Features {
			featureSet[feature] = struct{}{}
		}
		expectedFeatures := policy.RequiredFeatures[capability.Service]
		for feature := range expectedFeatures {
			if _, ok := featureSet[feature]; !ok {
				findings = append(findings, fmt.Sprintf("%s/%s 缺 capability feature=%s", capability.Service, capability.InstanceUID, feature))
			}
		}
		for feature := range featureSet {
			if _, ok := expectedFeatures[feature]; !ok {
				findings = append(findings, fmt.Sprintf("%s/%s 含未批准 capability feature=%s", capability.Service, capability.InstanceUID, feature))
			}
		}
		identity := capability.Service + "/" + capability.InstanceUID
		if _, ok := seenUID[identity]; ok {
			findings = append(findings, fmt.Sprintf("重复 capability %s", identity))
		}
		seenUID[identity] = struct{}{}
		if len(policy.RequiredInstances) > 0 {
			instances, ok := policy.RequiredInstances[capability.Service]
			if !ok {
				findings = append(findings, fmt.Sprintf("%s/%s 不在 K8s Pod UID 清单", capability.Service, capability.InstanceUID))
			} else if _, ok := instances[capability.InstanceUID]; !ok {
				findings = append(findings, fmt.Sprintf("%s/%s capability UID 不在 K8s 清单", capability.Service, capability.InstanceUID))
			}
		}
		counts[capability.Service]++
	}
	for service, want := range policy.RequiredServices {
		if want <= 0 {
			findings = append(findings, fmt.Sprintf("%s 预期副本数必须 >0", service))
			continue
		}
		if counts[service] != want {
			findings = append(findings, fmt.Sprintf("%s capability=%d, expected=%d", service, counts[service], want))
		}
	}
	for service, count := range counts {
		if _, ok := policy.RequiredServices[service]; !ok {
			findings = append(findings, fmt.Sprintf("发现未在激活清单中的旧/额外 writer %s=%d", service, count))
		}
	}
	for service, instances := range policy.RequiredInstances {
		for uid := range instances {
			if _, ok := seenUID[service+"/"+uid]; !ok {
				findings = append(findings, fmt.Sprintf("K8s live Pod %s/%s 缺 capability lease", service, uid))
			}
		}
	}
	sort.Strings(findings)
	return findings
}

// ParseExpectedDigests 解析 service=sha256:<64 lowercase hex> 的精确服务级镜像清单。
// 与全局 allowlist 不同，这个映射阻止一个 writer 冒用本次发布中另一个服务的合法 digest。
func ParseExpectedDigests(raw string) (map[string]string, error) {
	out := make(map[string]string)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 || parts[0] == "" || strings.ContainsAny(parts[0], "/,=| ") || !digestPattern.MatchString(parts[1]) {
			return nil, fmt.Errorf("invalid expected service digest %q", item)
		}
		if _, exists := out[parts[0]]; exists {
			return nil, fmt.Errorf("duplicate digest service %q", parts[0])
		}
		out[parts[0]] = parts[1]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("expected service digests is empty")
	}
	return out, nil
}

// ParseRequiredFeatures 解析 service=feature|feature,service=feature 的精确必需能力。
func ParseRequiredFeatures(raw string) (map[string]map[string]struct{}, error) {
	out := make(map[string]map[string]struct{})
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, item := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid required features %q", item)
		}
		if _, exists := out[parts[0]]; exists {
			return nil, fmt.Errorf("duplicate feature service %q", parts[0])
		}
		set := make(map[string]struct{})
		for _, feature := range strings.Split(parts[1], "|") {
			if err := validateFeatures([]string{feature}); err != nil {
				return nil, fmt.Errorf("invalid feature for %s", parts[0])
			}
			if _, exists := set[feature]; exists {
				return nil, fmt.Errorf("duplicate feature %q", feature)
			}
			set[feature] = struct{}{}
		}
		out[parts[0]] = set
	}
	return out, nil
}

// AdvanceRequired 只允许 expected→target 的前进 CAS，并写不可变审计记录。
// expectedModRevision 必须来自锁内 RequiredSnapshot；值+revision 双比较封住 1→2→1 ABA。
func (l *ActivationLock) AdvanceRequired(ctx context.Context, expected, target uint32, expectedModRevision int64, expectedServices map[string]int, audited []LiveCapability) error {
	if expected == 0 || target <= expected || expectedModRevision <= 0 {
		return fmt.Errorf("required epoch must advance: %d -> %d", expected, target)
	}
	key := requiredKey(l.client.prefix)
	recordKey := cleanPrefix(l.client.prefix) + "activations/" + strconv.FormatUint(uint64(target), 10)
	record := map[string]any{
		"from":                   expected,
		"to":                     target,
		"from_mod_revision":      expectedModRevision,
		"expected_services_hash": ExpectedServicesHash(expectedServices),
		"activated_at_ms":        time.Now().UnixMilli(),
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	compares, err := buildAdvanceCompares(
		l.client.prefix, l.token, key, recordKey, expected, expectedModRevision, audited)
	if err != nil {
		return err
	}
	resp, err := l.client.cli.Txn(ctx).
		If(compares...).
		Then(
			clientv3.OpPut(key, strconv.FormatUint(uint64(target), 10)),
			clientv3.OpPut(recordKey, string(payload)),
		).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return fmt.Errorf("required epoch CAS failed (expected=%d target=%d)", expected, target)
	}
	return nil
}

func buildAdvanceCompares(
	prefix, lockToken, key, recordKey string,
	expected uint32,
	expectedModRevision int64,
	audited []LiveCapability,
) ([]clientv3.Cmp, error) {
	if expected == 0 || expectedModRevision <= 0 || key == "" || recordKey == "" || lockToken == "" {
		return nil, fmt.Errorf("invalid required advance snapshot")
	}
	compares := []clientv3.Cmp{
		clientv3.Compare(clientv3.Value(key), "=", strconv.FormatUint(uint64(expected), 10)),
		clientv3.Compare(clientv3.ModRevision(key), "=", expectedModRevision),
		clientv3.Compare(clientv3.CreateRevision(recordKey), "=", 0),
		clientv3.Compare(clientv3.Value(activationLockKey(prefix)), "=", lockToken),
	}
	for _, capability := range audited {
		if capability.Key == "" || capability.ModRevision <= 0 {
			return nil, fmt.Errorf("invalid audited capability revision")
		}
		compares = append(compares,
			clientv3.Compare(clientv3.ModRevision(capability.Key), "=", capability.ModRevision))
	}
	return compares, nil
}

// ParseExpectedServices 解析 service=count 逗号清单。
func ParseExpectedServices(raw string) (map[string]int, error) {
	out := make(map[string]int)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, "=")
		if len(parts) != 2 || parts[0] == "" {
			return nil, fmt.Errorf("invalid expected service %q", item)
		}
		count, err := strconv.Atoi(parts[1])
		if err != nil || count <= 0 {
			return nil, fmt.Errorf("invalid expected service count %q", item)
		}
		if _, exists := out[parts[0]]; exists {
			return nil, fmt.Errorf("duplicate expected service %q", parts[0])
		}
		out[parts[0]] = count
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("expected services is empty")
	}
	return out, nil
}

// ParseExpectedInstances 解析 service=uid|uid,service=uid 的精确 K8s Pod UID 清单。
func ParseExpectedInstances(raw string) (map[string]map[string]struct{}, error) {
	out := make(map[string]map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid expected instances %q", item)
		}
		if _, exists := out[parts[0]]; exists {
			return nil, fmt.Errorf("duplicate instance service %q", parts[0])
		}
		set := make(map[string]struct{})
		for _, uid := range strings.Split(parts[1], "|") {
			uid = strings.TrimSpace(uid)
			if uid == "" || strings.ContainsAny(uid, "/,=") {
				return nil, fmt.Errorf("invalid instance uid %q", uid)
			}
			if _, exists := set[uid]; exists {
				return nil, fmt.Errorf("duplicate instance uid %q", uid)
			}
			set[uid] = struct{}{}
		}
		out[parts[0]] = set
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("expected instances is empty")
	}
	return out, nil
}
