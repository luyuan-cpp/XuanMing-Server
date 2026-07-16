package poduidpreflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/redisx"
)

const testRedisRunID = "0123456789abcdef0123456789abcdef01234567"

func TestProveReadOnlyACLRequiresExactRedis8ActivationUser(t *testing.T) {
	node := canonicalACLCommander()
	if err := proveReadOnlyACL(context.Background(), node, CanonicalReadOnlyUsername); err != nil {
		t.Fatalf("proveReadOnlyACL: %v", err)
	}
	getUserCalls := 0
	for _, call := range node.calls {
		if len(call) == 3 && call[0] == "ACL" && call[1] == "GETUSER" {
			getUserCalls++
			if call[2] != CanonicalReadOnlyUsername {
				t.Fatalf("proof queried another user's password hash metadata: %v", call)
			}
		}
		if len(call) >= 2 && call[0] == "COMMAND" && call[1] == "LIST" {
			t.Fatalf("finite COMMAND LIST/DRYRUN proof unexpectedly used: calls=%v", node.calls)
		}
	}
	if getUserCalls != 1 {
		t.Fatalf("ACL GETUSER self proof calls=%d, want 1; calls=%v", getUserCalls, node.calls)
	}
}

func TestProveReadOnlyACLRejectsEveryCanonicalFieldDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*scriptedRedisCommander)
		want   string
	}{
		{name: "wrong whoami", mutate: func(n *scriptedRedisCommander) { n.whoami = "default" }, want: "WHOAMI"},
		{name: "off flag", mutate: func(n *scriptedRedisCommander) {
			n.getUser[1] = []interface{}{"off", "sanitize-payload"}
		}, want: "flags"},
		{name: "missing sanitize flag", mutate: func(n *scriptedRedisCommander) {
			n.getUser[1] = []interface{}{"on"}
		}, want: "flags"},
		{name: "multiple passwords", mutate: func(n *scriptedRedisCommander) {
			n.getUser[3] = []interface{}{strings.Repeat("a", 64), strings.Repeat("b", 64)}
		}, want: "exactly one"},
		{name: "malformed password hash", mutate: func(n *scriptedRedisCommander) {
			n.getUser[3] = []interface{}{"not-a-canonical-hash"}
		}, want: "password metadata"},
		{name: "extra command", mutate: func(n *scriptedRedisCommander) {
			n.getUser[5] = n.getUser[5].(string) + " +exists"
		}, want: "allowlist"},
		{name: "noncanonical command spacing", mutate: func(n *scriptedRedisCommander) {
			n.getUser[5] = strings.Replace(n.getUser[5].(string), " +ping", "  +ping", 1)
		}, want: "commands are malformed"},
		{name: "category grant", mutate: func(n *scriptedRedisCommander) {
			n.getUser[5] = strings.Replace(n.getUser[5].(string), "+get", "+@read", 1)
		}, want: "allowlist"},
		{name: "writable key rule", mutate: func(n *scriptedRedisCommander) {
			n.getUser[7] = "~" + battleScanPattern
		}, want: "key rules"},
		{name: "channel rule", mutate: func(n *scriptedRedisCommander) {
			n.getUser[9] = "&*"
		}, want: "channel rules"},
		{name: "selector", mutate: func(n *scriptedRedisCommander) {
			n.getUser[11] = []interface{}{[]interface{}{"commands", "+get"}}
		}, want: "selectors"},
		{name: "semantic write grant", mutate: func(n *scriptedRedisCommander) {
			n.allowedForbidden["set"] = true
		}, want: "unexpectedly allowed"},
		{name: "outside namespace get", mutate: func(n *scriptedRedisCommander) {
			n.allowOutsideGET = true
		}, want: "out-of-namespace"},
		{name: "dryrun unavailable", mutate: func(n *scriptedRedisCommander) {
			n.dryRunFailure = errors.New("ERR unknown subcommand 'DRYRUN'")
		}, want: "must be allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := canonicalACLCommander()
			tc.mutate(node)
			err := proveReadOnlyACL(context.Background(), node, CanonicalReadOnlyUsername)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseACLGetUserSupportsRESP2AndRESP3WithoutReturningHashes(t *testing.T) {
	resp2 := canonicalACLGetUser()
	want, err := parseACLGetUser(resp2)
	if err != nil || want.PasswordCount != 1 || len(want.Flags) != 2 ||
		!equalStrings(want.Commands, mustCanonicalACLCommands(t)) {
		t.Fatalf("snapshot=%+v err=%v", want, err)
	}
	resp3 := make(map[string]interface{})
	for i := 0; i < len(resp2); i += 2 {
		resp3[resp2[i].(string)] = resp2[i+1]
	}
	got, err := parseACLGetUser(resp3)
	if err != nil || !reflect.DeepEqual(want, got) {
		t.Fatalf("RESP3 snapshot=%+v err=%v", got, err)
	}
	// The returned proof contains only a count, never the password hash itself.
	if strings.Contains(fmt.Sprintf("%+v", got), strings.Repeat("a", 64)) {
		t.Fatal("ACL proof snapshot retained a password hash")
	}
}

func TestPasswordRequiredProofRejectsUnauthenticatedRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redisx.NewClient(config.RedisConf{Host: mr.Addr()})
	defer func() { _ = client.Close() }()
	err := provePasswordRequired(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "accepted unauthenticated PING") {
		t.Fatalf("err=%v", err)
	}
}

func mustCanonicalACLCommands(t *testing.T) []string {
	t.Helper()
	result, err := canonicalACLTokenSet(canonicalReadOnlyACLCommands)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRedisConfigAndRuntimeIdentityAreCanonicalAndInjective(t *testing.T) {
	a := config.RedisConf{Addrs: []string{"redis-b:6379", "redis-a:6379"}}
	b := config.RedisConf{Addrs: []string{"redis-a:6379", "redis-b:6379"}}
	aID, err := IdentifyRedisConfig(a)
	if err != nil {
		t.Fatal(err)
	}
	bID, err := IdentifyRedisConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if aID != bID || aID.Topology != "cluster" || !ValidTargetIdentity(aID.Digest) {
		t.Fatalf("a=%+v b=%+v", aID, bID)
	}

	for name, unsafe := range map[string]config.RedisConf{
		"duplicate":              {Addrs: []string{"redis-a:6379", "redis-a:6379"}},
		"outer whitespace":       {Host: " redis-a:6379 "},
		"uppercase":              {Host: "REDIS-A:6379"},
		"port leading zero":      {Host: "redis-a:06379"},
		"noncanonical ipv4":      {Host: "127.0.0.001:6379"},
		"numeric ipv4 shorthand": {Host: "2130706433:6379"},
		"sentinel whitespace":    {Host: "redis-a:6379", MasterName: " master "},
		"non-zero db":            {Host: "redis-a:6379", DB: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := IdentifyRedisConfig(unsafe); err == nil {
				t.Fatalf("unsafe config unexpectedly accepted: %+v", unsafe)
			}
		})
	}

	// Match go-redis raw topology selection: one Addrs element is standalone,
	// while two raw elements select cluster (duplicates are then rejected).
	one := config.RedisConf{Host: "ignored:6379", Addrs: []string{"redis-a:6379"}}
	oneID, err := IdentifyRedisConfig(one)
	if err != nil || oneID.Topology != "standalone" {
		t.Fatalf("one addrs identity=%+v err=%v", oneID, err)
	}

	node := canonicalACLCommander()
	node.info = "# Server\r\nredis_version:8.0.0\r\nrun_id:" + testRedisRunID + "\r\n"
	got, err := standaloneRuntimeID(context.Background(), node)
	if err != nil || got != testRedisRunID {
		t.Fatalf("runtime id=%q err=%v", got, err)
	}
	node.info = "# Server\nredis_version:8.0.0\n"
	if _, err := standaloneRuntimeID(context.Background(), node); err == nil {
		t.Fatal("missing run_id unexpectedly accepted")
	}
}

func TestClusterTopologyParsersRequireExactStableOwnership(t *testing.T) {
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	c := strings.Repeat("c", 40)
	d := strings.Repeat("d", 40)
	nodes := strings.Join([]string{
		a + " 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191",
		b + " 10.0.0.2:6379@16379 master - 0 0 2 connected 8192-16383",
		c + " 10.0.0.3:6379@16379 slave " + a + " 0 0 3 connected",
	}, "\n") + "\n"
	info := canonicalClusterInfo(7, 3, 2)

	infoView, err := parseClusterInfoFence(info)
	if err != nil || infoView.CurrentEpoch != 7 || infoView.KnownNodes != 3 || infoView.ClusterSize != 2 {
		t.Fatalf("info=%+v err=%v", infoView, err)
	}
	nodeView, err := parseClusterNodesOwnership(nodes)
	if err != nil || len(nodeView.MasterIDs) != 2 || nodeView.NodeCount != 3 || nodeView.SelfID != a ||
		nodeView.Owners[0] != a || nodeView.Owners[16383] != b {
		t.Fatalf("nodes=%+v err=%v", nodeView, err)
	}
	slotsView, err := parseClusterSlotsOwnership([]redis.ClusterSlot{
		{Start: 0, End: 8191, Nodes: []redis.ClusterNode{{ID: a}}},
		{Start: 8192, End: 16383, Nodes: []redis.ClusterNode{{ID: b}}},
	})
	if err != nil || !equalStrings(nodeView.MasterIDs, slotsView.MasterIDs) ||
		!equalStrings(nodeView.Owners, slotsView.Owners) {
		t.Fatalf("nodes/slots mismatch err=%v", err)
	}
	changedEpoch, err := parseClusterInfoFence(canonicalClusterInfo(8, 3, 2))
	if err != nil || changedEpoch.Digest == infoView.Digest {
		t.Fatalf("current_epoch drift not bound: before=%+v after=%+v err=%v", infoView, changedEpoch, err)
	}

	for name, body := range map[string]string{
		"zero slot master": strings.Replace(nodes, " connected 0-8191", " connected", 1),
		"failed master":    strings.Replace(nodes, "myself,master -", "myself,master,fail -", 1),
		"unknown flag":     strings.Replace(nodes, "myself,master -", "myself,master,future -", 1),
		"multiple myself":  strings.Replace(nodes, " master -", " myself,master -", 1),
		"missing myself":   strings.Replace(nodes, "myself,master", "master", 1),
		"disconnected":     strings.Replace(nodes, " connected 0-8191", " disconnected 0-8191", 1),
		"migrating":        strings.Replace(nodes, " 0-8191", " 0-8191 [1->-"+b+"]", 1),
		"slot gap":         strings.Replace(nodes, "8192-16383", "8193-16383", 1),
		"slot overlap":     strings.Replace(nodes, "8192-16383", "8000-16383", 1),
		"unknown parent":   strings.Replace(nodes, "slave "+a, "slave "+d, 1),
		"replica slots":    strings.Replace(nodes, " 3 connected\n", " 3 connected 9\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseClusterNodesOwnership(body); err == nil {
				t.Fatal("unsafe CLUSTER NODES response unexpectedly accepted")
			}
		})
	}

	for name, body := range map[string]string{
		"not ok":          strings.Replace(info, "cluster_state:ok", "cluster_state:fail", 1),
		"not 16384":       strings.Replace(info, "cluster_slots_ok:16384", "cluster_slots_ok:16383", 1),
		"failed slots":    strings.Replace(info, "cluster_slots_fail:0", "cluster_slots_fail:1", 1),
		"bad known nodes": strings.Replace(info, "cluster_known_nodes:3", "cluster_known_nodes:1", 1),
	} {
		t.Run("info_"+name, func(t *testing.T) {
			if _, err := parseClusterInfoFence(body); err == nil {
				t.Fatal("unsafe CLUSTER INFO response unexpectedly accepted")
			}
		})
	}

	for name, slots := range map[string][]redis.ClusterSlot{
		"gap": {
			{Start: 0, End: 8191, Nodes: []redis.ClusterNode{{ID: a}}},
			{Start: 8193, End: 16383, Nodes: []redis.ClusterNode{{ID: b}}},
		},
		"overlap": {
			{Start: 0, End: 8192, Nodes: []redis.ClusterNode{{ID: a}}},
			{Start: 8192, End: 16383, Nodes: []redis.ClusterNode{{ID: b}}},
		},
		"noncanonical id": {
			{Start: 0, End: 16383, Nodes: []redis.ClusterNode{{ID: strings.ToUpper(a)}}},
		},
	} {
		t.Run("slots_"+name, func(t *testing.T) {
			if _, err := parseClusterSlotsOwnership(slots); err == nil {
				t.Fatal("unsafe CLUSTER SLOTS response unexpectedly accepted")
			}
		})
	}
}

func TestNonClusterServerProofRequiresCanonicalClusterDisabled(t *testing.T) {
	if err := parseServerClusterDisabled("# Cluster\r\ncluster_enabled:0\r\n"); err != nil {
		t.Fatalf("canonical standalone INFO cluster: %v", err)
	}
	for name, body := range map[string]string{
		"cluster enabled": "# Cluster\r\ncluster_enabled:1\r\n",
		"missing":         "# Cluster\r\n",
		"malformed":       "# Cluster\r\ncluster_enabled\r\n",
		"noncanonical":    "# Cluster\r\ncluster_enabled:00\r\n",
		"duplicate":       "# Cluster\r\ncluster_enabled:0\r\ncluster_enabled:0\r\n",
		"whitespace":      "# Cluster\r\n cluster_enabled:0\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := parseServerClusterDisabled(body); err == nil {
				t.Fatal("unsafe INFO cluster response unexpectedly accepted")
			}
		})
	}
}

func TestNonClusterServerProofRequiresCanonicalPrimaryRole(t *testing.T) {
	if err := parseServerPrimary("# Replication\r\nrole:master\r\nconnected_slaves:0\r\n"); err != nil {
		t.Fatalf("canonical primary INFO replication: %v", err)
	}
	for name, body := range map[string]string{
		"replica":   "# Replication\r\nrole:slave\r\n",
		"missing":   "# Replication\r\nconnected_slaves:0\r\n",
		"malformed": "# Replication\r\nrole\r\n",
		"duplicate": "# Replication\r\nrole:master\r\nrole:master\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := parseServerPrimary(body); err == nil {
				t.Fatal("unsafe INFO replication response unexpectedly accepted")
			}
		})
	}
}

func canonicalClusterInfo(epoch uint64, knownNodes, clusterSize int) string {
	return fmt.Sprintf("cluster_state:ok\r\ncluster_slots_assigned:16384\r\n"+
		"cluster_slots_ok:16384\r\ncluster_slots_pfail:0\r\ncluster_slots_fail:0\r\n"+
		"cluster_known_nodes:%d\r\ncluster_size:%d\r\ncluster_current_epoch:%d\r\n",
		knownNodes, clusterSize, epoch)
}

func TestRedis8ReadOnlyACLIntegration(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("PANDORA_TEST_REDIS8_ADDR"))
	password := os.Getenv("PANDORA_TEST_REDIS8_PASSWORD")
	if addr == "" || password == "" {
		t.Skip("set PANDORA_TEST_REDIS8_ADDR/PASSWORD for real Redis 8 ACL integration")
	}
	rc := config.RedisConf{Host: addr, MaintNotifications: "disabled"}
	client := redisx.NewUniversalClientWithCredentials(
		rc, CanonicalReadOnlyUsername, password)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("PING with dedicated ACL identity: %v", err)
	}
	identity, err := ProveReadOnlyAndIdentify(
		context.Background(), client, rc, CanonicalReadOnlyUsername)
	if err != nil || identity.Topology != "standalone" || identity.Nodes != 1 ||
		!ValidTargetIdentity(identity.Digest) || !ValidTargetIdentity(identity.MasterSetDigest) ||
		!ValidTargetIdentity(identity.TopologyDigest) {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
}

func TestRedis8ClusterReadOnlyACLIntegration(t *testing.T) {
	rawAddrs := os.Getenv("PANDORA_TEST_REDIS8_CLUSTER_ADDRS")
	password := os.Getenv("PANDORA_TEST_REDIS8_CLUSTER_PASSWORD")
	if rawAddrs == "" || password == "" {
		t.Skip("set PANDORA_TEST_REDIS8_CLUSTER_ADDRS/PASSWORD for real Redis 8 cluster integration")
	}
	addrs := strings.Split(rawAddrs, ",")
	if len(addrs) < 2 {
		t.Fatal("cluster integration requires at least two seed addresses")
	}
	rc := config.RedisConf{Addrs: addrs, MaintNotifications: "disabled"}
	identity := proveAndAuditRedis8Integration(t, rc, password)
	if identity.Topology != "cluster" || identity.Nodes < 2 {
		t.Fatalf("cluster identity=%+v", identity)
	}
}

func TestRedis8SentinelReadOnlyACLIntegration(t *testing.T) {
	rawAddrs := os.Getenv("PANDORA_TEST_REDIS8_SENTINEL_ADDRS")
	masterName := os.Getenv("PANDORA_TEST_REDIS8_SENTINEL_MASTER_NAME")
	password := os.Getenv("PANDORA_TEST_REDIS8_SENTINEL_PASSWORD")
	if rawAddrs == "" || masterName == "" || password == "" {
		t.Skip("set Redis 8 sentinel integration addresses/master/password")
	}
	rc := config.RedisConf{
		Addrs: strings.Split(rawAddrs, ","), MasterName: masterName,
		MaintNotifications: "disabled",
	}
	identity := proveAndAuditRedis8Integration(t, rc, password)
	if identity.Topology != "sentinel" || identity.Nodes != 1 {
		t.Fatalf("sentinel identity=%+v", identity)
	}
}

func proveAndAuditRedis8Integration(
	t *testing.T,
	rc config.RedisConf,
	password string,
) RedisTargetIdentity {
	t.Helper()
	client := redisx.NewUniversalClientWithCredentials(
		rc, CanonicalReadOnlyUsername, password)
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("PING with dedicated ACL identity: %v", err)
	}
	before, err := ProveReadOnlyAndIdentify(ctx, client, rc, CanonicalReadOnlyUsername)
	if err != nil {
		t.Fatalf("pre-audit identity: %v", err)
	}
	summary := new(AuditSummary)
	if err := AuditRedis(ctx, client, 100, summary); err != nil {
		t.Fatalf("read-only audit: %v", err)
	}
	scanDigest, err := summary.RuntimeMasterSetDigest()
	if err != nil || summary.MastersVisited != before.Nodes || scanDigest != before.MasterSetDigest {
		t.Fatalf("identity=%+v summary=%+v scan_digest=%q err=%v",
			before, summary, scanDigest, err)
	}
	after, err := ProveReadOnlyAndIdentify(ctx, client, rc, CanonicalReadOnlyUsername)
	if err != nil || after != before {
		t.Fatalf("identity changed: before=%+v after=%+v err=%v", before, after, err)
	}
	return before
}

type scriptedRedisCommander struct {
	whoami           string
	info             string
	getUser          []interface{}
	allowedForbidden map[string]bool
	calls            [][]string
	allowOutsideGET  bool
	dryRunFailure    error
}

func canonicalACLCommander() *scriptedRedisCommander {
	return &scriptedRedisCommander{
		whoami:           CanonicalReadOnlyUsername,
		info:             "# Server\r\nrun_id:" + testRedisRunID + "\r\n",
		getUser:          canonicalACLGetUser(),
		allowedForbidden: make(map[string]bool),
	}
}

func canonicalACLGetUser() []interface{} {
	return []interface{}{
		"flags", []interface{}{"on", "sanitize-payload"},
		"passwords", []interface{}{strings.Repeat("a", 64)},
		"commands", strings.Join(canonicalReadOnlyACLCommands, " "),
		"keys", "%R~" + battleScanPattern,
		"channels", "",
		"selectors", []interface{}{},
	}
}

func (s *scriptedRedisCommander) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	cmd := redis.NewCmd(ctx, args...)
	words := make([]string, len(args))
	for i, arg := range args {
		words[i] = fmt.Sprint(arg)
	}
	s.calls = append(s.calls, words)
	if len(words) == 2 && words[0] == "ACL" && words[1] == "WHOAMI" {
		cmd.SetVal(s.whoami)
		return cmd
	}
	if len(words) == 3 && words[0] == "ACL" && words[1] == "GETUSER" {
		cmd.SetVal(s.getUser)
		return cmd
	}
	if len(words) >= 4 && words[0] == "ACL" && words[1] == "DRYRUN" {
		if s.dryRunFailure != nil {
			cmd.SetErr(s.dryRunFailure)
			return cmd
		}
		name := strings.ToLower(words[3])
		if name == "get" && len(words) >= 5 &&
			words[4] == "pandora:outside-preflight-trust-domain" && !s.allowOutsideGET {
			cmd.SetErr(errors.New("NOPERM this user has no permissions to access one of the keys"))
			return cmd
		}
		if name == "get" || name == "scan" || s.allowedForbidden[name] {
			cmd.SetVal("OK")
		} else {
			cmd.SetErr(fmt.Errorf("NOPERM this user has no permissions to run the '%s' command", name))
		}
		return cmd
	}
	if len(words) == 2 && words[0] == "INFO" && words[1] == "server" {
		cmd.SetVal(s.info)
		return cmd
	}
	cmd.SetErr(fmt.Errorf("unexpected test command %v", words))
	return cmd
}
