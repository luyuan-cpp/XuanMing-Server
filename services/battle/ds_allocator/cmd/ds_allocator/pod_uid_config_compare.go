package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/poduidpreflight"
)

var flagPodUIDReleasePreflightCompareConfigs bool

func init() {
	flag.BoolVar(&flagPodUIDReleasePreflightCompareConfigs,
		"pod-uid-release-preflight-compare-configs", false,
		"read two base64 YAML snapshots from stdin, compare safe Redis target identity and exit")
}

type podUIDConfigCompareInput struct {
	WriterConfigBase64   string `json:"writer_config_base64"`
	ReadOnlyConfigBase64 string `json:"read_only_config_base64"`
}

type podUIDConfigCompareOutput struct {
	Matched             bool   `json:"matched"`
	RedisConfigIdentity string `json:"redis_config_identity"`
	RedisTopology       string `json:"redis_topology"`
}

// runPodUIDConfigCompare never echoes input, decoded YAML, endpoints,
// usernames, passwords, or password hashes.  The activation controller can
// feed Kubernetes Secret data directly as base64 JSON without creating local
// plaintext files or reimplementing YAML normalization in PowerShell.
func runPodUIDConfigCompare(stdin io.Reader, stdout io.Writer) error {
	if stdin == nil || stdout == nil {
		return fmt.Errorf("pod_uid config comparison requires input and output")
	}
	decoder := json.NewDecoder(io.LimitReader(stdin, 8<<20))
	decoder.DisallowUnknownFields()
	var input podUIDConfigCompareInput
	if err := decoder.Decode(&input); err != nil {
		return fmt.Errorf("pod_uid config comparison input is invalid")
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("pod_uid config comparison input must contain exactly one JSON value")
	}
	writer, err := base64.StdEncoding.Strict().DecodeString(input.WriterConfigBase64)
	if err != nil || len(writer) == 0 {
		return fmt.Errorf("writer config snapshot is not canonical base64")
	}
	readOnly, err := base64.StdEncoding.Strict().DecodeString(input.ReadOnlyConfigBase64)
	if err != nil || len(readOnly) == 0 {
		return fmt.Errorf("read-only config snapshot is not canonical base64")
	}
	identity, err := poduidpreflight.CompareRedisConfigYAML(writer, readOnly)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(podUIDConfigCompareOutput{
		Matched: true, RedisConfigIdentity: identity.Digest, RedisTopology: identity.Topology,
	})
}
