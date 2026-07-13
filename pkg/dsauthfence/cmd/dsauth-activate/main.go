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
		allowedDigestsRaw    = flag.String("allowed-image-digests", "", "comma-separated immutable sha256 digests allowed in this activation")
		expected             = flag.Uint("expected-epoch", 1, "current required writer epoch")
		target               = flag.Uint("target-epoch", uint(dsauthfence.ProtocolEpochV2), "target writer epoch")
		bootstrap            = flag.Bool("bootstrap", false, "CAS-create missing required epoch at expected-epoch")
		apply                = flag.Bool("apply", false, "advance required epoch after a clean audit")
		timeout              = flag.Duration("timeout", 10*time.Second, "single audit/apply timeout")
	)
	flag.Parse()

	services, err := dsauthfence.ParseExpectedServices(*expectedRaw)
	must(err)
	instances, err := dsauthfence.ParseExpectedInstances(*expectedInstancesRaw)
	must(err)
	if *keysetRevision == "" {
		must(fmt.Errorf("-keyset-revision is required"))
	}
	allowedDigests := make(map[string]struct{})
	for _, digest := range splitNonEmpty(*allowedDigestsRaw) {
		allowedDigests[digest] = struct{}{}
	}
	if len(allowedDigests) == 0 {
		must(fmt.Errorf("-allowed-image-digests is required"))
	}
	items := splitNonEmpty(*endpoints)
	if len(items) == 0 {
		must(fmt.Errorf("-endpoints is required"))
	}
	client, err := dsauthfence.NewActivationClient(items, *prefix, *timeout)
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
	findings := dsauthfence.AuditCapabilities(capabilities, dsauthfence.AuditPolicy{
		RequiredServices:  services,
		RequiredInstances: instances,
		TargetEpoch:       uint32(*target),
		KeysetRevision:    *keysetRevision,
		AllowedDigests:    allowedDigests,
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
