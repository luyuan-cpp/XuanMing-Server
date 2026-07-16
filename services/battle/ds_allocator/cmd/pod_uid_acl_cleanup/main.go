// pod_uid_acl_cleanup is the post-CAS lifecycle controller for the temporary
// Model-B release-audit Redis identity. It receives only the already-audited
// credential-free Redis config on stdin. The distinct control-plane identity
// and password are read from caller-selected environment variables and are
// never printed, written to Kubernetes, or included in activation evidence.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/poduidpreflight"
)

const (
	targetACLUser       = poduidpreflight.CanonicalReadOnlyUsername
	cleanupMethod       = "DELUSER"
	minimumSecretLength = 32
)

var (
	envNamePattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,127}$`)
	aclUserPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	errTargetExists = errors.New("temporary Redis ACL user still exists")
)

type cleanupInput struct {
	ReadOnlyConfigBase64 string `json:"read_only_config_base64"`
	PreflightPassBase64  string `json:"preflight_password_base64"`
}

type controlCredentials struct {
	username string
	password string
}

type cleanupSummary struct {
	configIdentity string
	topology       string
	visitedNodes   int
}

func main() {
	adminUsernameEnv := flag.String("admin-username-env", "", "environment variable containing the distinct Redis control-plane username")
	adminPasswordEnv := flag.String("admin-password-env", "", "environment variable containing the distinct Redis control-plane password")
	runtimePasswordEnv := flag.String("runtime-password-env", "", "environment variable containing the default runtime Redis password for separation proof")
	expectedIdentity := flag.String("expected-config-identity", "", "exact non-sensitive Redis config identity already bound by activation evidence")
	expectedTopology := flag.String("expected-topology", "", "exact Redis topology already bound by activation evidence")
	timeout := flag.Duration("timeout", 45*time.Second, "hard cleanup and readback deadline")
	flag.Parse()

	if *timeout <= 0 || *timeout > 5*time.Minute || !digestPattern.MatchString(*expectedIdentity) ||
		(*expectedTopology != "standalone" && *expectedTopology != "sentinel" && *expectedTopology != "cluster") {
		fatal()
	}
	rc, preflightPassword, err := decodeCleanupInput(os.Stdin)
	if err != nil {
		fatal()
	}
	credentials, err := loadControlCredentials(
		*adminUsernameEnv, *adminPasswordEnv, *runtimePasswordEnv, preflightPassword)
	zero(preflightPassword)
	if err != nil {
		fatal()
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	summary, err := cleanupTemporaryACLUser(ctx, rc, credentials, *expectedIdentity, *expectedTopology)
	if err != nil {
		fatal()
	}
	fmt.Printf("pod_uid preflight ACL cleanup PASSED: method=%s target_user=%s redis_config_identity=%s redis_topology=%s visited_nodes=%d state=absent\n",
		cleanupMethod, targetACLUser, summary.configIdentity, summary.topology, summary.visitedNodes)
}

func fatal() {
	// Do not print wrapped Redis errors: they may contain control-plane identity
	// metadata or endpoint names. The activation controller emits the stable,
	// recoverable "epoch CAS complete; cleanup pending" operator message.
	fmt.Fprintln(os.Stderr, "ERROR: temporary Redis ACL cleanup failed closed")
	os.Exit(1)
}

func decodeCleanupInput(reader io.Reader) (config.RedisConf, []byte, error) {
	if reader == nil {
		return config.RedisConf{}, nil, errors.New("missing cleanup input")
	}
	decoder := json.NewDecoder(io.LimitReader(reader, 8<<20))
	decoder.DisallowUnknownFields()
	var input cleanupInput
	if err := decoder.Decode(&input); err != nil {
		return config.RedisConf{}, nil, errors.New("invalid cleanup input")
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return config.RedisConf{}, nil, errors.New("cleanup input must contain exactly one JSON value")
	}
	configBody, err := base64.StdEncoding.Strict().DecodeString(input.ReadOnlyConfigBase64)
	if err != nil || len(configBody) == 0 {
		return config.RedisConf{}, nil, errors.New("invalid read-only config encoding")
	}
	preflightPassword, err := base64.StdEncoding.Strict().DecodeString(input.PreflightPassBase64)
	if err != nil || len(preflightPassword) < minimumSecretLength || bytes.IndexByte(preflightPassword, 0) >= 0 {
		zero(configBody)
		zero(preflightPassword)
		return config.RedisConf{}, nil, errors.New("invalid preflight credential comparison input")
	}
	rc, err := poduidpreflight.ParseReadOnlyRedisConfigYAML(configBody)
	zero(configBody)
	if err != nil {
		zero(preflightPassword)
		return config.RedisConf{}, nil, errors.New("invalid read-only Redis config")
	}
	return rc, preflightPassword, nil
}

func loadControlCredentials(
	usernameEnv, passwordEnv, runtimePasswordEnv string,
	preflightPassword []byte,
) (controlCredentials, error) {
	if !envNamePattern.MatchString(usernameEnv) || !envNamePattern.MatchString(passwordEnv) ||
		!envNamePattern.MatchString(runtimePasswordEnv) || usernameEnv == passwordEnv ||
		usernameEnv == runtimePasswordEnv || passwordEnv == runtimePasswordEnv {
		return controlCredentials{}, errors.New("control credential environment names are not distinct and canonical")
	}
	username, usernameSet := os.LookupEnv(usernameEnv)
	password, passwordSet := os.LookupEnv(passwordEnv)
	runtimePassword, runtimeSet := os.LookupEnv(runtimePasswordEnv)
	if !usernameSet || !aclUserPattern.MatchString(username) || username == "default" || username == targetACLUser ||
		!passwordSet || len(password) < minimumSecretLength || strings.IndexByte(password, 0) >= 0 ||
		!runtimeSet || runtimePassword == "" {
		return controlCredentials{}, errors.New("control credential is missing or not a distinct identity")
	}
	if constantTimeEqual([]byte(password), []byte(runtimePassword)) ||
		constantTimeEqual([]byte(password), preflightPassword) {
		return controlCredentials{}, errors.New("control credential reuses a runtime or preflight password")
	}
	return controlCredentials{username: username, password: password}, nil
}

func constantTimeEqual(left, right []byte) bool {
	if len(left) != len(right) {
		// Keep a fixed cryptographic comparison in the unequal-length branch too;
		// the lengths of high-entropy operator secrets are not emitted anywhere.
		var a, b [32]byte
		copy(a[:], left)
		copy(b[:], right)
		_ = subtle.ConstantTimeCompare(a[:], b[:])
		return false
	}
	return subtle.ConstantTimeCompare(left, right) == 1
}

func zero(body []byte) {
	for index := range body {
		body[index] = 0
	}
}

func cleanupTemporaryACLUser(
	ctx context.Context,
	rc config.RedisConf,
	credentials controlCredentials,
	expectedIdentity, expectedTopology string,
) (cleanupSummary, error) {
	identity, err := poduidpreflight.IdentifyRedisConfig(rc)
	if err != nil || identity.Digest != expectedIdentity || identity.Topology != expectedTopology {
		return cleanupSummary{}, errors.New("Redis config identity differs from activation evidence")
	}
	var visited int
	switch identity.Topology {
	case "standalone":
		endpoints := effectiveSeedEndpoints(rc)
		if len(endpoints) != 1 {
			return cleanupSummary{}, errors.New("standalone cleanup requires one endpoint")
		}
		if err := cleanupDirectNodesTwice(ctx, rc, endpoints, credentials); err != nil {
			return cleanupSummary{}, err
		}
		visited = 1
	case "sentinel":
		first, err := discoverSentinelDataNodes(ctx, rc)
		if err != nil || len(first) == 0 {
			return cleanupSummary{}, errors.New("sentinel data-node discovery failed")
		}
		if err := cleanupDirectNodeSet(ctx, rc, first, credentials, true); err != nil {
			return cleanupSummary{}, err
		}
		second, err := discoverSentinelDataNodes(ctx, rc)
		if err != nil || !equalStringSets(first, second) {
			return cleanupSummary{}, errors.New("sentinel topology changed during cleanup")
		}
		if err := cleanupDirectNodeSet(ctx, rc, second, credentials, false); err != nil {
			return cleanupSummary{}, err
		}
		visited = len(second)
	case "cluster":
		first, err := cleanupClusterPass(ctx, rc, credentials, true)
		if err != nil || len(first) == 0 {
			return cleanupSummary{}, errors.New("cluster cleanup pass failed")
		}
		second, err := cleanupClusterPass(ctx, rc, credentials, false)
		if err != nil || !equalStringSets(first, second) {
			return cleanupSummary{}, errors.New("cluster topology changed during cleanup")
		}
		visited = len(second)
	default:
		return cleanupSummary{}, errors.New("unsupported Redis topology")
	}
	return cleanupSummary{configIdentity: identity.Digest, topology: identity.Topology, visitedNodes: visited}, nil
}

func effectiveSeedEndpoints(rc config.RedisConf) []string {
	if len(rc.Addrs) != 0 {
		return append([]string(nil), rc.Addrs...)
	}
	return []string{rc.Host}
}

func cleanupDirectNodesTwice(
	ctx context.Context,
	rc config.RedisConf,
	endpoints []string,
	credentials controlCredentials,
) error {
	set := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		set[endpoint] = struct{}{}
	}
	if len(set) != len(endpoints) {
		return errors.New("duplicate direct Redis endpoint")
	}
	if err := cleanupDirectNodeSet(ctx, rc, set, credentials, true); err != nil {
		return err
	}
	return cleanupDirectNodeSet(ctx, rc, set, credentials, false)
}

func cleanupDirectNodeSet(
	ctx context.Context,
	rc config.RedisConf,
	nodes map[string]struct{},
	credentials controlCredentials,
	deleteUser bool,
) error {
	ordered := make([]string, 0, len(nodes))
	for endpoint := range nodes {
		ordered = append(ordered, endpoint)
	}
	sort.Strings(ordered)
	for _, endpoint := range ordered {
		direct := rc
		direct.Host = endpoint
		direct.Addrs = nil
		direct.MasterName = ""
		client := redisx.NewUniversalClientWithCredentials(direct, credentials.username, credentials.password)
		if client == nil {
			return errors.New("Redis cleanup client construction failed")
		}
		err := cleanupNode(ctx, client, credentials.username, deleteUser)
		_ = client.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func cleanupClusterPass(
	ctx context.Context,
	rc config.RedisConf,
	credentials controlCredentials,
	deleteUser bool,
) (map[string]struct{}, error) {
	client := redisx.NewUniversalClientWithCredentials(rc, credentials.username, credentials.password)
	cluster, ok := client.(*redis.ClusterClient)
	if !ok {
		_ = client.Close()
		return nil, errors.New("configured cluster did not construct ClusterClient")
	}
	defer func() { _ = cluster.Close() }()
	seen := make(map[string]struct{})
	var mutex sync.Mutex
	err := cluster.ForEachShard(ctx, func(nodeCtx context.Context, node *redis.Client) error {
		if node == nil || node.Options() == nil || node.Options().Addr == "" {
			return errors.New("cluster returned an unidentified node")
		}
		if err := cleanupNode(nodeCtx, node, credentials.username, deleteUser); err != nil {
			return err
		}
		mutex.Lock()
		seen[node.Options().Addr] = struct{}{}
		mutex.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return seen, nil
}

func discoverSentinelDataNodes(ctx context.Context, rc config.RedisConf) (map[string]struct{}, error) {
	seeds := effectiveSeedEndpoints(rc)
	if rc.MasterName == "" || len(seeds) == 0 {
		return nil, errors.New("sentinel config is incomplete")
	}
	var canonical map[string]struct{}
	for _, seed := range seeds {
		sentinel := redis.NewSentinelClient(&redis.Options{
			Addr: seed, DialTimeout: rc.DialTimeout.Std(),
			ReadTimeout: rc.ReadTimeout.Std(), WriteTimeout: rc.WriteTimeout.Std(),
		})
		master, err := sentinel.GetMasterAddrByName(ctx, rc.MasterName).Result()
		if err != nil || len(master) != 2 {
			_ = sentinel.Close()
			return nil, errors.New("sentinel master discovery failed")
		}
		set := make(map[string]struct{})
		masterEndpoint, err := joinSentinelEndpoint(master[0], master[1])
		if err != nil {
			_ = sentinel.Close()
			return nil, errors.New("sentinel returned an invalid master endpoint")
		}
		set[masterEndpoint] = struct{}{}
		replicas, err := sentinel.Replicas(ctx, rc.MasterName).Result()
		_ = sentinel.Close()
		if err != nil {
			return nil, errors.New("sentinel replica discovery failed")
		}
		for _, replica := range replicas {
			flags := strings.Split(replica["flags"], ",")
			if containsString(flags, "s_down") || containsString(flags, "o_down") ||
				containsString(flags, "disconnected") {
				return nil, errors.New("sentinel reports an unavailable replica")
			}
			endpoint, err := joinSentinelEndpoint(replica["ip"], replica["port"])
			if err != nil {
				return nil, errors.New("sentinel returned an invalid replica endpoint")
			}
			set[endpoint] = struct{}{}
		}
		if canonical == nil {
			canonical = set
		} else if !equalStringSets(canonical, set) {
			return nil, errors.New("sentinels disagree on data-node membership")
		}
	}
	return canonical, nil
}

func joinSentinelEndpoint(host, portText string) (string, error) {
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 || strconv.FormatUint(port, 10) != portText || host == "" ||
		strings.ContainsAny(host, "\r\n\t ") {
		return "", errors.New("invalid Sentinel endpoint")
	}
	return net.JoinHostPort(host, portText), nil
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func equalStringSets(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for item := range left {
		if _, ok := right[item]; !ok {
			return false
		}
	}
	return true
}

type redisCommander interface {
	Do(context.Context, ...interface{}) *redis.Cmd
}

func cleanupNode(ctx context.Context, node redisCommander, adminUsername string, deleteUser bool) error {
	if node == nil || adminUsername == "" || adminUsername == "default" || adminUsername == targetACLUser {
		return errors.New("invalid cleanup node or control identity")
	}
	whoami, err := node.Do(ctx, "ACL", "WHOAMI").Text()
	if err != nil || whoami != adminUsername {
		return errors.New("Redis control identity proof failed")
	}
	if deleteUser {
		deleted, err := node.Do(ctx, "ACL", "DELUSER", targetACLUser).Int64()
		if err != nil || (deleted != 0 && deleted != 1) {
			return errors.New("Redis ACL DELUSER failed")
		}
	}
	value, err := node.Do(ctx, "ACL", "GETUSER", targetACLUser).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return errors.New("Redis ACL GETUSER readback failed")
	}
	if err == nil && value != nil {
		return errTargetExists
	}
	return nil
}
