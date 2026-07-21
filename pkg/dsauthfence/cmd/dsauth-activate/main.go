// dsauth-activate 默认只审计 capability；只有显式 -apply 才推进全局 required epoch。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/luyuancpp/pandora/pkg/dsauthfence"
)

func main() {
	var (
		endpoints                       = flag.String("endpoints", "", "etcd endpoints, comma separated")
		prefix                          = flag.String("prefix", dsauthfence.DefaultPrefix, "DS auth fence key prefix")
		expectedRaw                     = flag.String("expected-services", "", "exact live capabilities, service=count,...")
		expectedInstancesRaw            = flag.String("expected-instances", "", "exact K8s Pod UIDs, service=uid|uid,...")
		keysetRevision                  = flag.String("keyset-revision", "", "exact immutable Secret/keyset revision expected on every capability")
		etcdIdentityRevision            = flag.String("etcd-identity-revision", "", "exact revisioned immutable etcd client identity expected on every capability")
		allowedDigestsRaw               = flag.String("allowed-image-digests", "", "comma-separated immutable sha256 digests allowed in this activation")
		expectedDigestsRaw              = flag.String("expected-image-digests", "", "exact service-level immutable digests, service=sha256:...,...")
		requiredFeaturesRaw             = flag.String("required-features", "", "exact versioned capabilities, service=feature|feature,...")
		activationEvidence              = flag.String("activation-evidence-sha256", "", "exact immutable Kubernetes activation evidence digest bound to epoch transition")
		activationEvidenceCompletedAtMS = flag.Int64("activation-evidence-completed-at-ms", 0, "exact completed-at Unix milliseconds bound to activation evidence")
		expected                        = flag.Uint("expected-epoch", 1, "current required writer epoch")
		target                          = flag.Uint("target-epoch", uint(dsauthfence.ProtocolEpochV2), "target writer epoch")
		bootstrap                       = flag.Bool("bootstrap", false, "CAS-create missing required epoch at expected-epoch")
		apply                           = flag.Bool("apply", false, "advance required epoch after a clean audit")
		policyV3                        = flag.Bool("policy-v3", false, "audit/apply the immutable V2->V3 policy-only transition")
		zeroWriterV3                    = flag.Bool("zero-writer-v1-to-v3", false, "audit/apply the fresh-cluster V1->V3 transition with an empty capability prefix")
		genesisV3                       = flag.Bool("zero-writer-genesis-v3", false, "single-transaction fresh-cluster missing->V3 genesis with an empty capability prefix")
		prepareGenesisV3                = flag.Bool("prepare-zero-writer-genesis-v3", false, "create/verify the exact data-volume continuity sentinel before missing->V3 genesis")
		verifyGenesisContinuity         = flag.Bool("verify-genesis-continuity", false, "read-only exact verification of the data-volume continuity sentinel")
		genesisContinuityToken          = flag.String("genesis-continuity-token", "", "exact random nonce mirrored by the immutable Kubernetes genesis marker")
		requireEmpty                    = flag.Bool("require-empty-capabilities", false, "read-only proof that the DS auth capability prefix is empty")
		timeout                         = flag.Duration("timeout", 10*time.Second, "single audit/apply timeout")
		requireMTLS                     = flag.Bool("require-mtls", false, "require custom-CA mutual TLS for etcd")
		caFile                          = flag.String("ca-file", "", "custom etcd CA PEM path")
		certFile                        = flag.String("cert-file", "", "etcd client certificate PEM path")
		keyFile                         = flag.String("key-file", "", "etcd client private-key PEM path")
		serverName                      = flag.String("server-name", "", "exact etcd TLS server name")
		clientIdentity                  = flag.String("client-identity", "", "exact etcd client certificate common name")
		usernameFile                    = flag.String("username-file", "", "optional etcd username file path")
		passwordFile                    = flag.String("password-file", "", "optional etcd password file path")
		requireAuth                     = flag.Bool("require-auth", false, "require etcd v3 authentication to be enabled")
		forbiddenReadPrefix             = flag.String("forbidden-read-prefix", "", "prefix this identity must be denied from reading")
	)
	flag.Parse()
	explicitFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })
	modeCount := 0
	for _, selected := range []bool{*policyV3, *zeroWriterV3, *genesisV3, *prepareGenesisV3, *verifyGenesisContinuity} {
		if selected {
			modeCount++
		}
	}
	if modeCount > 1 {
		must(fmt.Errorf("policy-v3, zero-writer V1->V3, genesis prepare, and genesis apply modes are mutually exclusive"))
	}
	if modeCount != 0 && *bootstrap {
		must(fmt.Errorf("policy transition modes cannot be combined with -bootstrap; create baseline V1 first"))
	}
	if modeCount == 0 && (*expected != 1 || *target != uint(dsauthfence.ProtocolEpochV2)) {
		must(fmt.Errorf("activation policy is fixed to required 1 -> %d", dsauthfence.ProtocolEpochV2))
	}
	if modeCount == 0 {
		must(validateApplyMode(*bootstrap, *apply))
	}
	if (*prepareGenesisV3 || *genesisV3 || *verifyGenesisContinuity) && *genesisContinuityToken == "" {
		must(fmt.Errorf("-genesis-continuity-token is required for genesis prepare/apply"))
	}
	if !*prepareGenesisV3 && !*genesisV3 && !*verifyGenesisContinuity && *genesisContinuityToken != "" {
		must(fmt.Errorf("-genesis-continuity-token is only valid for genesis prepare/apply"))
	}
	if *prepareGenesisV3 || *verifyGenesisContinuity || *genesisV3 {
		must(rejectExplicitFlags("genesis continuity mode", explicitFlags,
			"expected-services", "expected-instances", "keyset-revision",
			"allowed-image-digests", "expected-image-digests", "required-features",
			"expected-epoch", "target-epoch", "require-empty-capabilities"))
	}
	if (*prepareGenesisV3 || *verifyGenesisContinuity) &&
		(explicitFlags["activation-evidence-sha256"] || explicitFlags["activation-evidence-completed-at-ms"]) {
		must(fmt.Errorf("genesis prepare/verify cannot accept activation evidence before the Kubernetes marker is pending"))
	}
	if *verifyGenesisContinuity && *apply {
		must(fmt.Errorf("-verify-genesis-continuity is read-only and cannot be combined with -apply"))
	}

	services := make(map[string]int)
	instances := make(map[string]map[string]struct{})
	requiredFeatures := make(map[string]map[string]struct{})
	expectedDigests := make(map[string]string)
	var err error
	needsCapabilityAudit := !*requireEmpty && !*bootstrap && !*zeroWriterV3 && !*genesisV3 &&
		!*prepareGenesisV3 && !*verifyGenesisContinuity
	if needsCapabilityAudit {
		services, err = dsauthfence.ParseExpectedServices(*expectedRaw)
		must(err)
		instances, err = dsauthfence.ParseExpectedInstances(*expectedInstancesRaw)
		must(err)
		requiredFeatures, err = dsauthfence.ParseRequiredFeatures(*requiredFeaturesRaw)
		must(err)
		expectedDigests, err = dsauthfence.ParseExpectedDigests(*expectedDigestsRaw)
		must(err)
		if *policyV3 {
			must(dsauthfence.ValidateActivationPolicyGeneration(
				dsauthfence.RequiredPolicyGenerationV3, services, requiredFeatures))
		} else {
			must(dsauthfence.ValidateActivationPolicy(uint32(*target), services, requiredFeatures))
		}
	}
	if *bootstrap && *requireEmpty {
		must(fmt.Errorf("-bootstrap cannot be combined with -require-empty-capabilities"))
	}
	if *requireMTLS && *etcdIdentityRevision == "" {
		must(fmt.Errorf("-etcd-identity-revision is required with -require-mtls"))
	}
	allowedDigests := make(map[string]struct{})
	if needsCapabilityAudit || *requireEmpty {
		if *keysetRevision == "" {
			must(fmt.Errorf("-keyset-revision is required"))
		}
		for _, digest := range splitNonEmpty(*allowedDigestsRaw) {
			allowedDigests[digest] = struct{}{}
		}
		if len(allowedDigests) == 0 {
			must(fmt.Errorf("-allowed-image-digests is required"))
		}
	}
	items := splitNonEmpty(*endpoints)
	if len(items) == 0 {
		must(fmt.Errorf("-endpoints is required"))
	}
	client, err := dsauthfence.NewActivationClientWithSecurity(items, *prefix, *timeout, dsauthfence.ClientSecurity{
		RequireMTLS:         *requireMTLS,
		CAFile:              *caFile,
		CertFile:            *certFile,
		KeyFile:             *keyFile,
		ServerName:          *serverName,
		ClientIdentity:      *clientIdentity,
		IdentityRevision:    *etcdIdentityRevision,
		UsernameFile:        *usernameFile,
		PasswordFile:        *passwordFile,
		RequireAuth:         *requireAuth,
		ForbiddenReadPrefix: *forbiddenReadPrefix,
	})
	must(err)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if *verifyGenesisContinuity {
		must(dsauthfence.ValidateGenesisContinuityToken(*genesisContinuityToken))
		must(client.VerifyGenesisContinuity(ctx, *genesisContinuityToken))
		fmt.Println("fresh genesis 数据盘 continuity sentinel 精确只读验证通过")
		return
	}
	if *prepareGenesisV3 {
		if !*apply {
			must(fmt.Errorf("-prepare-zero-writer-genesis-v3 requires explicit -apply"))
		}
		must(dsauthfence.ValidateGenesisContinuityToken(*genesisContinuityToken))
		must(client.PrepareMissingRequiredPolicyV3Continuity(ctx, *genesisContinuityToken))
		fmt.Println("已 create-only 建立/精确回读 fresh genesis 数据盘 continuity sentinel")
		return
	}
	if *bootstrap {
		if *expected != 1 || *target != uint(dsauthfence.ProtocolEpochV2) {
			must(fmt.Errorf("bootstrap is fixed to baseline expected-epoch=1 and target-epoch=%d", dsauthfence.ProtocolEpochV2))
		}
		if *apply {
			must(client.BootstrapRequired(ctx, 1))
			fmt.Println("已初始化 required_writer_epoch=1；bootstrap 命令到此结束，请另起 audit/apply 命令推进")
		} else {
			fmt.Println("审计模式：-bootstrap 未执行；加 -apply 只会 CAS 初始化 baseline=1，不会推进协议")
		}
		return
	}
	if *genesisV3 {
		if !*apply {
			must(fmt.Errorf("-zero-writer-genesis-v3 requires explicit -apply; its CAS is already a read+write proof"))
		}
		if *requireEmpty {
			must(fmt.Errorf("-zero-writer-genesis-v3 already proves an empty capability prefix in its transaction"))
		}
		must(dsauthfence.ValidateActivationEvidenceInput(0, dsauthfence.RequiredPolicyGenerationV3,
			*activationEvidence, *activationEvidenceCompletedAtMS, true))
		must(dsauthfence.ValidateGenesisContinuityToken(*genesisContinuityToken))
		lock, err := client.AcquireLock(ctx, 30)
		must(err)
		defer func() { _ = lock.Close() }()
		must(lock.BootstrapRequiredPolicyV3FromMissing(ctx, *activationEvidence,
			*activationEvidenceCompletedAtMS, *genesisContinuityToken))
		must(client.VerifyRequiredPolicyV3ActivationEvidenceAndContinuity(ctx, *activationEvidence,
			*activationEvidenceCompletedAtMS, *genesisContinuityToken))
		fmt.Println("已单事务完成 fresh-cluster missing -> V3 genesis（required + immutable record + lock + empty capability prefix）")
		return
	}
	if *zeroWriterV3 && *requireEmpty {
		must(fmt.Errorf("-zero-writer-v1-to-v3 already requires an empty capability prefix"))
	}
	if *requireEmpty && *apply {
		must(fmt.Errorf("-require-empty-capabilities is read-only and cannot be combined with -apply"))
	}
	var lock *dsauthfence.ActivationLock
	if *apply {
		lock, err = client.AcquireLock(ctx, 30)
		must(err)
		defer func() { _ = lock.Close() }()
	}
	requiredSnapshot, err := client.RequiredSnapshot(ctx)
	must(err)
	if *zeroWriterV3 {
		must(runZeroWriterV3(ctx, client, lock, requiredSnapshot, *apply,
			*activationEvidence, *activationEvidenceCompletedAtMS))
		return
	}
	current := requiredSnapshot.Epoch
	if *policyV3 {
		if requiredSnapshot.PolicyGeneration != dsauthfence.RequiredPolicyGenerationV2 &&
			requiredSnapshot.PolicyGeneration != dsauthfence.RequiredPolicyGenerationV3 {
			must(fmt.Errorf("current required policy generation=%d, expected V2 or already-target V3",
				requiredSnapshot.PolicyGeneration))
		}
	} else if current != uint32(*expected) && current != uint32(*target) {
		must(fmt.Errorf("current required epoch=%d, expected=%d or already-target=%d", current, *expected, *target))
	}
	if *policyV3 {
		must(dsauthfence.ValidateActivationEvidenceInput(requiredSnapshot.PolicyGeneration,
			dsauthfence.RequiredPolicyGenerationV3, *activationEvidence,
			*activationEvidenceCompletedAtMS, *apply))
	} else {
		must(dsauthfence.ValidateActivationEvidenceInput(current, uint32(*target),
			*activationEvidence, *activationEvidenceCompletedAtMS, *apply))
	}
	capabilities, err := client.Capabilities(ctx)
	must(err)
	if *requireEmpty {
		if len(capabilities) != 0 {
			for _, capability := range capabilities {
				fmt.Fprintf(os.Stderr, "FAIL: live capability remains: %s/%s\n", capability.Capability.Service, capability.Capability.InstanceUID)
			}
			os.Exit(2)
		}
		fmt.Printf("只读空窗审计通过：required=%d capabilities=0\n", current)
		return
	}
	findings := dsauthfence.AuditCapabilities(capabilities, dsauthfence.AuditPolicy{
		Prefix: *prefix, RequiredServices: services, RequiredInstances: instances,
		TargetEpoch: uint32(*target), TargetPolicyGeneration: func() uint32 {
			if *policyV3 {
				return dsauthfence.RequiredPolicyGenerationV3
			}
			return 0
		}(),
		ExpectedAcquiredPolicyGeneration: requiredSnapshot.PolicyGeneration,
		KeysetRevision:                   *keysetRevision, EtcdIdentityRevision: *etcdIdentityRevision,
		AllowedDigests: allowedDigests, ExpectedDigests: expectedDigests,
		RequiredFeatures: requiredFeatures,
	})
	if len(findings) > 0 {
		for _, finding := range findings {
			fmt.Fprintln(os.Stderr, "FAIL:", finding)
		}
		os.Exit(2)
	}
	if *policyV3 {
		fmt.Printf("V3 policy staging capability 审计通过：required_generation=%d target_generation=%d capabilities=%d（Hub Pod 无需 Ready/Service Endpoint）\n",
			requiredSnapshot.PolicyGeneration, dsauthfence.RequiredPolicyGenerationV3, len(capabilities))
	} else {
		fmt.Printf("审计通过：required=%d target=%d capabilities=%d\n", current, *target, len(capabilities))
	}
	if *policyV3 && requiredSnapshot.PolicyGeneration == dsauthfence.RequiredPolicyGenerationV3 {
		must(client.VerifyRequiredPolicyV3ActivationEvidence(ctx, *activationEvidence, *activationEvidenceCompletedAtMS))
		fmt.Println("required policy 已在 V3；按幂等重试处理，未执行重复写")
		return
	}
	if !*policyV3 && current == uint32(*target) {
		must(client.VerifyActivationEvidence(ctx, uint32(*target), *activationEvidence, *activationEvidenceCompletedAtMS))
		fmt.Println("required_writer_epoch 已在 target；按幂等重试处理，未执行回退/重复写")
		return
	}
	if !*apply {
		fmt.Println("未改动 etcd；显式加 -apply 才会推进")
		return
	}
	if *policyV3 {
		must(lock.AdvanceRequiredPolicyV3(ctx, requiredSnapshot, services, capabilities,
			*activationEvidence, *activationEvidenceCompletedAtMS))
		fmt.Printf("已原子推进 required policy: V2 -> V3（writer epoch 保持 %d）\n", dsauthfence.ProtocolEpochV2)
		return
	}
	must(lock.AdvanceRequired(ctx, uint32(*expected), uint32(*target), requiredSnapshot.ModRevision, services, capabilities, *activationEvidence, *activationEvidenceCompletedAtMS))
	fmt.Printf("已原子推进 required_writer_epoch: %d -> %d\n", *expected, *target)
}

func runZeroWriterV3(ctx context.Context, client *dsauthfence.ActivationClient,
	lock *dsauthfence.ActivationLock, snapshot dsauthfence.RequiredSnapshot, apply bool,
	evidence string, evidenceCompletedAtMS int64) error {
	if snapshot.PolicyGeneration != dsauthfence.RequiredPolicyGenerationV1 &&
		snapshot.PolicyGeneration != dsauthfence.RequiredPolicyGenerationV3 {
		return fmt.Errorf("zero-writer bootstrap requires V1 or already-target V3, got generation=%d", snapshot.PolicyGeneration)
	}
	if err := dsauthfence.ValidateActivationEvidenceInput(snapshot.PolicyGeneration,
		dsauthfence.RequiredPolicyGenerationV3, evidence, evidenceCompletedAtMS, apply); err != nil {
		return err
	}
	if snapshot.PolicyGeneration == dsauthfence.RequiredPolicyGenerationV3 {
		if err := client.VerifyRequiredPolicyV3ActivationEvidence(ctx, evidence, evidenceCompletedAtMS); err != nil {
			return err
		}
		fmt.Println("required policy 已在 V3；zero-writer bootstrap 按幂等重试处理")
		return nil
	}
	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		return err
	}
	if len(capabilities) != 0 {
		return fmt.Errorf("zero-writer bootstrap refused: %d live capability keys remain", len(capabilities))
	}
	if !apply {
		fmt.Println("zero-writer V1->V3 只读审计通过：capabilities=0；显式加 -apply 才会执行单事务 CAS")
		return nil
	}
	if lock == nil {
		return fmt.Errorf("activation lock is required")
	}
	if err := lock.AdvanceRequiredPolicyV3FromZeroWriters(ctx, snapshot, evidence, evidenceCompletedAtMS); err != nil {
		return err
	}
	fmt.Printf("已原子推进 fresh-cluster required policy: V1 -> V3（capability prefix 在 CAS 内确认为空）\n")
	return nil
}

func validateApplyMode(bootstrap, apply bool) error {
	if apply && !bootstrap {
		return dsauthfence.ErrTopologyChangeLockProviderUnavailable
	}
	return nil
}

func rejectExplicitFlags(mode string, explicit map[string]bool, names ...string) error {
	for _, name := range names {
		if explicit[name] {
			return fmt.Errorf("%s does not accept -%s because that flag would be ignored", mode, name)
		}
	}
	return nil
}

func splitNonEmpty(raw string) []string {
	out := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}
