// dsauth-required 只读验证 required_writer_epoch 已由显式 genesis/bootstrap 流程建立。
// 它绝不创建、修复或回退 key，供 online 部署在任何 registry/k8s 写入前 fail-closed 预检。
package main

import (
	"context"
	"encoding/json"
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
	minPolicyGeneration := flag.Uint64("min-policy-generation", 1, "minimum accepted immutable required policy generation")
	maxPolicyGeneration := flag.Uint64("max-policy-generation", uint64(dsauthfence.RequiredPolicyGenerationV3), "maximum accepted immutable required policy generation")
	requireV3Record := flag.Bool("require-v3-activation-record", false, "require canonical same-transaction immutable V3 activation record/provenance")
	requireActivationEvidenceSHA256 := flag.String("require-activation-evidence-sha256", "", "require exact V3 activation evidence digest")
	requireActivationEvidenceCompletedAtMS := flag.Int64("require-activation-evidence-completed-at-ms", 0, "require exact V3 activation evidence completion Unix milliseconds")
	requireGenesisContinuityToken := flag.String("require-genesis-continuity-token", "", "require exact create-only local genesis data-volume continuity token")
	timeout := flag.Duration("timeout", 10*time.Second, "linearizable read timeout")
	output := flag.String("output", "text", "output format: text or json")
	requireMTLS := flag.Bool("require-mtls", false, "require custom-CA mutual TLS for etcd")
	caFile := flag.String("ca-file", "", "custom etcd CA PEM path")
	certFile := flag.String("cert-file", "", "etcd client certificate PEM path")
	keyFile := flag.String("key-file", "", "etcd client private-key PEM path")
	serverName := flag.String("server-name", "", "exact etcd TLS server name")
	clientIdentity := flag.String("client-identity", "", "exact etcd client certificate common name")
	etcdIdentityRevision := flag.String("etcd-identity-revision", "", "exact revisioned immutable etcd client identity")
	usernameFile := flag.String("username-file", "", "optional etcd username file path")
	passwordFile := flag.String("password-file", "", "optional etcd password file path")
	requireAuth := flag.Bool("require-auth", false, "require etcd v3 authentication to be enabled")
	forbiddenReadPrefix := flag.String("forbidden-read-prefix", "", "prefix this identity must be denied from reading")
	flag.Parse()
	if *output != "text" && *output != "json" {
		fatal(fmt.Errorf("-output must be text or json"))
	}
	if err := validateActivationEvidenceRequirement(*requireV3Record, *requireActivationEvidenceSHA256,
		*requireActivationEvidenceCompletedAtMS, *requireGenesisContinuityToken); err != nil {
		fatal(err)
	}

	endpoints := splitNonEmpty(*endpointsRaw)
	if len(endpoints) == 0 {
		fatal(fmt.Errorf("-endpoints is required"))
	}
	if *requireMTLS && *etcdIdentityRevision == "" {
		fatal(fmt.Errorf("-etcd-identity-revision is required with -require-mtls"))
	}
	minAccepted, maxAccepted, err := checkedEpochRange(*minEpoch, *maxEpoch)
	if err != nil {
		fatal(err)
	}
	minPolicy, maxPolicy, err := checkedPolicyGenerationRange(*minPolicyGeneration, *maxPolicyGeneration)
	if err != nil {
		fatal(err)
	}
	client, err := dsauthfence.NewActivationClientWithSecurity(endpoints, *prefix, *timeout, dsauthfence.ClientSecurity{
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
	if err := validateRequiredState(snapshot, minAccepted, maxAccepted, minPolicy, maxPolicy); err != nil {
		fatal(err)
	}
	if *requireV3Record {
		if snapshot.PolicyGeneration != dsauthfence.RequiredPolicyGenerationV3 {
			fatal(fmt.Errorf("V3 activation record cannot be required for policy generation %d", snapshot.PolicyGeneration))
		}
		if *requireActivationEvidenceSHA256 != "" {
			var evidenceErr error
			if *requireGenesisContinuityToken != "" {
				evidenceErr = client.VerifyRequiredPolicyV3ActivationEvidenceAndContinuity(ctx,
					*requireActivationEvidenceSHA256, *requireActivationEvidenceCompletedAtMS,
					*requireGenesisContinuityToken)
			} else {
				evidenceErr = client.VerifyRequiredPolicyV3ActivationEvidence(ctx,
					*requireActivationEvidenceSHA256, *requireActivationEvidenceCompletedAtMS)
			}
			if evidenceErr != nil {
				fatal(fmt.Errorf("required V3 exact activation evidence preflight failed: %w", evidenceErr))
			}
		} else if err := client.VerifyRequiredPolicyV3ActivationRecord(ctx); err != nil {
			fatal(fmt.Errorf("required V3 activation record preflight failed: %w", err))
		}
	}
	if *output == "json" {
		mustJSON(json.NewEncoder(os.Stdout).Encode(struct {
			Epoch            uint32 `json:"epoch"`
			PolicyGeneration uint32 `json:"policy_generation"`
			PolicyID         string `json:"policy_id"`
			RawValue         string `json:"raw_value"`
			ModRevision      int64  `json:"mod_revision"`
		}{Epoch: snapshot.Epoch, PolicyGeneration: snapshot.PolicyGeneration,
			PolicyID: snapshot.PolicyID, RawValue: snapshot.RawValue, ModRevision: snapshot.ModRevision}))
		return
	}
	fmt.Printf("required_writer_epoch 只读预检通过: epoch=%d policy_generation=%d policy_id=%q raw_value=%q mod_revision=%d\n",
		snapshot.Epoch, snapshot.PolicyGeneration, snapshot.PolicyID, snapshot.RawValue, snapshot.ModRevision)
}

func validateActivationEvidenceRequirement(requireV3Record bool, sha256 string, completedAtMS int64,
	genesisContinuityToken string) error {
	hasSHA := strings.TrimSpace(sha256) != ""
	hasTime := completedAtMS != 0
	if hasSHA != hasTime {
		return fmt.Errorf("exact activation evidence digest and completion time must be provided together")
	}
	if hasSHA && !requireV3Record {
		return fmt.Errorf("exact activation evidence requires -require-v3-activation-record")
	}
	if strings.TrimSpace(genesisContinuityToken) != "" {
		if !hasSHA {
			return fmt.Errorf("genesis continuity token requires exact V3 activation evidence")
		}
		if err := dsauthfence.ValidateGenesisContinuityToken(genesisContinuityToken); err != nil {
			return err
		}
	}
	if completedAtMS < 0 {
		return fmt.Errorf("exact activation evidence completion time must be positive")
	}
	return nil
}

func checkedPolicyGenerationRange(minGeneration, maxGeneration uint64) (uint32, uint32, error) {
	if minGeneration > math.MaxUint32 || maxGeneration > math.MaxUint32 {
		return 0, 0, fmt.Errorf("accepted policy generation range exceeds uint32: %d..%d", minGeneration, maxGeneration)
	}
	minAccepted, maxAccepted := uint32(minGeneration), uint32(maxGeneration)
	if minAccepted == 0 || maxAccepted == 0 || minAccepted > maxAccepted ||
		maxAccepted > dsauthfence.RequiredPolicyGenerationV3 {
		return 0, 0, fmt.Errorf("invalid accepted policy generation range: %d..%d", minAccepted, maxAccepted)
	}
	return minAccepted, maxAccepted, nil
}

func validateRequiredState(snapshot dsauthfence.RequiredSnapshot, minEpoch, maxEpoch,
	minPolicyGeneration, maxPolicyGeneration uint32) error {
	if err := validateRequiredEpoch(snapshot.Epoch, minEpoch, maxEpoch); err != nil {
		return err
	}
	if snapshot.PolicyGeneration < minPolicyGeneration || snapshot.PolicyGeneration > maxPolicyGeneration {
		return fmt.Errorf("required policy generation %d outside accepted range %d..%d",
			snapshot.PolicyGeneration, minPolicyGeneration, maxPolicyGeneration)
	}
	return nil
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

func mustJSON(err error) {
	if err != nil {
		fatal(fmt.Errorf("encode output: %w", err))
	}
}
