// dsauth-required 只读验证 required_writer_epoch 已由显式 genesis/bootstrap 流程建立。
// 它绝不创建、修复或回退 key，供 online 部署在任何 registry/k8s 写入前 fail-closed 预检。
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/luyuancpp/pandora/pkg/dsauthfence"
)

func main() {
	endpointsRaw := flag.String("endpoints", "", "etcd endpoints, comma separated")
	prefix := flag.String("prefix", dsauthfence.DefaultPrefix, "DS auth fence key prefix")
	minEpoch := flag.Uint64("min-epoch", 1, "minimum accepted required writer epoch")
	maxEpoch := flag.Uint64("max-epoch", uint64(dsauthfence.ProtocolEpochV2), "maximum accepted required writer epoch")
	timeout := flag.Duration("timeout", 10*time.Second, "linearizable read timeout")
	flag.Parse()

	endpoints := splitNonEmpty(*endpointsRaw)
	if len(endpoints) == 0 {
		fatal(fmt.Errorf("-endpoints is required"))
	}
	minAccepted, maxAccepted, err := checkedEpochRange(*minEpoch, *maxEpoch)
	if err != nil {
		fatal(err)
	}
	client, err := dsauthfence.NewActivationClient(endpoints, *prefix, *timeout)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	snapshot, err := client.RequiredSnapshot(ctx)
	if err != nil {
		fatal(fmt.Errorf("required writer epoch preflight failed: %w", err))
	}
	if err := validateRequiredEpoch(snapshot.Epoch, minAccepted, maxAccepted); err != nil {
		fatal(err)
	}
	fmt.Printf("required_writer_epoch 只读预检通过: epoch=%d mod_revision=%d\n", snapshot.Epoch, snapshot.ModRevision)
}

func checkedEpochRange(minEpoch, maxEpoch uint64) (uint32, uint32, error) {
	if minEpoch > math.MaxUint32 || maxEpoch > math.MaxUint32 {
		return 0, 0, fmt.Errorf("accepted epoch range exceeds uint32: %d..%d", minEpoch, maxEpoch)
	}
	minAccepted, maxAccepted := uint32(minEpoch), uint32(maxEpoch)
	if err := validateEpochRange(minAccepted, maxAccepted); err != nil {
		return 0, 0, err
	}
	return minAccepted, maxAccepted, nil
}

func validateEpochRange(minEpoch, maxEpoch uint32) error {
	if minEpoch == 0 || maxEpoch == 0 || minEpoch > maxEpoch {
		return fmt.Errorf("invalid accepted epoch range: %d..%d", minEpoch, maxEpoch)
	}
	return nil
}

func validateRequiredEpoch(epoch, minEpoch, maxEpoch uint32) error {
	if err := validateEpochRange(minEpoch, maxEpoch); err != nil {
		return err
	}
	if epoch < minEpoch || epoch > maxEpoch {
		return fmt.Errorf("required writer epoch %d outside accepted range %d..%d", epoch, minEpoch, maxEpoch)
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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
