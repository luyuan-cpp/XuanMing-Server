package poduidpreflight

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/luyuancpp/pandora/pkg/config"
)

// RedisConfigIdentity binds only connection routing, never credentials or
// clear-text endpoints.  Activation can compare an immutable writer snapshot
// with a separately delivered read-only preflight snapshot without handing
// the writer password to the audit Job.
type RedisConfigIdentity struct {
	Digest   string `json:"redis_config_identity"`
	Topology string `json:"redis_topology"`
}

func IdentifyRedisConfig(rc config.RedisConf) (RedisConfigIdentity, error) {
	// Redis Cluster supports DB 0 only.  Requiring DB 0 uniformly also keeps
	// SELECT out of the dedicated ACL command set, so the release identity has
	// one topology-independent, mechanically reviewable allowlist.
	if rc.DB != 0 {
		return RedisConfigIdentity{}, fmt.Errorf("pod_uid preflight Redis target must use db=0")
	}
	endpoints, err := normalizedEffectiveEndpoints(rc)
	if err != nil {
		return RedisConfigIdentity{}, err
	}
	if rc.MasterName != strings.TrimSpace(rc.MasterName) ||
		strings.ContainsFunc(rc.MasterName, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) {
		return RedisConfigIdentity{}, fmt.Errorf("Redis sentinel master_name is non-canonical")
	}
	topology := configuredRedisTopology(rc)
	parts := []string{
		"pod-uid-release-preflight-config-v1",
		topology,
		rc.MasterName,
		fmt.Sprintf("%d", rc.DB),
		fmt.Sprintf("%d", len(endpoints)),
	}
	parts = append(parts, endpoints...)
	digest := lengthPrefixedSHA256(parts)
	return RedisConfigIdentity{
		Digest:   fmt.Sprintf("sha256:%x", digest[:]),
		Topology: topology,
	}, nil
}

func configuredRedisTopology(rc config.RedisConf) string {
	if rc.MasterName != "" {
		return "sentinel"
	}
	// Match redis.NewUniversalClient exactly: raw Addrs length, not a
	// normalized/deduplicated derivative, selects ClusterClient.
	if len(rc.Addrs) > 1 {
		return "cluster"
	}
	return "standalone"
}

type redisConfigDocument struct {
	Node struct {
		RedisClient redisConfigYAML `yaml:"redis_client"`
	} `yaml:"node"`
}

type redisConfigYAML struct {
	Host               string   `yaml:"host"`
	Addrs              []string `yaml:"addrs"`
	MasterName         string   `yaml:"master_name"`
	DB                 uint32   `yaml:"db"`
	Username           string   `yaml:"username"`
	Password           string   `yaml:"password"`
	MaintNotifications string   `yaml:"maint_notifications"`
}

func (c redisConfigYAML) redisConf() config.RedisConf {
	return config.RedisConf{
		Host: c.Host, Addrs: c.Addrs, MasterName: c.MasterName, DB: c.DB,
	}
}

// CompareRedisConfigYAML is the cross-snapshot mechanical gate.  It parses
// both YAML documents in-process, refuses credentials in the Job snapshot,
// and returns only the non-sensitive identity.  Callers must never log the
// input documents or YAML parse nodes.
func CompareRedisConfigYAML(
	writerYAML, readOnlyYAML []byte,
) (RedisConfigIdentity, error) {
	writer, err := decodeRedisConfigYAML(writerYAML)
	if err != nil {
		return RedisConfigIdentity{}, fmt.Errorf("writer Redis snapshot is invalid")
	}
	readOnly, err := ParseReadOnlyRedisConfigYAML(readOnlyYAML)
	if err != nil {
		return RedisConfigIdentity{}, fmt.Errorf("read-only Redis snapshot is invalid")
	}
	writerIdentity, err := IdentifyRedisConfig(writer.redisConf())
	if err != nil {
		return RedisConfigIdentity{}, fmt.Errorf("writer Redis target is invalid")
	}
	readOnlyIdentity, err := IdentifyRedisConfig(readOnly)
	if err != nil {
		return RedisConfigIdentity{}, fmt.Errorf("read-only Redis target is invalid")
	}
	if writerIdentity != readOnlyIdentity {
		return RedisConfigIdentity{}, fmt.Errorf(
			"writer and read-only Redis snapshots target different normalized identities")
	}
	return writerIdentity, nil
}

type readOnlyRedisConfigDocument struct {
	Node struct {
		RedisClient readOnlyRedisConfigYAML `yaml:"redis_client"`
	} `yaml:"node"`
}

// Deliberately omits username/password. KnownFields(true) therefore rejects
// those fields even when their YAML values are empty; credentials may arrive
// only through the two dedicated Secret-backed environment variables.
type readOnlyRedisConfigYAML struct {
	Host               string               `yaml:"host"`
	DB                 uint32               `yaml:"db"`
	DefaultTTL         strictConfigDuration `yaml:"default_ttl"`
	DialTimeout        strictConfigDuration `yaml:"dial_timeout"`
	ReadTimeout        strictConfigDuration `yaml:"read_timeout"`
	WriteTimeout       strictConfigDuration `yaml:"write_timeout"`
	Addrs              []string             `yaml:"addrs"`
	MasterName         string               `yaml:"master_name"`
	MaintNotifications string               `yaml:"maint_notifications"`
}

// config.Duration implements JSON decoding because production Kratos config
// converts YAML through JSON. This strict yaml.v3 path needs the equivalent
// scalar decoder explicitly so valid "2s" settings are not rejected.
type strictConfigDuration config.Duration

func (d *strictConfigDuration) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	var encoded []byte
	switch node.Tag {
	case "!!str":
		encoded = []byte(strconv.Quote(node.Value))
	case "!!int":
		encoded = []byte(node.Value)
	case "!!null":
		encoded = []byte("null")
	default:
		return fmt.Errorf("duration has unsupported YAML type")
	}
	parsed := config.Duration(*d)
	if err := parsed.UnmarshalJSON(encoded); err != nil {
		return err
	}
	*d = strictConfigDuration(parsed)
	return nil
}

func (c readOnlyRedisConfigYAML) redisConf() config.RedisConf {
	return config.RedisConf{
		Host: c.Host, Addrs: c.Addrs, MasterName: c.MasterName, DB: c.DB,
		DefaultTTL: config.Duration(c.DefaultTTL), DialTimeout: config.Duration(c.DialTimeout),
		ReadTimeout: config.Duration(c.ReadTimeout), WriteTimeout: config.Duration(c.WriteTimeout),
		MaintNotifications: c.MaintNotifications,
	}
}

func decodeReadOnlyRedisConfigYAML(body []byte) (redisConfigYAML, error) {
	rc, err := ParseReadOnlyRedisConfigYAML(body)
	if err != nil {
		return redisConfigYAML{}, err
	}
	return redisConfigYAML{
		Host: rc.Host, Addrs: rc.Addrs, MasterName: rc.MasterName, DB: rc.DB,
		MaintNotifications: rc.MaintNotifications,
	}, nil
}

// ParseReadOnlyRedisConfigYAML is the credential-free parser shared by the
// audit and post-CAS ACL cleanup commands. It accepts exactly one strict YAML
// document, rejects username/password fields even when empty, and returns a
// canonical DB-0 target with maint_notifications explicitly disabled.
func ParseReadOnlyRedisConfigYAML(body []byte) (config.RedisConf, error) {
	if len(body) == 0 {
		return config.RedisConf{}, fmt.Errorf("empty YAML")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	var document readOnlyRedisConfigDocument
	if err := decoder.Decode(&document); err != nil {
		return config.RedisConf{}, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return config.RedisConf{}, fmt.Errorf("multiple YAML documents")
		}
		return config.RedisConf{}, err
	}
	rc := document.Node.RedisClient.redisConf()
	if rc.MaintNotifications != "disabled" {
		return config.RedisConf{}, fmt.Errorf("maint_notifications must be disabled")
	}
	if _, err := IdentifyRedisConfig(rc); err != nil {
		return config.RedisConf{}, err
	}
	return rc, nil
}

func decodeRedisConfigYAML(body []byte) (redisConfigYAML, error) {
	if len(body) == 0 {
		return redisConfigYAML{}, fmt.Errorf("empty YAML")
	}
	var document redisConfigDocument
	if err := yaml.Unmarshal(body, &document); err != nil {
		return redisConfigYAML{}, err
	}
	return document.Node.RedisClient, nil
}
