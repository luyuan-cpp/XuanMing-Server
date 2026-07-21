package dsauthfence

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	Epoch            uint32
	PolicyGeneration uint32
	PolicyID         string
	RawValue         string
	ModRevision      int64
}

// ActivationRecord is the immutable audit record written in the same etcd
// transaction that advances required_writer_epoch.  ActivationEvidenceSHA256
// binds the external, create-only Kubernetes evidence marker (including the
// completed preflight Job/Pod and exact configuration identity) to the epoch
// transition; a same-named Job created after activation can therefore never
// satisfy an epoch-2 audit.
type ActivationRecord struct {
	From                            uint32 `json:"from"`
	To                              uint32 `json:"to"`
	FromRequiredValue               string `json:"from_required_value"`
	ToRequiredValue                 string `json:"to_required_value"`
	RequiredPolicyID                string `json:"required_policy_id"`
	FromPolicyGeneration            uint32 `json:"from_policy_generation,omitempty"`
	ToPolicyGeneration              uint32 `json:"to_policy_generation,omitempty"`
	FromModRevision                 int64  `json:"from_mod_revision"`
	ExpectedServicesHash            string `json:"expected_services_hash"`
	ActivationEvidenceSHA256        string `json:"activation_evidence_sha256"`
	ActivationEvidenceCompletedAtMS int64  `json:"activation_evidence_completed_at_ms"`
	ActivatedAtMS                   int64  `json:"activated_at_ms"`
	ZeroWriterBootstrap             bool   `json:"zero_writer_bootstrap,omitempty"`
	GenesisBootstrap                bool   `json:"genesis_bootstrap,omitempty"`
}

// ActivationLock 冻结 capability 集合，封住 audit→CAS 的检查使用竞态。
type ActivationLock struct {
	client  *ActivationClient
	leaseID clientv3.LeaseID
	token   string
	cancel  context.CancelFunc
	// No code path sets this until a real control-plane provider verifier is
	// implemented. It prevents library callers from bypassing the release
	// wrapper and advancing with a self-asserted Kubernetes marker.
	topologyLeaseVerified bool
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
	value, err := RequiredValueForEpoch(epoch)
	if err != nil {
		return err
	}
	resp, err := c.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, value)).
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
	state, err := ParseRequiredState(resp.Kvs[0].Value)
	if err != nil {
		return RequiredSnapshot{}, err
	}
	if resp.Kvs[0].ModRevision <= 0 {
		return RequiredSnapshot{}, fmt.Errorf("required epoch has invalid mod revision")
	}
	return RequiredSnapshot{
		Epoch: state.Epoch, PolicyGeneration: state.PolicyGeneration,
		PolicyID: state.PolicyID, RawValue: state.RawValue,
		ModRevision: resp.Kvs[0].ModRevision,
	}, nil
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
	Prefix                           string
	RequiredServices                 map[string]int
	RequiredInstances                map[string]map[string]struct{}
	TargetEpoch                      uint32
	TargetPolicyGeneration           uint32
	ExpectedAcquiredPolicyGeneration uint32
	KeysetRevision                   string
	EtcdIdentityRevision             string
	AllowedDigests                   map[string]struct{}
	ExpectedDigests                  map[string]string
	RequiredFeatures                 map[string]map[string]struct{}
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
	targetWriterEpoch := policy.TargetEpoch
	if policy.TargetPolicyGeneration != 0 {
		if expected, err := requiredWriterEpochForPolicyGeneration(policy.TargetPolicyGeneration); err != nil {
			findings = append(findings, err.Error())
		} else {
			targetWriterEpoch = expected
		}
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
		if capability.WriterEpoch != targetWriterEpoch {
			findings = append(findings, fmt.Sprintf("%s/%s writer_epoch=%d != target=%d", capability.Service, capability.InstanceUID, capability.WriterEpoch, targetWriterEpoch))
		}
		if policy.TargetPolicyGeneration == RequiredPolicyGenerationV3 &&
			(capability.SupportedPolicyGeneration != RequiredPolicyGenerationV3 ||
				capability.SupportedPolicyID != RequiredPolicyV3) {
			findings = append(findings, fmt.Sprintf(
				"%s/%s supported_policy=%d@%q != target=%d@%q",
				capability.Service, capability.InstanceUID,
				capability.SupportedPolicyGeneration, capability.SupportedPolicyID,
				RequiredPolicyGenerationV3, RequiredPolicyV3))
		}
		if policy.ExpectedAcquiredPolicyGeneration != 0 {
			expectedPolicyID, err := requiredPolicyIDForGeneration(policy.ExpectedAcquiredPolicyGeneration)
			if err != nil {
				findings = append(findings, err.Error())
			} else if capability.AcquiredPolicyGeneration != policy.ExpectedAcquiredPolicyGeneration ||
				capability.AcquiredPolicyID != expectedPolicyID {
				findings = append(findings, fmt.Sprintf(
					"%s/%s acquired_policy=%d@%q != expected=%d@%q",
					capability.Service, capability.InstanceUID,
					capability.AcquiredPolicyGeneration, capability.AcquiredPolicyID,
					policy.ExpectedAcquiredPolicyGeneration, expectedPolicyID))
			}
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

func validateAuditedTargetPolicy(target uint32, expectedServices map[string]int, audited []LiveCapability) error {
	if target != ProtocolEpochV2 {
		return fmt.Errorf("unsupported audited target writer epoch %d", target)
	}
	return validateAuditedPolicyGeneration(RequiredPolicyGenerationV2, expectedServices, audited)
}

func validateAuditedPolicyGeneration(generation uint32, expectedServices map[string]int,
	audited []LiveCapability) error {
	expectedPolicy, err := requiredFeaturesForPolicyGeneration(generation)
	if err != nil {
		return err
	}
	policyFeatures := make(map[string]map[string]struct{}, len(expectedPolicy))
	for service, features := range expectedPolicy {
		set := make(map[string]struct{}, len(features))
		for _, feature := range features {
			set[feature] = struct{}{}
		}
		policyFeatures[service] = set
	}
	if err := ValidateActivationPolicyGeneration(generation, expectedServices, policyFeatures); err != nil {
		return err
	}
	targetValue, err := RequiredValueForPolicyGeneration(generation)
	if err != nil {
		return err
	}
	state, err := ParseRequiredState([]byte(targetValue))
	if err != nil {
		return err
	}
	counts := make(map[string]int)
	targetWriterEpoch, err := requiredWriterEpochForPolicyGeneration(generation)
	if err != nil {
		return err
	}
	for _, live := range audited {
		capability := live.Capability
		if capability.WriterEpoch != targetWriterEpoch {
			return fmt.Errorf("audited capability %s/%s writer epoch does not match target policy", capability.Service, capability.InstanceUID)
		}
		if generation == RequiredPolicyGenerationV3 &&
			(capability.SupportedPolicyGeneration != RequiredPolicyGenerationV3 ||
				capability.SupportedPolicyID != RequiredPolicyV3) {
			return fmt.Errorf("audited capability %s/%s does not compile in exact target policy %d@%s",
				capability.Service, capability.InstanceUID, RequiredPolicyGenerationV3, RequiredPolicyV3)
		}
		if generation == RequiredPolicyGenerationV3 &&
			(capability.AcquiredPolicyGeneration != RequiredPolicyGenerationV2 ||
				capability.AcquiredPolicyID != RequiredPolicyV2) {
			return fmt.Errorf("audited staging capability %s/%s was not acquired against exact V2",
				capability.Service, capability.InstanceUID)
		}
		if err := validateRequiredPolicyForCapability(
			state, capability.Service, capability.WriterEpoch, capability.Features); err != nil {
			return err
		}
		counts[capability.Service]++
	}
	for service, expected := range expectedServices {
		if counts[service] != expected {
			return fmt.Errorf("audited capability count for %s does not match activation policy", service)
		}
	}
	return nil
}

// AdvanceRequired 只允许 expected→target 的前进 CAS，并写不可变审计记录。
// expectedModRevision 必须来自锁内 RequiredSnapshot；值+revision 双比较封住 1→2→1 ABA。
func (l *ActivationLock) AdvanceRequired(ctx context.Context, expected, target uint32, expectedModRevision int64, expectedServices map[string]int, audited []LiveCapability, activationEvidenceSHA256 string, activationEvidenceCompletedAtMS int64) error {
	if l == nil || !l.topologyLeaseVerified {
		return ErrTopologyChangeLockProviderUnavailable
	}
	if expected == 0 || target <= expected || expectedModRevision <= 0 {
		return fmt.Errorf("required epoch must advance: %d -> %d", expected, target)
	}
	if err := validateActivationEvidenceSHA256(activationEvidenceSHA256); err != nil {
		return err
	}
	if err := validateAuditedTargetPolicy(target, expectedServices, audited); err != nil {
		return err
	}
	fromValue, err := RequiredValueForEpoch(expected)
	if err != nil {
		return err
	}
	toValue, err := RequiredValueForEpoch(target)
	if err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	if activationEvidenceCompletedAtMS <= 0 || activationEvidenceCompletedAtMS > nowMS {
		return fmt.Errorf("activation evidence completion time is invalid")
	}
	key := requiredKey(l.client.prefix)
	recordKey := cleanPrefix(l.client.prefix) + "activations/" + strconv.FormatUint(uint64(target), 10)
	record := ActivationRecord{
		From:                            expected,
		To:                              target,
		FromRequiredValue:               fromValue,
		ToRequiredValue:                 toValue,
		RequiredPolicyID:                RequiredPolicyV2,
		FromModRevision:                 expectedModRevision,
		ExpectedServicesHash:            ExpectedServicesHash(expectedServices),
		ActivationEvidenceSHA256:        activationEvidenceSHA256,
		ActivationEvidenceCompletedAtMS: activationEvidenceCompletedAtMS,
		ActivatedAtMS:                   nowMS,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	compares, err := buildAdvanceCompares(
		l.client.prefix, l.token, key, recordKey, fromValue, expectedModRevision, audited)
	if err != nil {
		return err
	}
	resp, err := l.client.cli.Txn(ctx).
		If(compares...).
		Then(
			clientv3.OpPut(key, toValue),
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

func policyActivationRecordKey(prefix string, generation uint32, policyID string) string {
	return cleanPrefix(prefix) + "activations/policies/" + strconv.FormatUint(uint64(generation), 10) + "@" + policyID
}

// AdvanceRequiredPolicyV3 atomically advances immutable V2 to immutable V3
// while retaining data-plane WriterEpoch=2. This is a policy-only transition:
// it needs the etcd activation lock and an exact target-policy capability
// snapshot, but deliberately does not depend on the Redis topology lock used
// by a data-plane writer-epoch change.
func (l *ActivationLock) AdvanceRequiredPolicyV3(ctx context.Context, expected RequiredSnapshot,
	expectedServices map[string]int, audited []LiveCapability, activationEvidenceSHA256 string,
	activationEvidenceCompletedAtMS int64) error {
	if l == nil || l.client == nil || l.token == "" {
		return fmt.Errorf("activation lock is missing")
	}
	if expected.ModRevision <= 0 || expected.PolicyGeneration != RequiredPolicyGenerationV2 ||
		expected.Epoch != ProtocolEpochV2 || expected.PolicyID != RequiredPolicyV2 ||
		expected.RawValue != RequiredValueV2 {
		return fmt.Errorf("required policy-only transition must advance from V2 to V3")
	}
	fromValue, err := RequiredValueForPolicyGeneration(expected.PolicyGeneration)
	if err != nil {
		return err
	}
	if expected.RawValue != fromValue {
		return fmt.Errorf("required policy snapshot raw value mismatch")
	}
	if err := validateActivationEvidenceSHA256(activationEvidenceSHA256); err != nil {
		return err
	}
	if err := validateAuditedPolicyGeneration(RequiredPolicyGenerationV3, expectedServices, audited); err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	if activationEvidenceCompletedAtMS <= 0 || activationEvidenceCompletedAtMS > nowMS {
		return fmt.Errorf("activation evidence completion time is invalid")
	}
	toValue := RequiredValueV3
	recordKey := policyActivationRecordKey(l.client.prefix, RequiredPolicyGenerationV3, RequiredPolicyV3)
	record := ActivationRecord{
		From: expected.Epoch, To: ProtocolEpochV2,
		FromPolicyGeneration: expected.PolicyGeneration, ToPolicyGeneration: RequiredPolicyGenerationV3,
		FromRequiredValue: fromValue, ToRequiredValue: toValue, RequiredPolicyID: RequiredPolicyV3,
		FromModRevision: expected.ModRevision, ExpectedServicesHash: ExpectedServicesHash(expectedServices),
		ActivationEvidenceSHA256:        activationEvidenceSHA256,
		ActivationEvidenceCompletedAtMS: activationEvidenceCompletedAtMS, ActivatedAtMS: nowMS,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	key := requiredKey(l.client.prefix)
	compares, err := buildAdvanceCompares(l.client.prefix, l.token, key, recordKey,
		fromValue, expected.ModRevision, audited)
	if err != nil {
		return err
	}
	resp, err := l.client.cli.Txn(ctx).If(compares...).Then(
		clientv3.OpPut(key, toValue),
		clientv3.OpPut(recordKey, string(payload)),
	).Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return fmt.Errorf("required policy CAS failed (expected_generation=%d target_generation=%d)",
			expected.PolicyGeneration, RequiredPolicyGenerationV3)
	}
	return nil
}

// AdvanceRequiredPolicyV3FromZeroWriters is the only supported V1→V3 path.
// AcquireLock prevents new capability registration; the same CAS that changes
// required also proves the complete capability prefix is still empty. This is
// intended for a fresh/reset cluster before any writer process starts and must
// never be replaced with a manual etcd put.
func (l *ActivationLock) AdvanceRequiredPolicyV3FromZeroWriters(ctx context.Context,
	expected RequiredSnapshot, activationEvidenceSHA256 string,
	activationEvidenceCompletedAtMS int64) error {
	if l == nil || l.client == nil || l.token == "" {
		return fmt.Errorf("activation lock is missing")
	}
	if expected.ModRevision <= 0 || expected.PolicyGeneration != RequiredPolicyGenerationV1 ||
		expected.Epoch != 1 || expected.PolicyID != "" || expected.RawValue != "1" {
		return fmt.Errorf("zero-writer policy bootstrap must advance exact V1 to V3")
	}
	if err := validateActivationEvidenceSHA256(activationEvidenceSHA256); err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	if activationEvidenceCompletedAtMS <= 0 || activationEvidenceCompletedAtMS > nowMS {
		return fmt.Errorf("activation evidence completion time is invalid")
	}
	recordKey := policyActivationRecordKey(l.client.prefix, RequiredPolicyGenerationV3, RequiredPolicyV3)
	record := ActivationRecord{
		From: 1, To: ProtocolEpochV2,
		FromPolicyGeneration: RequiredPolicyGenerationV1,
		ToPolicyGeneration:   RequiredPolicyGenerationV3,
		FromRequiredValue:    "1", ToRequiredValue: RequiredValueV3,
		RequiredPolicyID: RequiredPolicyV3, FromModRevision: expected.ModRevision,
		ExpectedServicesHash:            ExpectedServicesHash(map[string]int{}),
		ActivationEvidenceSHA256:        activationEvidenceSHA256,
		ActivationEvidenceCompletedAtMS: activationEvidenceCompletedAtMS,
		ActivatedAtMS:                   nowMS, ZeroWriterBootstrap: true,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	key := requiredKey(l.client.prefix)
	compares, err := buildZeroWriterPolicyAdvanceCompares(l.client.prefix, l.token,
		key, recordKey, expected.ModRevision)
	if err != nil {
		return err
	}
	resp, err := l.client.cli.Txn(ctx).If(compares...).Then(
		clientv3.OpPut(key, RequiredValueV3),
		clientv3.OpPut(recordKey, string(payload)),
	).Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return fmt.Errorf("zero-writer required policy CAS failed; V1 snapshot, lock, empty capability prefix, or create-only record changed")
	}
	return nil
}

// BootstrapRequiredPolicyV3FromMissing is the crash-safe fresh-cluster
// genesis. Unlike a two-command missing→V1→V3 sequence, required V3 and its
// immutable record are created together. The activation lock plus range
// compare prove no writer capability existed at the linearization point.
func (l *ActivationLock) BootstrapRequiredPolicyV3FromMissing(ctx context.Context,
	activationEvidenceSHA256 string, activationEvidenceCompletedAtMS int64,
	genesisContinuityToken string) error {
	if l == nil || l.client == nil || l.token == "" {
		return fmt.Errorf("activation lock is missing")
	}
	if err := validateActivationEvidenceSHA256(activationEvidenceSHA256); err != nil {
		return err
	}
	if err := ValidateGenesisContinuityToken(genesisContinuityToken); err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	if activationEvidenceCompletedAtMS <= 0 || activationEvidenceCompletedAtMS > nowMS {
		return fmt.Errorf("activation evidence completion time is invalid")
	}
	recordKey := policyActivationRecordKey(l.client.prefix, RequiredPolicyGenerationV3, RequiredPolicyV3)
	record := ActivationRecord{
		From: 0, To: ProtocolEpochV2,
		FromPolicyGeneration: 0, ToPolicyGeneration: RequiredPolicyGenerationV3,
		FromRequiredValue: "", ToRequiredValue: RequiredValueV3,
		RequiredPolicyID: RequiredPolicyV3, FromModRevision: 0,
		ExpectedServicesHash:            ExpectedServicesHash(map[string]int{}),
		ActivationEvidenceSHA256:        activationEvidenceSHA256,
		ActivationEvidenceCompletedAtMS: activationEvidenceCompletedAtMS,
		ActivatedAtMS:                   nowMS, ZeroWriterBootstrap: true, GenesisBootstrap: true,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	key := requiredKey(l.client.prefix)
	compares, err := buildMissingZeroWriterPolicyBootstrapCompares(
		l.client.prefix, l.token, key, recordKey, genesisContinuityToken)
	if err != nil {
		return err
	}
	resp, err := l.client.cli.Txn(ctx).If(compares...).Then(
		clientv3.OpPut(key, RequiredValueV3), clientv3.OpPut(recordKey, string(payload))).Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return fmt.Errorf("fresh V3 genesis CAS failed; required/record/continuity, lock, or capability prefix changed")
	}
	return nil
}

// ValidateGenesisContinuityToken validates the random token mirrored by the
// immutable Kubernetes genesis marker and the etcd data volume sentinel.
func ValidateGenesisContinuityToken(token string) error {
	const prefix = "nonce:"
	if !strings.HasPrefix(token, prefix) {
		return fmt.Errorf("genesis continuity token must use nonce:<64 lowercase hex>")
	}
	rawHex := strings.TrimPrefix(token, prefix)
	raw, err := hex.DecodeString(rawHex)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != rawHex {
		return fmt.Errorf("genesis continuity token must use nonce:<64 lowercase hex>")
	}
	return nil
}

func genesisContinuityKey(prefix string) string {
	// Keep the sentinel outside the authoritative DS-auth prefix. This lets
	// prepare/re-entry prove that the complete prefix is empty and lets the
	// genesis CAS prove that the activation lock is its only pre-existing key.
	return strings.TrimSuffix(cleanPrefix(prefix), "/") + "-genesis-continuity"
}

// PrepareMissingRequiredPolicyV3Continuity creates the data-volume sentinel
// before Kubernetes is allowed to advance its marker to pending. The create is
// one transaction that also proves required/record/capabilities/activation-lock
// are still absent. Re-entry accepts only the exact immutable sentinel while
// required/record/capabilities remain absent.
func (c *ActivationClient) PrepareMissingRequiredPolicyV3Continuity(ctx context.Context, token string) error {
	if c == nil || c.cli == nil {
		return fmt.Errorf("activation client is missing")
	}
	if err := ValidateGenesisContinuityToken(token); err != nil {
		return err
	}
	continuityKey := genesisContinuityKey(c.prefix)
	compares := buildMissingZeroWriterContinuityPrepareCompares(c.prefix, continuityKey)
	resp, err := c.cli.Txn(ctx).If(compares...).Then(clientv3.OpPut(continuityKey, token)).Commit()
	if err != nil {
		return err
	}
	if resp.Succeeded {
		return nil
	}
	if err := c.verifyMissingRequiredPolicyV3Continuity(ctx, token); err != nil {
		return fmt.Errorf("genesis continuity prepare CAS failed: %w", err)
	}
	return nil
}

// VerifyGenesisContinuity proves the exact create-only sentinel is still on
// the etcd data volume. A pending/complete Kubernetes marker must never recreate
// a missing sentinel; absence means data continuity was lost.
func (c *ActivationClient) VerifyGenesisContinuity(ctx context.Context, token string) error {
	if c == nil || c.cli == nil {
		return fmt.Errorf("activation client is missing")
	}
	if err := ValidateGenesisContinuityToken(token); err != nil {
		return err
	}
	resp, err := c.cli.Get(ctx, genesisContinuityKey(c.prefix))
	if err != nil {
		return err
	}
	if len(resp.Kvs) != 1 {
		return fmt.Errorf("genesis continuity sentinel missing")
	}
	kv := resp.Kvs[0]
	if string(kv.Value) != token || kv.Version != 1 || kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision {
		return fmt.Errorf("genesis continuity sentinel is not the exact create-only token")
	}
	return nil
}

func (c *ActivationClient) verifyMissingRequiredPolicyV3Continuity(ctx context.Context, token string) error {
	authorityPrefix := cleanPrefix(c.prefix)
	resp, err := c.cli.Txn(ctx).Then(
		clientv3.OpGet(genesisContinuityKey(c.prefix)),
		clientv3.OpGet(authorityPrefix, clientv3.WithRange(clientv3.GetPrefixRangeEnd(authorityPrefix)), clientv3.WithLimit(1)),
	).Commit()
	if err != nil {
		return err
	}
	if len(resp.Responses) != 2 {
		return fmt.Errorf("genesis continuity verification read is incomplete")
	}
	sentinelResp := resp.Responses[0].GetResponseRange()
	authorityResp := resp.Responses[1].GetResponseRange()
	if sentinelResp == nil || len(sentinelResp.Kvs) != 1 {
		return fmt.Errorf("genesis continuity sentinel missing")
	}
	sentinelKV := sentinelResp.Kvs[0]
	if string(sentinelKV.Value) != token || sentinelKV.Version != 1 ||
		sentinelKV.CreateRevision <= 0 || sentinelKV.ModRevision != sentinelKV.CreateRevision {
		return fmt.Errorf("genesis continuity sentinel is not the exact create-only token")
	}
	if authorityResp == nil || len(authorityResp.Kvs) != 0 {
		return fmt.Errorf("DS-auth authority prefix is not empty after genesis continuity prepare")
	}
	return nil
}

// VerifyRequiredPolicyV3ActivationEvidence verifies the immutable V3 policy
// transition record for either a direct V1→V3 or an incremental V2→V3 move.
func (c *ActivationClient) VerifyRequiredPolicyV3ActivationEvidence(ctx context.Context,
	activationEvidenceSHA256 string, activationEvidenceCompletedAtMS int64) error {
	return c.verifyRequiredPolicyV3ActivationEvidence(ctx, activationEvidenceSHA256,
		activationEvidenceCompletedAtMS, "")
}

// VerifyRequiredPolicyV3ActivationEvidenceAndContinuity reads required, the
// immutable activation record, and the exact data-volume sentinel in one etcd
// transaction. Local pending/complete markers use this instead of separate
// reads so a volume replacement cannot be hidden between checks.
func (c *ActivationClient) VerifyRequiredPolicyV3ActivationEvidenceAndContinuity(ctx context.Context,
	activationEvidenceSHA256 string, activationEvidenceCompletedAtMS int64,
	genesisContinuityToken string) error {
	if err := ValidateGenesisContinuityToken(genesisContinuityToken); err != nil {
		return err
	}
	return c.verifyRequiredPolicyV3ActivationEvidence(ctx, activationEvidenceSHA256,
		activationEvidenceCompletedAtMS, genesisContinuityToken)
}

func (c *ActivationClient) verifyRequiredPolicyV3ActivationEvidence(ctx context.Context,
	activationEvidenceSHA256 string, activationEvidenceCompletedAtMS int64,
	genesisContinuityToken string) error {
	if err := validateActivationEvidenceSHA256(activationEvidenceSHA256); err != nil {
		return err
	}
	record, err := c.verifyRequiredPolicyV3ActivationRecordWithContinuity(ctx, genesisContinuityToken)
	if err != nil {
		return err
	}
	if record.ActivationEvidenceSHA256 != activationEvidenceSHA256 ||
		record.ActivationEvidenceCompletedAtMS != activationEvidenceCompletedAtMS {
		return fmt.Errorf("V3 policy activation record evidence mismatch")
	}
	return nil
}

// VerifyRequiredPolicyV3ActivationRecord is a read-only startup/resume proof.
// It validates that required V3 and a canonical create-only activation record
// were written in the same transaction, including zero-writer provenance for
// genesis/V1 paths. Production migration additionally calls the exact-evidence
// variant above to bind the external immutable staging marker.
func (c *ActivationClient) VerifyRequiredPolicyV3ActivationRecord(ctx context.Context) error {
	_, err := c.verifyRequiredPolicyV3ActivationRecord(ctx)
	return err
}

func validateZeroWriterServicesTopology(record ActivationRecord) error {
	if record.ZeroWriterBootstrap && record.ExpectedServicesHash != ExpectedServicesHash(map[string]int{}) {
		return fmt.Errorf("V3 zero-writer activation record has a non-empty services topology")
	}
	return nil
}

// validateGenesisContinuityActivationProvenance prevents a continuity
// sentinel from being combined with an unrelated V1/V2 migration record. A
// local pending marker owns a sentinel only for the direct missing->V3 genesis
// transaction, and that transaction must happen strictly after the create-only
// sentinel was persisted.
func validateGenesisContinuityActivationProvenance(record ActivationRecord,
	continuityCreateRevision, recordCreateRevision int64) error {
	if record.FromPolicyGeneration != 0 || !record.GenesisBootstrap ||
		!record.ZeroWriterBootstrap || record.From != 0 ||
		record.FromRequiredValue != "" || record.FromModRevision != 0 {
		return fmt.Errorf("V3 activation record is not the canonical continuity genesis")
	}
	if continuityCreateRevision <= 0 || recordCreateRevision <= continuityCreateRevision {
		return fmt.Errorf("V3 continuity sentinel was not created before the genesis record")
	}
	return nil
}

func (c *ActivationClient) verifyRequiredPolicyV3ActivationRecord(ctx context.Context) (ActivationRecord, error) {
	return c.verifyRequiredPolicyV3ActivationRecordWithContinuity(ctx, "")
}

func (c *ActivationClient) verifyRequiredPolicyV3ActivationRecordWithContinuity(ctx context.Context,
	genesisContinuityToken string) (ActivationRecord, error) {
	requiredEpochKey := requiredKey(c.prefix)
	recordKey := policyActivationRecordKey(c.prefix, RequiredPolicyGenerationV3, RequiredPolicyV3)
	ops := []clientv3.Op{clientv3.OpGet(requiredEpochKey), clientv3.OpGet(recordKey)}
	if genesisContinuityToken != "" {
		if err := ValidateGenesisContinuityToken(genesisContinuityToken); err != nil {
			return ActivationRecord{}, err
		}
		ops = append(ops, clientv3.OpGet(genesisContinuityKey(c.prefix)))
	}
	resp, err := c.cli.Txn(ctx).Then(ops...).Commit()
	if err != nil {
		return ActivationRecord{}, err
	}
	if len(resp.Responses) != len(ops) {
		return ActivationRecord{}, fmt.Errorf("V3 policy activation evidence read is incomplete")
	}
	requiredResp, recordResp := resp.Responses[0].GetResponseRange(), resp.Responses[1].GetResponseRange()
	if requiredResp == nil || len(requiredResp.Kvs) != 1 || string(requiredResp.Kvs[0].Value) != RequiredValueV3 ||
		recordResp == nil || len(recordResp.Kvs) != 1 {
		return ActivationRecord{}, fmt.Errorf("required V3 policy or immutable activation record missing")
	}
	requiredKV, recordKV := requiredResp.Kvs[0], recordResp.Kvs[0]
	var continuityCreateRevision int64
	if genesisContinuityToken != "" {
		continuityResp := resp.Responses[2].GetResponseRange()
		if continuityResp == nil || len(continuityResp.Kvs) != 1 {
			return ActivationRecord{}, fmt.Errorf("genesis continuity sentinel missing from V3 evidence transaction")
		}
		continuityKV := continuityResp.Kvs[0]
		if string(continuityKV.Value) != genesisContinuityToken || continuityKV.Version != 1 ||
			continuityKV.CreateRevision <= 0 || continuityKV.ModRevision != continuityKV.CreateRevision {
			return ActivationRecord{}, fmt.Errorf("genesis continuity sentinel is not the exact create-only token")
		}
		continuityCreateRevision = continuityKV.CreateRevision
	}
	if recordKV.CreateRevision <= 0 || recordKV.Version != 1 ||
		recordKV.ModRevision != recordKV.CreateRevision || requiredKV.ModRevision != recordKV.CreateRevision {
		return ActivationRecord{}, fmt.Errorf("V3 policy activation record is not the immutable required-policy transaction")
	}
	record, err := decodeActivationRecord(recordKV.Value)
	if err != nil {
		return ActivationRecord{}, fmt.Errorf("decode V3 policy activation record: %w", err)
	}
	if genesisContinuityToken != "" {
		if err := validateGenesisContinuityActivationProvenance(record,
			continuityCreateRevision, recordKV.CreateRevision); err != nil {
			return ActivationRecord{}, err
		}
	}
	validFrom := false
	switch record.FromPolicyGeneration {
	case 0:
		validFrom = record.GenesisBootstrap && record.ZeroWriterBootstrap && record.From == 0 &&
			record.FromRequiredValue == "" && record.FromModRevision == 0
	case RequiredPolicyGenerationV1, RequiredPolicyGenerationV2:
		fromValue, valueErr := RequiredValueForPolicyGeneration(record.FromPolicyGeneration)
		fromWriter, writerErr := requiredWriterEpochForPolicyGeneration(record.FromPolicyGeneration)
		validFrom = valueErr == nil && writerErr == nil && record.From == fromWriter &&
			record.FromRequiredValue == fromValue && record.FromModRevision > 0 &&
			record.FromModRevision < recordKV.CreateRevision && !record.GenesisBootstrap &&
			((record.FromPolicyGeneration == RequiredPolicyGenerationV1) == record.ZeroWriterBootstrap)
	}
	if !validFrom || record.ToPolicyGeneration != RequiredPolicyGenerationV3 ||
		record.To != ProtocolEpochV2 ||
		record.ToRequiredValue != RequiredValueV3 || record.RequiredPolicyID != RequiredPolicyV3 ||
		!isCanonicalLowerHexSHA256(record.ExpectedServicesHash) || record.ActivatedAtMS <= 0 ||
		record.ActivationEvidenceCompletedAtMS <= 0 ||
		record.ActivationEvidenceCompletedAtMS > record.ActivatedAtMS {
		return ActivationRecord{}, fmt.Errorf("V3 policy activation record is not canonical")
	}
	if err := validateZeroWriterServicesTopology(record); err != nil {
		return ActivationRecord{}, err
	}
	if record.GenesisBootstrap && (requiredKV.Version != 1 ||
		requiredKV.CreateRevision != recordKV.CreateRevision) {
		return ActivationRecord{}, fmt.Errorf("V3 genesis record is not a create-only empty-services transaction")
	}
	if err := validateActivationEvidenceSHA256(record.ActivationEvidenceSHA256); err != nil {
		return ActivationRecord{}, fmt.Errorf("V3 policy activation record has invalid evidence: %w", err)
	}
	return record, nil
}

// VerifyActivationEvidence performs a linear read of the immutable target
// activation record and requires the exact evidence digest.  Legacy epoch-1
// state has no activation record and remains readable; epoch-2 never falls
// back when the record or evidence field is absent/malformed.
func (c *ActivationClient) VerifyActivationEvidence(ctx context.Context, target uint32, activationEvidenceSHA256 string, activationEvidenceCompletedAtMS int64) error {
	if target <= 1 {
		return fmt.Errorf("activation evidence target must be greater than baseline: %d", target)
	}
	if err := validateActivationEvidenceSHA256(activationEvidenceSHA256); err != nil {
		return err
	}
	requiredEpochKey := requiredKey(c.prefix)
	recordKey := cleanPrefix(c.prefix) + "activations/" + strconv.FormatUint(uint64(target), 10)
	resp, err := c.cli.Txn(ctx).Then(
		clientv3.OpGet(requiredEpochKey),
		clientv3.OpGet(recordKey),
	).Commit()
	if err != nil {
		return err
	}
	if len(resp.Responses) != 2 {
		return fmt.Errorf("activation evidence read for epoch %d is incomplete", target)
	}
	requiredResp := resp.Responses[0].GetResponseRange()
	recordResp := resp.Responses[1].GetResponseRange()
	targetValue, valueErr := RequiredValueForEpoch(target)
	if valueErr != nil {
		return valueErr
	}
	if requiredResp == nil || len(requiredResp.Kvs) != 1 ||
		string(requiredResp.Kvs[0].Value) != targetValue {
		return fmt.Errorf("required epoch %d is not current while verifying activation evidence", target)
	}
	if recordResp == nil || len(recordResp.Kvs) != 1 {
		return fmt.Errorf("activation record for epoch %d missing", target)
	}
	requiredKV := requiredResp.Kvs[0]
	recordKV := recordResp.Kvs[0]
	// Both keys were written by the same activation transaction.  Version=1
	// additionally rejects any later overwrite of the nominally immutable
	// record, even when an attacker preserves the JSON fields.
	if recordKV.CreateRevision <= 0 || recordKV.Version != 1 ||
		recordKV.ModRevision != recordKV.CreateRevision ||
		requiredKV.ModRevision != recordKV.CreateRevision {
		return fmt.Errorf("activation record for epoch %d is not the immutable required-epoch transaction", target)
	}
	record, err := decodeActivationRecord(recordKV.Value)
	if err != nil {
		return fmt.Errorf("decode activation record for epoch %d: %w", target, err)
	}
	if record.From == 0 || record.To != target || record.From >= record.To ||
		record.FromRequiredValue != "1" || record.ToRequiredValue != targetValue ||
		record.RequiredPolicyID != RequiredPolicyV2 ||
		record.FromModRevision <= 0 || record.FromModRevision >= recordKV.CreateRevision ||
		!isCanonicalLowerHexSHA256(record.ExpectedServicesHash) || record.ActivatedAtMS <= 0 ||
		record.ActivationEvidenceCompletedAtMS <= 0 || record.ActivationEvidenceCompletedAtMS > record.ActivatedAtMS {
		return fmt.Errorf("activation record for epoch %d is not canonical", target)
	}
	if err := validateActivationEvidenceSHA256(record.ActivationEvidenceSHA256); err != nil {
		return fmt.Errorf("activation record for epoch %d has invalid evidence: %w", target, err)
	}
	if record.ActivationEvidenceSHA256 != activationEvidenceSHA256 {
		return fmt.Errorf("activation record evidence mismatch for epoch %d", target)
	}
	if record.ActivationEvidenceCompletedAtMS != activationEvidenceCompletedAtMS {
		return fmt.Errorf("activation record evidence completion mismatch for epoch %d", target)
	}
	return nil
}

func decodeActivationRecord(payload []byte) (ActivationRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var record ActivationRecord
	if err := decoder.Decode(&record); err != nil {
		return ActivationRecord{}, err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ActivationRecord{}, fmt.Errorf("activation record contains trailing JSON")
		}
		return ActivationRecord{}, err
	}
	return record, nil
}

func isCanonicalLowerHexSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validateActivationEvidenceSHA256(value string) error {
	if !digestPattern.MatchString(value) {
		return fmt.Errorf("activation evidence must be immutable sha256 digest")
	}
	return nil
}

// ValidateActivationEvidenceInput keeps legacy epoch-1 read-only audits
// compatible while making an epoch advance and every already-target audit
// fail closed unless both pieces of external evidence are present.
func ValidateActivationEvidenceInput(current, target uint32, digest string, completedAtMS int64, advancing bool) error {
	required := current == target || advancing
	if !required && digest == "" && completedAtMS == 0 {
		return nil
	}
	if err := validateActivationEvidenceSHA256(digest); err != nil {
		return err
	}
	if completedAtMS <= 0 {
		return fmt.Errorf("activation evidence completion time is required")
	}
	return nil
}

func buildAdvanceCompares(
	prefix, lockToken, key, recordKey string,
	expectedRequiredValue string,
	expectedModRevision int64,
	audited []LiveCapability,
) ([]clientv3.Cmp, error) {
	if expectedRequiredValue == "" || expectedModRevision <= 0 || key == "" || recordKey == "" || lockToken == "" {
		return nil, fmt.Errorf("invalid required advance snapshot")
	}
	compares := []clientv3.Cmp{
		clientv3.Compare(clientv3.Value(key), "=", expectedRequiredValue),
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

func buildZeroWriterPolicyAdvanceCompares(prefix, lockToken, key, recordKey string,
	expectedModRevision int64) ([]clientv3.Cmp, error) {
	if expectedModRevision <= 0 || key == "" || recordKey == "" || lockToken == "" {
		return nil, fmt.Errorf("invalid zero-writer policy advance snapshot")
	}
	capabilities := capabilityPrefix(prefix)
	return []clientv3.Cmp{
		clientv3.Compare(clientv3.Value(key), "=", "1"),
		clientv3.Compare(clientv3.ModRevision(key), "=", expectedModRevision),
		clientv3.Compare(clientv3.CreateRevision(recordKey), "=", 0),
		clientv3.Compare(clientv3.Value(activationLockKey(prefix)), "=", lockToken),
		clientv3.Compare(clientv3.CreateRevision(capabilities), "=", 0).
			WithRange(clientv3.GetPrefixRangeEnd(capabilities)),
	}, nil
}

func buildMissingZeroWriterContinuityPrepareCompares(prefix, continuityKey string) []clientv3.Cmp {
	authorityPrefix := cleanPrefix(prefix)
	return []clientv3.Cmp{
		clientv3.Compare(clientv3.CreateRevision(continuityKey), "=", 0),
		clientv3.Compare(clientv3.CreateRevision(authorityPrefix), "=", 0).
			WithRange(clientv3.GetPrefixRangeEnd(authorityPrefix)),
	}
}

func buildMissingZeroWriterPolicyBootstrapCompares(prefix, lockToken, key, recordKey,
	continuityToken string) ([]clientv3.Cmp, error) {
	if key == "" || recordKey == "" || lockToken == "" {
		return nil, fmt.Errorf("invalid missing zero-writer V3 bootstrap snapshot")
	}
	if err := ValidateGenesisContinuityToken(continuityToken); err != nil {
		return nil, err
	}
	capabilities := capabilityPrefix(prefix)
	continuityKey := genesisContinuityKey(prefix)
	authorityPrefix := cleanPrefix(prefix)
	authorityEnd := clientv3.GetPrefixRangeEnd(authorityPrefix)
	lockKey := activationLockKey(prefix)
	return []clientv3.Cmp{
		clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
		clientv3.Compare(clientv3.CreateRevision(recordKey), "=", 0),
		clientv3.Compare(clientv3.Value(lockKey), "=", lockToken),
		clientv3.Compare(clientv3.CreateRevision(authorityPrefix), "=", 0).WithRange(lockKey),
		clientv3.Compare(clientv3.CreateRevision(lockKey+"\x00"), "=", 0).WithRange(authorityEnd),
		clientv3.Compare(clientv3.Version(continuityKey), "=", 1),
		clientv3.Compare(clientv3.Value(continuityKey), "=", continuityToken),
		clientv3.Compare(clientv3.CreateRevision(capabilities), "=", 0).
			WithRange(clientv3.GetPrefixRangeEnd(capabilities)),
	}, nil
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
