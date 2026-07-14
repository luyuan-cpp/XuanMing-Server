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
		endpoints            = flag.String("endpoints", "", "etcd endpoints, comma separated")
		prefix               = flag.String("prefix", dsauthfence.DefaultPrefix, "DS auth fence key prefix")
		expectedRaw          = flag.String("expected-services", "", "exact live capabilities, service=count,...")
		expectedInstancesRaw = flag.String("expected-instances", "", "exact K8s Pod UIDs, service=uid|uid,...")
		keysetRevision       = flag.String("keyset-revision", "", "exact immutable Secret/keyset revision expected on every capability")
		etcdIdentityRevision = flag.String("etcd-identity-revision", "", "exact revisioned immutable etcd client identity expected on every capability")
		allowedDigestsRaw    = flag.String("allowed-image-digests", "", "comma-separated immutable sha256 digests allowed in this activation")
		expectedDigestsRaw   = flag.String("expected-image-digests", "", "exact service-level immutable digests, service=sha256:...,...")
		requiredFeaturesRaw  = flag.String("required-features", "", "exact versioned capabilities, service=feature|feature,...")
		expected             = flag.Uint("expected-epoch", 1, "current required writer epoch")
		target               = flag.Uint("target-epoch", uint(dsauthfence.ProtocolEpochV2), "target writer epoch")
		bootstrap            = flag.Bool("bootstrap", false, "CAS-create missing required epoch at expected-epoch")
		apply                = flag.Bool("apply", false, "advance required epoch after a clean audit")
		requireEmpty         = flag.Bool("require-empty-capabilities", false, "read-only proof that the DS auth capability prefix is empty")
		timeout              = flag.Duration("timeout", 10*time.Second, "single audit/apply timeout")
		requireMTLS          = flag.Bool("require-mtls", false, "require custom-CA mutual TLS for etcd")
		caFile               = flag.String("ca-file", "", "custom etcd CA PEM path")
		certFile             = flag.String("cert-file", "", "etcd client certificate PEM path")
		keyFile              = flag.String("key-file", "", "etcd client private-key PEM path")
		serverName           = flag.String("server-name", "", "exact etcd TLS server name")
		clientIdentity       = flag.String("client-identity", "", "exact etcd client certificate common name")
		usernameFile         = flag.String("username-file", "", "optional etcd username file path")
		passwordFile         = flag.String("password-file", "", "optional etcd password file path")
		requireAuth          = flag.Bool("require-auth", false, "require etcd v3 authentication to be enabled")
		forbiddenReadPrefix  = flag.String("forbidden-read-prefix", "", "prefix this identity must be denied from reading")
	)
	flag.Parse()

	services := make(map[string]int)
	instances := make(map[string]map[string]struct{})
	requiredFeatures := make(map[string]map[string]struct{})
	expectedDigests := make(map[string]string)
	var err error
	if !*requireEmpty && !*bootstrap {
		services, err = dsauthfence.ParseExpectedServices(*expectedRaw)
		must(err)
		instances, err = dsauthfence.ParseExpectedInstances(*expectedInstancesRaw)
		must(err)
		requiredFeatures, err = dsauthfence.ParseRequiredFeatures(*requiredFeaturesRaw)
		must(err)
		expectedDigests, err = dsauthfence.ParseExpectedDigests(*expectedDigestsRaw)
		must(err)
	}
	if *bootstrap && *requireEmpty {
		must(fmt.Errorf("-bootstrap cannot be combined with -require-empty-capabilities"))
	}
	if *requireMTLS && *etcdIdentityRevision == "" {
		must(fmt.Errorf("-etcd-identity-revision is required with -require-mtls"))
	}
	allowedDigests := make(map[string]struct{})
	if !*bootstrap {
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
	current := requiredSnapshot.Epoch
	if current != uint32(*expected) && current != uint32(*target) {
		must(fmt.Errorf("current required epoch=%d, expected=%d or already-target=%d", current, *expected, *target))
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
		Prefix:               *prefix,
		RequiredServices:     services,
		RequiredInstances:    instances,
		TargetEpoch:          uint32(*target),
		KeysetRevision:       *keysetRevision,
		EtcdIdentityRevision: *etcdIdentityRevision,
		AllowedDigests:       allowedDigests,
		ExpectedDigests:      expectedDigests,
		RequiredFeatures:     requiredFeatures,
	})
	if len(findings) > 0 {
		for _, finding := range findings {
			fmt.Fprintln(os.Stderr, "FAIL:", finding)
		}
		os.Exit(2)
	}
	fmt.Printf("审计通过：required=%d target=%d capabilities=%d\n", current, *target, len(capabilities))
	if current == uint32(*target) {
		fmt.Println("required_writer_epoch 已在 target；按幂等重试处理，未执行回退/重复写")
		return
	}
	if !*apply {
		fmt.Println("未改动 etcd；显式加 -apply 才会推进")
		return
	}
	must(lock.AdvanceRequired(ctx, uint32(*expected), uint32(*target), requiredSnapshot.ModRevision, services, capabilities))
	fmt.Printf("已原子推进 required_writer_epoch: %d -> %d\n", *expected, *target)
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
