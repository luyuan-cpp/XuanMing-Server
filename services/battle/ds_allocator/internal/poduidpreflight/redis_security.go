package poduidpreflight

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
)

const CanonicalReadOnlyUsername = "pandora-pod-uid-release-preflight-ro"

var (
	redisRuntimeIDPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	targetIdentityPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	redisPasswordHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// RedisTargetIdentity is safe to publish in activation logs.  The digest
// binds the normalized configured target and the runtime Redis identity, but
// contains neither credentials nor clear-text internal endpoints.
type RedisTargetIdentity struct {
	Digest          string
	Topology        string
	Nodes           int
	MasterSetDigest string
	TopologyDigest  string
}

func ValidTargetIdentity(value string) bool {
	return targetIdentityPattern.MatchString(value)
}

type redisCommander interface {
	Do(context.Context, ...interface{}) *redis.Cmd
}

// ProveReadOnlyAndIdentify verifies the authenticated ACL identity on every
// Redis master and returns a privacy-safe target digest. It never executes a
// write: exact ACL state comes from ACL GETUSER and the secondary semantic
// probes use ACL DRYRUN rather than executing their commands.
// Observation detects visible drift but cannot exclude Sentinel A->B->A or
// Redis 8.4 atomic slot migration. Activation must separately hold and recheck
// an externally enforced failover/reshard/migration lock from before prepare
// through the successful rollout CAS; an evidence digest is not such a lock.
func ProveReadOnlyAndIdentify(
	ctx context.Context,
	rdb redis.UniversalClient,
	rc config.RedisConf,
	expectedUsername string,
) (RedisTargetIdentity, error) {
	if ctx == nil || rdb == nil {
		return RedisTargetIdentity{}, fmt.Errorf("Redis ACL proof requires context and client")
	}
	if expectedUsername != CanonicalReadOnlyUsername {
		return RedisTargetIdentity{}, fmt.Errorf(
			"Redis ACL proof requires canonical read-only username %q", CanonicalReadOnlyUsername)
	}
	endpoints, err := normalizedEffectiveEndpoints(rc)
	if err != nil {
		return RedisTargetIdentity{}, err
	}
	configIdentity, err := IdentifyRedisConfig(rc)
	if err != nil {
		return RedisTargetIdentity{}, err
	}
	topology := configIdentity.Topology
	var runtimeIDs []string
	var topologyDigest string
	var snapshots []clusterTopologySnapshot
	if cluster, ok := rdb.(*redis.ClusterClient); ok {
		if topology != "cluster" {
			return RedisTargetIdentity{}, fmt.Errorf(
				"Redis client topology is cluster but normalized config topology is %s", topology)
		}
		var mu sync.Mutex
		err = cluster.ForEachMaster(ctx, func(nodeCtx context.Context, node *redis.Client) error {
			if err := provePasswordRequired(nodeCtx, node); err != nil {
				return err
			}
			if err := proveReadOnlyACL(nodeCtx, node, expectedUsername); err != nil {
				return err
			}
			id, err := clusterMasterID(nodeCtx, node)
			if err != nil {
				return err
			}
			snapshot, err := observeStableClusterTopology(nodeCtx, node, id)
			if err != nil {
				return err
			}
			mu.Lock()
			runtimeIDs = append(runtimeIDs, id)
			snapshots = append(snapshots, snapshot)
			mu.Unlock()
			return nil
		})
		if err != nil {
			return RedisTargetIdentity{}, fmt.Errorf("Redis cluster ACL/identity proof failed: %w", err)
		}
		if len(snapshots) == 0 {
			return RedisTargetIdentity{}, fmt.Errorf("Redis cluster topology proof visited zero slot-owning masters")
		}
		baseline := snapshots[0]
		for _, snapshot := range snapshots[1:] {
			if snapshot != baseline {
				return RedisTargetIdentity{}, fmt.Errorf("Redis masters disagree on cluster topology")
			}
		}
		topologyDigest = baseline.Digest
	} else {
		if topology == "cluster" {
			return RedisTargetIdentity{}, fmt.Errorf(
				"normalized Redis config requires cluster but client is not a cluster client")
		}
		single, ok := rdb.(*redis.Client)
		if !ok {
			return RedisTargetIdentity{}, fmt.Errorf("standalone/sentinel Redis client has unsupported type %T", rdb)
		}
		if err := provePasswordRequired(ctx, single); err != nil {
			return RedisTargetIdentity{}, err
		}
		if err := proveReadOnlyACL(ctx, rdb, expectedUsername); err != nil {
			return RedisTargetIdentity{}, fmt.Errorf("Redis ACL proof failed: %w", err)
		}
		if err := proveServerClusterDisabled(ctx, rdb); err != nil {
			return RedisTargetIdentity{}, fmt.Errorf("Redis non-cluster topology proof failed: %w", err)
		}
		if err := proveServerPrimary(ctx, rdb); err != nil {
			return RedisTargetIdentity{}, fmt.Errorf("Redis primary role proof failed: %w", err)
		}
		id, err := standaloneRuntimeID(ctx, rdb)
		if err != nil {
			return RedisTargetIdentity{}, fmt.Errorf("Redis runtime identity proof failed: %w", err)
		}
		runtimeIDs = append(runtimeIDs, id)
		topologyDigest = digestStrings("pod-uid-preflight-standalone-topology-v1", []string{id})
	}
	if len(runtimeIDs) == 0 {
		return RedisTargetIdentity{}, fmt.Errorf("Redis target identity visited zero masters")
	}
	sort.Strings(runtimeIDs)
	for i, id := range runtimeIDs {
		if !redisRuntimeIDPattern.MatchString(id) {
			return RedisTargetIdentity{}, fmt.Errorf("Redis returned a non-canonical runtime identity")
		}
		if i > 0 && runtimeIDs[i-1] == id {
			return RedisTargetIdentity{}, fmt.Errorf("Redis returned a duplicate master identity")
		}
	}
	masterSetDigest, err := runtimeMasterSetDigest(runtimeIDs)
	if err != nil {
		return RedisTargetIdentity{}, err
	}
	if topology == "cluster" {
		for _, snapshot := range snapshots {
			if snapshot.MasterSetDigest != masterSetDigest || snapshot.MasterCount != len(runtimeIDs) {
				return RedisTargetIdentity{}, fmt.Errorf(
					"Redis slot-owner callbacks do not exactly cover the topology master set")
			}
		}
	}

	parts := []string{
		"pod-uid-release-preflight-target-v1",
		configIdentity.Digest,
		topology,
		rc.MasterName,
		strconv.FormatUint(uint64(rc.DB), 10),
		strconv.Itoa(len(endpoints)),
	}
	parts = append(parts, endpoints...)
	parts = append(parts, strconv.Itoa(len(runtimeIDs)))
	parts = append(parts, runtimeIDs...)
	parts = append(parts, masterSetDigest, topologyDigest)
	digest := lengthPrefixedSHA256(parts)
	return RedisTargetIdentity{
		Digest:          "sha256:" + fmt.Sprintf("%x", digest[:]),
		Topology:        topology,
		Nodes:           len(runtimeIDs),
		MasterSetDigest: masterSetDigest,
		TopologyDigest:  topologyDigest,
	}, nil
}

func normalizedEffectiveEndpoints(rc config.RedisConf) ([]string, error) {
	raw := rc.Addrs
	if len(raw) == 0 {
		raw = []string{rc.Host}
	}
	unique := make(map[string]struct{}, len(raw))
	result := make([]string, 0, len(raw))
	for _, endpoint := range raw {
		if endpoint == "" || endpoint != strings.TrimSpace(endpoint) ||
			endpoint != strings.ToLower(endpoint) ||
			strings.ContainsFunc(endpoint, func(r rune) bool {
				return unicode.IsControl(r) || unicode.IsSpace(r)
			}) {
			return nil, fmt.Errorf("Redis target contains an empty or non-canonical endpoint")
		}
		canonical, err := canonicalRedisEndpoint(endpoint)
		if err != nil || canonical != endpoint {
			return nil, fmt.Errorf("Redis target contains an empty or non-canonical endpoint")
		}
		if _, duplicate := unique[endpoint]; duplicate {
			return nil, fmt.Errorf("Redis target contains a duplicate endpoint")
		}
		unique[endpoint] = struct{}{}
		result = append(result, endpoint)
	}
	if len(unique) == 0 {
		return nil, fmt.Errorf("Redis target contains no endpoints")
	}
	sort.Strings(result)
	return result, nil
}

func canonicalRedisEndpoint(endpoint string) (string, error) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || portText == "" {
		return "", fmt.Errorf("endpoint must be canonical host:port")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 || strconv.FormatUint(port, 10) != portText {
		return "", fmt.Errorf("endpoint has invalid port")
	}
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		host = address.String()
	} else {
		labels := strings.Split(host, ".")
		if len(host) > 253 {
			return "", fmt.Errorf("endpoint has invalid DNS host")
		}
		allNumeric := true
		for _, label := range labels {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", fmt.Errorf("endpoint has invalid DNS host")
			}
			for _, r := range label {
				if r < '0' || r > '9' {
					allNumeric = false
				}
			}
			for _, r := range label {
				if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
					return "", fmt.Errorf("endpoint has invalid DNS host")
				}
			}
		}
		if allNumeric {
			return "", fmt.Errorf("endpoint contains a non-canonical IP address")
		}
	}
	return net.JoinHostPort(host, portText), nil
}

func proveReadOnlyACL(ctx context.Context, node redisCommander, username string) error {
	whoami, err := node.Do(ctx, "ACL", "WHOAMI").Text()
	if err != nil {
		return fmt.Errorf("ACL WHOAMI is unavailable: %w", err)
	}
	if whoami != username {
		return fmt.Errorf("ACL WHOAMI=%q, want dedicated read-only identity %q", whoami, username)
	}
	value, err := node.Do(ctx, "ACL", "GETUSER", username).Result()
	if err != nil {
		return fmt.Errorf("ACL GETUSER is unavailable for the dedicated identity: %w", err)
	}
	user, err := parseACLGetUser(value)
	if err != nil {
		return fmt.Errorf("ACL GETUSER returned a non-canonical dedicated identity: %w", err)
	}
	if err := validateCanonicalReadOnlyACL(user); err != nil {
		return err
	}

	// ACL GETUSER is the complete proof above. DRYRUN calls are deliberately
	// only defence-in-depth semantic checks; wrong arity and a finite deny list
	// can never prove the absence of another grant.
	for _, command := range [][]string{
		{"GET", "pandora:ds:battle:{1}"},
		{"SCAN", "0", "MATCH", battleScanPattern, "COUNT", "1"},
	} {
		if err := requireACLDryRunAllowed(ctx, node, username, command); err != nil {
			return err
		}
	}
	if err := requireACLDryRunKeyDenied(ctx, node, username,
		[]string{"GET", "pandora:outside-preflight-trust-domain"}); err != nil {
		return err
	}
	for _, command := range [][]string{
		{"SET", "pandora:ds:battle:{1}", "forbidden"},
		{"DEL", "pandora:ds:battle:{1}"},
		{"EVAL", "return 1", "0"},
		{"CONFIG", "GET", "requirepass"},
	} {
		if err := requireACLDryRunDenied(ctx, node, username, command); err != nil {
			return err
		}
	}
	return nil
}

var canonicalReadOnlyACLCommands = []string{
	"-@all",
	"+ping", "+get", "+scan", "+info",
	"+acl|whoami", "+acl|dryrun", "+acl|getuser",
	"+cluster|myid", "+cluster|shards", "+cluster|slots",
	"+cluster|info", "+cluster|nodes",
}

type aclUserSnapshot struct {
	Flags         []string
	PasswordCount int
	Commands      []string
	Keys          string
	Channels      string
	SelectorCount int
}

// SECURITY BOUNDARY: ACL GETUSER is required because it is the only one of
// these mechanisms that proves the full flags/password-count/command/key/
// channel/selector state. The command can also disclose password hashes of
// every other user on this Redis instance, while SCAN discloses key names.
// Therefore this identity is activation-only and may be used only against a
// dedicated or same-trust Redis whose every password is high entropy. The
// activation controller must provision an immutable revisioned credential and
// immediately disable/delete this user after the successful rollout CAS. This
// process cannot perform that post-CAS cleanup and must not be treated as the
// lifecycle controller.
func parseACLGetUser(value interface{}) (aclUserSnapshot, error) {
	fields, err := aclUserFields(value)
	if err != nil {
		return aclUserSnapshot{}, err
	}
	if len(fields) != 6 {
		return aclUserSnapshot{}, fmt.Errorf("unexpected field set")
	}
	flags, err := aclStringList(fields["flags"])
	if err != nil {
		return aclUserSnapshot{}, fmt.Errorf("flags are malformed")
	}
	passwords, err := aclStringList(fields["passwords"])
	if err != nil {
		return aclUserSnapshot{}, fmt.Errorf("password metadata is malformed")
	}
	for _, hash := range passwords {
		if !redisPasswordHashPattern.MatchString(hash) {
			return aclUserSnapshot{}, fmt.Errorf("password metadata is non-canonical")
		}
	}
	commands, ok := fields["commands"].(string)
	if !ok || commands == "" || strings.TrimSpace(commands) != commands ||
		strings.Join(strings.Fields(commands), " ") != commands {
		return aclUserSnapshot{}, fmt.Errorf("commands are malformed")
	}
	commandTokens, err := canonicalACLTokenSet(strings.Fields(commands))
	if err != nil {
		return aclUserSnapshot{}, fmt.Errorf("commands are malformed")
	}
	keys, ok := fields["keys"].(string)
	if !ok {
		return aclUserSnapshot{}, fmt.Errorf("key rules are malformed")
	}
	channels, ok := fields["channels"].(string)
	if !ok {
		return aclUserSnapshot{}, fmt.Errorf("channel rules are malformed")
	}
	selectors, err := aclInterfaceList(fields["selectors"])
	if err != nil {
		return aclUserSnapshot{}, fmt.Errorf("selectors are malformed")
	}
	return aclUserSnapshot{
		Flags: flags, PasswordCount: len(passwords), Commands: commandTokens,
		Keys: keys, Channels: channels, SelectorCount: len(selectors),
	}, nil
}

func aclUserFields(value interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	add := func(rawKey, rawValue interface{}) error {
		key, ok := rawKey.(string)
		if !ok || key == "" || strings.ToLower(key) != key {
			return fmt.Errorf("non-canonical field name")
		}
		if _, duplicate := result[key]; duplicate {
			return fmt.Errorf("duplicate field")
		}
		result[key] = rawValue
		return nil
	}
	switch typed := value.(type) {
	case []interface{}:
		if len(typed)%2 != 0 {
			return nil, fmt.Errorf("odd field array")
		}
		for i := 0; i < len(typed); i += 2 {
			if err := add(typed[i], typed[i+1]); err != nil {
				return nil, err
			}
		}
	case map[string]interface{}:
		for key, field := range typed {
			if err := add(key, field); err != nil {
				return nil, err
			}
		}
	case map[interface{}]interface{}:
		for key, field := range typed {
			if err := add(key, field); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unexpected response shape")
	}
	for _, required := range []string{"flags", "passwords", "commands", "keys", "channels", "selectors"} {
		if _, ok := result[required]; !ok {
			return nil, fmt.Errorf("missing %s field", required)
		}
	}
	return result, nil
}

func aclStringList(value interface{}) ([]string, error) {
	items, err := aclInterfaceList(value)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(items))
	for i, item := range items {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("non-string list member")
		}
		result[i] = text
	}
	return result, nil
}

func aclInterfaceList(value interface{}) ([]interface{}, error) {
	switch typed := value.(type) {
	case []interface{}:
		return typed, nil
	case []string:
		result := make([]interface{}, len(typed))
		for i, item := range typed {
			result[i] = item
		}
		return result, nil
	default:
		return nil, fmt.Errorf("not a list")
	}
}

func canonicalACLTokenSet(tokens []string) ([]string, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty token set")
	}
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if token == "" || token != strings.ToLower(token) ||
			strings.ContainsFunc(token, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
			return nil, fmt.Errorf("non-canonical token")
		}
		if _, duplicate := seen[token]; duplicate {
			return nil, fmt.Errorf("duplicate token")
		}
		seen[token] = struct{}{}
	}
	result := append([]string(nil), tokens...)
	sort.Strings(result)
	return result, nil
}

func validateCanonicalReadOnlyACL(user aclUserSnapshot) error {
	wantFlags, _ := canonicalACLTokenSet([]string{"on", "sanitize-payload"})
	wantCommands, _ := canonicalACLTokenSet(canonicalReadOnlyACLCommands)
	gotFlags, err := canonicalACLTokenSet(user.Flags)
	if err != nil || !equalStrings(gotFlags, wantFlags) {
		return fmt.Errorf("dedicated Redis ACL flags differ from the exact activation-only contract")
	}
	if user.PasswordCount != 1 {
		return fmt.Errorf("dedicated Redis ACL must contain exactly one password hash")
	}
	if !equalStrings(user.Commands, wantCommands) {
		return fmt.Errorf("dedicated Redis ACL commands differ from the exact read-only allowlist")
	}
	if user.Keys != "%R~"+battleScanPattern {
		return fmt.Errorf("dedicated Redis ACL key rules differ from the exact read-only namespace")
	}
	if user.Channels != "" {
		return fmt.Errorf("dedicated Redis ACL channel rules must be empty")
	}
	if user.SelectorCount != 0 {
		return fmt.Errorf("dedicated Redis ACL selectors must be empty")
	}
	return nil
}

func requireACLDryRunAllowed(
	ctx context.Context,
	node redisCommander,
	username string,
	command []string,
) error {
	args := aclDryRunArgs(username, command)
	result, err := node.Do(ctx, args...).Text()
	if err != nil {
		return fmt.Errorf("ACL DRYRUN %s must be allowed: %w", command[0], err)
	}
	if result != "OK" {
		return fmt.Errorf("ACL DRYRUN %s returned %q, want OK", command[0], result)
	}
	return nil
}

func requireACLDryRunDenied(
	ctx context.Context,
	node redisCommander,
	username string,
	command []string,
) error {
	args := aclDryRunArgs(username, command)
	result, err := node.Do(ctx, args...).Text()
	message := strings.ToLower(result)
	if err != nil {
		message = strings.ToLower(err.Error())
	}
	name := strings.ToLower(command[0])
	permissionDenied := strings.Contains(message, "no permissions") || strings.Contains(message, "noperm")
	if permissionDenied && strings.Contains(message, name) {
		return nil
	}
	if err == nil && result == "OK" {
		return fmt.Errorf("ACL DRYRUN %s unexpectedly allowed a forbidden command", command[0])
	}
	return fmt.Errorf("ACL DRYRUN %s did not return an explicit command permission denial", command[0])
}

func requireACLDryRunKeyDenied(
	ctx context.Context,
	node redisCommander,
	username string,
	command []string,
) error {
	args := aclDryRunArgs(username, command)
	result, err := node.Do(ctx, args...).Text()
	message := strings.ToLower(result)
	if err != nil {
		message = strings.ToLower(err.Error())
	}
	if (strings.Contains(message, "no permissions") || strings.Contains(message, "noperm")) &&
		strings.Contains(message, "key") {
		return nil
	}
	if err == nil && result == "OK" {
		return fmt.Errorf("ACL DRYRUN GET unexpectedly allowed an out-of-namespace key")
	}
	return fmt.Errorf("ACL DRYRUN GET did not return an explicit key permission denial")
}

func aclDryRunArgs(username string, command []string) []interface{} {
	args := make([]interface{}, 0, 3+len(command))
	args = append(args, "ACL", "DRYRUN", username)
	for _, value := range command {
		args = append(args, value)
	}
	return args
}

func clusterMasterID(ctx context.Context, node redisCommander) (string, error) {
	id, err := node.Do(ctx, "CLUSTER", "MYID").Text()
	if err != nil {
		return "", fmt.Errorf("CLUSTER MYID is unavailable: %w", err)
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !redisRuntimeIDPattern.MatchString(id) {
		return "", fmt.Errorf("CLUSTER MYID returned a non-canonical node identity")
	}
	return id, nil
}

func provePasswordRequired(ctx context.Context, authenticated *redis.Client) error {
	if authenticated == nil || authenticated.Options() == nil {
		return fmt.Errorf("Redis password-required proof has no concrete client")
	}
	options := *authenticated.Options()
	options.Username = ""
	options.Password = ""
	options.CredentialsProvider = nil
	options.CredentialsProviderContext = nil
	options.StreamingCredentialsProvider = nil
	options.OnConnect = nil
	unauthenticated := redis.NewClient(&options)
	defer func() { _ = unauthenticated.Close() }()
	err := unauthenticated.Ping(ctx).Err()
	if err == nil {
		return fmt.Errorf("Redis accepted unauthenticated PING; dedicated password is not mandatory")
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "noauth") && !strings.Contains(message, "authentication required") {
		return fmt.Errorf("Redis unauthenticated PING did not return canonical authentication denial")
	}
	return nil
}

type clusterTopologySnapshot struct {
	Digest          string
	MasterSetDigest string
	MasterCount     int
}

type clusterInfoFence struct {
	CurrentEpoch uint64
	KnownNodes   int
	ClusterSize  int
	Digest       string
}

type clusterOwnershipView struct {
	Owners    []string
	MasterIDs []string
	NodeCount int
	SelfID    string
	Digest    string
}

// observeStableClusterTopology requires two identical local observations.
// The activation protocol must additionally hold an external Redis topology-
// change lock: Redis 8.4 atomic migration can hide keys from SCAN while its
// ownership table remains unchanged, and its STATUS subcommand cannot be
// granted without also granting destructive CLUSTER MIGRATION operations.
func observeStableClusterTopology(
	ctx context.Context,
	node *redis.Client,
	expectedSelfID string,
) (clusterTopologySnapshot, error) {
	first, err := observeClusterTopology(ctx, node, expectedSelfID)
	if err != nil {
		return clusterTopologySnapshot{}, err
	}
	second, err := observeClusterTopology(ctx, node, expectedSelfID)
	if err != nil {
		return clusterTopologySnapshot{}, err
	}
	if first != second {
		return clusterTopologySnapshot{}, fmt.Errorf("Redis cluster topology changed during observation")
	}
	return first, nil
}

func observeClusterTopology(
	ctx context.Context,
	node *redis.Client,
	expectedSelfID string,
) (clusterTopologySnapshot, error) {
	infoBody, err := node.ClusterInfo(ctx).Result()
	if err != nil {
		return clusterTopologySnapshot{}, fmt.Errorf("CLUSTER INFO is unavailable: %w", err)
	}
	info, err := parseClusterInfoFence(infoBody)
	if err != nil {
		return clusterTopologySnapshot{}, err
	}
	nodesBody, err := node.ClusterNodes(ctx).Result()
	if err != nil {
		return clusterTopologySnapshot{}, fmt.Errorf("CLUSTER NODES is unavailable: %w", err)
	}
	nodes, err := parseClusterNodesOwnership(nodesBody)
	if err != nil {
		return clusterTopologySnapshot{}, err
	}
	slotsBody, err := node.ClusterSlots(ctx).Result()
	if err != nil {
		return clusterTopologySnapshot{}, fmt.Errorf("CLUSTER SLOTS is unavailable: %w", err)
	}
	slots, err := parseClusterSlotsOwnership(slotsBody)
	if err != nil {
		return clusterTopologySnapshot{}, err
	}
	if info.ClusterSize != len(nodes.MasterIDs) || info.KnownNodes != nodes.NodeCount {
		return clusterTopologySnapshot{}, fmt.Errorf(
			"CLUSTER INFO size/count does not match CLUSTER NODES")
	}
	if nodes.SelfID != expectedSelfID {
		return clusterTopologySnapshot{}, fmt.Errorf(
			"CLUSTER MYID and CLUSTER NODES disagree on the connected master")
	}
	if !equalStrings(nodes.MasterIDs, slots.MasterIDs) ||
		!equalStrings(nodes.Owners, slots.Owners) {
		return clusterTopologySnapshot{}, fmt.Errorf(
			"CLUSTER NODES and CLUSTER SLOTS disagree on exact slot ownership")
	}
	masterSetDigest, err := runtimeMasterSetDigest(nodes.MasterIDs)
	if err != nil {
		return clusterTopologySnapshot{}, err
	}
	digest := digestStrings("pod-uid-preflight-cluster-topology-v1", []string{
		info.Digest, nodes.Digest, slots.Digest, masterSetDigest,
	})
	return clusterTopologySnapshot{
		Digest: digest, MasterSetDigest: masterSetDigest, MasterCount: len(nodes.MasterIDs),
	}, nil
}

func parseClusterInfoFence(body string) (clusterInfoFence, error) {
	fields, err := parseRedisInfoFields(body)
	if err != nil {
		return clusterInfoFence{}, fmt.Errorf("CLUSTER INFO is malformed: %w", err)
	}
	if fields["cluster_state"] != "ok" {
		return clusterInfoFence{}, fmt.Errorf("CLUSTER INFO state is not ok")
	}
	required := map[string]uint64{
		"cluster_slots_assigned": 16384,
		"cluster_slots_ok":       16384,
		"cluster_slots_pfail":    0,
		"cluster_slots_fail":     0,
	}
	for name, want := range required {
		got, err := parseStrictUintField(fields, name)
		if err != nil || got != want {
			return clusterInfoFence{}, fmt.Errorf("CLUSTER INFO %s is not canonical", name)
		}
	}
	currentEpoch, err := parseStrictUintField(fields, "cluster_current_epoch")
	if err != nil {
		return clusterInfoFence{}, fmt.Errorf("CLUSTER INFO current_epoch is invalid")
	}
	knownNodes, err := parseStrictPositiveIntField(fields, "cluster_known_nodes")
	if err != nil {
		return clusterInfoFence{}, err
	}
	clusterSize, err := parseStrictPositiveIntField(fields, "cluster_size")
	if err != nil {
		return clusterInfoFence{}, err
	}
	if knownNodes < clusterSize {
		return clusterInfoFence{}, fmt.Errorf("CLUSTER INFO known_nodes is smaller than cluster_size")
	}
	digest := digestStrings("pod-uid-preflight-cluster-info-v1", []string{
		strconv.FormatUint(currentEpoch, 10), strconv.Itoa(knownNodes), strconv.Itoa(clusterSize),
	})
	return clusterInfoFence{
		CurrentEpoch: currentEpoch, KnownNodes: knownNodes, ClusterSize: clusterSize, Digest: digest,
	}, nil
}

func parseRedisInfoFields(body string) (map[string]string, error) {
	fields := make(map[string]string)
	for _, rawLine := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line := rawLine
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.TrimSpace(line) != line {
			return nil, fmt.Errorf("non-canonical line whitespace")
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.TrimSpace(name) != name || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("non-canonical field")
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("duplicate field")
		}
		fields[name] = value
	}
	return fields, nil
}

func proveServerClusterDisabled(ctx context.Context, node redisCommander) error {
	body, err := node.Do(ctx, "INFO", "cluster").Text()
	if err != nil {
		return fmt.Errorf("INFO cluster is unavailable: %w", err)
	}
	return parseServerClusterDisabled(body)
}

func parseServerClusterDisabled(body string) error {
	fields, err := parseRedisInfoFields(body)
	if err != nil {
		return fmt.Errorf("INFO cluster is malformed: %w", err)
	}
	value, ok := fields["cluster_enabled"]
	if !ok || value != "0" {
		return fmt.Errorf("INFO cluster must prove cluster_enabled=0")
	}
	return nil
}

func proveServerPrimary(ctx context.Context, node redisCommander) error {
	body, err := node.Do(ctx, "INFO", "replication").Text()
	if err != nil {
		return fmt.Errorf("INFO replication is unavailable: %w", err)
	}
	return parseServerPrimary(body)
}

func parseServerPrimary(body string) error {
	fields, err := parseRedisInfoFields(body)
	if err != nil {
		return fmt.Errorf("INFO replication is malformed: %w", err)
	}
	role, ok := fields["role"]
	if !ok || role != "master" {
		return fmt.Errorf("INFO replication must prove role=master")
	}
	return nil
}

func parseStrictUintField(fields map[string]string, name string) (uint64, error) {
	value, ok := fields[name]
	if !ok || value == "" {
		return 0, fmt.Errorf("missing %s", name)
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(parsed, 10) != value {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return parsed, nil
}

func parseStrictPositiveIntField(fields map[string]string, name string) (int, error) {
	value, err := parseStrictUintField(fields, name)
	if err != nil || value == 0 || value > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("CLUSTER INFO %s is invalid", name)
	}
	return int(value), nil
}

func parseClusterNodesOwnership(body string) (clusterOwnershipView, error) {
	owners := make([]string, 16384)
	masterIDs := make([]string, 0)
	nodeParts := make([]string, 0)
	seenNodes := make(map[string]struct{})
	replicaParents := make([]string, 0)
	selfID := ""
	nodeCount := 0
	for _, rawLine := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains a short record")
		}
		id := strings.ToLower(fields[0])
		if fields[0] != id || !redisRuntimeIDPattern.MatchString(id) {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains a non-canonical node ID")
		}
		if _, duplicate := seenNodes[id]; duplicate {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains a duplicate node ID")
		}
		seenNodes[id] = struct{}{}
		nodeCount++
		flags := make(map[string]bool)
		for _, flag := range strings.Split(fields[2], ",") {
			if flag == "" || flag != strings.ToLower(flag) {
				return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains a non-canonical flag")
			}
			if _, duplicate := flags[flag]; duplicate {
				return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains a duplicate flag")
			}
			switch flag {
			case "myself", "master", "slave", "replica", "nofailover",
				"fail", "fail?", "handshake", "noaddr", "noflags":
			default:
				return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains an unknown node flag")
			}
			flags[flag] = true
		}
		for _, unsafe := range []string{"fail", "fail?", "handshake", "noaddr"} {
			if flags[unsafe] {
				return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains an unsafe node flag")
			}
		}
		if fields[7] != "connected" {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains a disconnected node")
		}
		if flags["myself"] {
			if selfID != "" {
				return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains multiple myself nodes")
			}
			selfID = id
		}
		master := flags["master"]
		replica := flags["slave"] || flags["replica"]
		if master == replica {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains an ambiguous node role")
		}
		configEpoch, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil || strconv.FormatUint(configEpoch, 10) != fields[6] {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains an invalid config epoch")
		}
		parent := fields[3]
		role := "master"
		if replica {
			role = "replica"
			parent = strings.ToLower(parent)
			if !redisRuntimeIDPattern.MatchString(parent) {
				return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES replica has invalid master identity")
			}
			replicaParents = append(replicaParents, parent)
		} else if parent != "-" {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES master has a parent identity")
		}
		nodeParts = append(nodeParts, digestStrings("pod-uid-preflight-cluster-node-v1", []string{
			id, role, parent, strconv.FormatUint(configEpoch, 10),
		}))
		slotCount := 0
		for _, token := range fields[8:] {
			if strings.HasPrefix(token, "[") || strings.Contains(token, "->-") || strings.Contains(token, "-<-") {
				return clusterOwnershipView{}, fmt.Errorf(
					"CLUSTER NODES reports an importing or migrating slot")
			}
			if replica {
				return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES replica unexpectedly owns slots")
			}
			start, end, err := parseClusterSlotToken(token)
			if err != nil {
				return clusterOwnershipView{}, err
			}
			for slot := start; slot <= end; slot++ {
				if owners[slot] != "" {
					return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES assigns a slot more than once")
				}
				owners[slot] = id
				slotCount++
			}
		}
		if master {
			if slotCount == 0 {
				return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains a zero-slot master")
			}
			masterIDs = append(masterIDs, id)
		}
	}
	if nodeCount == 0 || len(masterIDs) == 0 {
		return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES contains no usable masters")
	}
	if selfID == "" {
		return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES omitted the myself node")
	}
	masterSet := make(map[string]struct{}, len(masterIDs))
	for _, id := range masterIDs {
		masterSet[id] = struct{}{}
	}
	for _, parent := range replicaParents {
		if _, ok := masterSet[parent]; !ok {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES replica references an unknown master")
		}
	}
	if _, ok := masterSet[selfID]; !ok {
		return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES myself node is not a slot-owning master")
	}
	for slot, owner := range owners {
		if owner == "" {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER NODES leaves slot %d unassigned", slot)
		}
	}
	sort.Strings(masterIDs)
	sort.Strings(nodeParts)
	digestParts := append([]string{"nodes", strconv.Itoa(nodeCount)}, nodeParts...)
	digestParts = append(digestParts, compressSlotOwners(owners)...)
	return clusterOwnershipView{
		Owners: owners, MasterIDs: masterIDs, NodeCount: nodeCount, SelfID: selfID,
		Digest: digestStrings("pod-uid-preflight-cluster-nodes-v1", digestParts),
	}, nil
}

func parseClusterSlotsOwnership(slots []redis.ClusterSlot) (clusterOwnershipView, error) {
	if len(slots) == 0 {
		return clusterOwnershipView{}, fmt.Errorf("CLUSTER SLOTS returned no ownership ranges")
	}
	sorted := append([]redis.ClusterSlot(nil), slots...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	owners := make([]string, 16384)
	masters := make(map[string]struct{})
	next := 0
	for _, slotRange := range sorted {
		if slotRange.Start != next || slotRange.End < slotRange.Start || slotRange.End >= len(owners) ||
			len(slotRange.Nodes) == 0 {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER SLOTS is not an exact contiguous 0..16383 map")
		}
		id := strings.ToLower(slotRange.Nodes[0].ID)
		if slotRange.Nodes[0].ID != id || !redisRuntimeIDPattern.MatchString(id) {
			return clusterOwnershipView{}, fmt.Errorf("CLUSTER SLOTS contains a non-canonical master ID")
		}
		masters[id] = struct{}{}
		for slot := slotRange.Start; slot <= slotRange.End; slot++ {
			owners[slot] = id
		}
		next = slotRange.End + 1
	}
	if next != len(owners) {
		return clusterOwnershipView{}, fmt.Errorf("CLUSTER SLOTS does not cover all 16384 slots")
	}
	masterIDs := make([]string, 0, len(masters))
	for id := range masters {
		masterIDs = append(masterIDs, id)
	}
	sort.Strings(masterIDs)
	return clusterOwnershipView{
		Owners: owners, MasterIDs: masterIDs,
		Digest: digestStrings("pod-uid-preflight-cluster-slots-v1", compressSlotOwners(owners)),
	}, nil
}

func parseClusterSlotToken(token string) (int, int, error) {
	parts := strings.Split(token, "-")
	if len(parts) > 2 || len(parts) == 0 || parts[0] == "" {
		return 0, 0, fmt.Errorf("CLUSTER NODES contains an invalid slot token")
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil || strconv.Itoa(start) != parts[0] || start < 0 || start >= 16384 {
		return 0, 0, fmt.Errorf("CLUSTER NODES contains an invalid slot token")
	}
	end := start
	if len(parts) == 2 {
		end, err = strconv.Atoi(parts[1])
		if err != nil || strconv.Itoa(end) != parts[1] || end < start || end >= 16384 {
			return 0, 0, fmt.Errorf("CLUSTER NODES contains an invalid slot range")
		}
	}
	return start, end, nil
}

func compressSlotOwners(owners []string) []string {
	if len(owners) == 0 {
		return nil
	}
	result := make([]string, 0)
	start := 0
	for start < len(owners) {
		end := start
		for end+1 < len(owners) && owners[end+1] == owners[start] {
			end++
		}
		result = append(result, strconv.Itoa(start), strconv.Itoa(end), owners[start])
		start = end + 1
	}
	return result
}

func runtimeMasterSetDigest(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("Redis master identity set is empty")
	}
	canonical := append([]string(nil), ids...)
	sort.Strings(canonical)
	for i, id := range canonical {
		if !redisRuntimeIDPattern.MatchString(id) {
			return "", fmt.Errorf("Redis master identity set is non-canonical")
		}
		if i > 0 && canonical[i-1] == id {
			return "", fmt.Errorf("Redis master identity set contains a duplicate")
		}
	}
	return digestStrings("pod-uid-preflight-master-set-v1", canonical), nil
}

func digestStrings(prefix string, values []string) string {
	parts := append([]string{prefix}, values...)
	digest := lengthPrefixedSHA256(parts)
	return fmt.Sprintf("sha256:%x", digest[:])
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func standaloneRuntimeID(ctx context.Context, node redisCommander) (string, error) {
	info, err := node.Do(ctx, "INFO", "server").Text()
	if err != nil {
		return "", fmt.Errorf("INFO server is unavailable: %w", err)
	}
	for _, line := range strings.Split(strings.ReplaceAll(info, "\r\n", "\n"), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && key == "run_id" {
			value = strings.ToLower(strings.TrimSpace(value))
			if !redisRuntimeIDPattern.MatchString(value) {
				return "", fmt.Errorf("INFO server returned a non-canonical run_id")
			}
			return value, nil
		}
	}
	return "", fmt.Errorf("INFO server omitted run_id")
}

func lengthPrefixedSHA256(parts []string) [sha256.Size]byte {
	h := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(part))
	}
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}
