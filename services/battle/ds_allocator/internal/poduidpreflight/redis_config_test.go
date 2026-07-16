package poduidpreflight

import (
	"strings"
	"testing"
	"time"
)

func TestCompareRedisConfigYAMLMatchesNormalizedTargetAndRejectsCredentials(t *testing.T) {
	writer := []byte(`node:
  redis_client:
    host: ignored-writer-host:6379
    addrs: [redis-b:6379, redis-a:6379]
    master_name: ""
    password: writer-secret-never-forwarded
    db: 0
`)
	readOnly := []byte(`node:
  redis_client:
    addrs:
      - redis-a:6379
      - redis-b:6379
    db: 0
    maint_notifications: disabled
`)
	identity, err := CompareRedisConfigYAML(writer, readOnly)
	if err != nil || identity.Topology != "cluster" || !ValidTargetIdentity(identity.Digest) {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}

	for name, body := range map[string][]byte{
		"inline password": []byte(strings.Replace(string(readOnly),
			"maint_notifications: disabled", "password: leaked\n    maint_notifications: disabled", 1)),
		"inline username": []byte(strings.Replace(string(readOnly),
			"maint_notifications: disabled", "username: writer\n    maint_notifications: disabled", 1)),
		"maintenance auto":       []byte(strings.Replace(string(readOnly), "disabled", "auto", 1)),
		"maintenance whitespace": []byte(strings.Replace(string(readOnly), "disabled", `" disabled "`, 1)),
		"non-zero db":            []byte(strings.Replace(string(readOnly), "db: 0", "db: 5", 1)),
		"different endpoint":     []byte(strings.Replace(string(readOnly), "redis-b:6379", "redis-c:6379", 1)),
		"duplicate endpoint":     []byte(strings.Replace(string(readOnly), "- redis-b:6379", "- redis-a:6379", 1)),
		"whitespace endpoint":    []byte(strings.Replace(string(readOnly), "redis-b:6379", `" redis-b:6379 "`, 1)),
		"uppercase endpoint":     []byte(strings.Replace(string(readOnly), "redis-b:6379", "REDIS-B:6379", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CompareRedisConfigYAML(writer, body); err == nil {
				t.Fatal("unsafe snapshot unexpectedly matched")
			}
		})
	}
}

func TestReadOnlyRedisConfigYAMLIsStrictSingleDocumentAndCredentialFree(t *testing.T) {
	canonical := `node:
  redis_client:
    host: redis.internal:6379
    db: 0
    dial_timeout: 2s
    read_timeout: 3s
    write_timeout: 4s
    maint_notifications: disabled
`
	rc, err := ParseReadOnlyRedisConfigYAML([]byte(canonical))
	if err != nil {
		t.Fatalf("canonical read-only YAML: %v", err)
	}
	if rc.Password != "" || rc.Host != "redis.internal:6379" ||
		rc.DialTimeout.Std() != 2*time.Second || rc.ReadTimeout.Std() != 3*time.Second ||
		rc.WriteTimeout.Std() != 4*time.Second {
		t.Fatalf("credential-free parsed config=%+v", rc)
	}
	for name, body := range map[string]string{
		"empty username":          strings.Replace(canonical, "    db: 0", "    username: \"\"\n    db: 0", 1),
		"empty password":          strings.Replace(canonical, "    db: 0", "    password: \"\"\n    db: 0", 1),
		"unknown redis field":     strings.Replace(canonical, "    db: 0", "    future_field: x\n    db: 0", 1),
		"unknown node field":      strings.Replace(canonical, "  redis_client:", "  future_node_field: x\n  redis_client:", 1),
		"unknown top-level field": "future_root: x\n" + canonical,
		"second document":         canonical + "---\nnode: {}\n",
		"empty trailing document": canonical + "---\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeReadOnlyRedisConfigYAML([]byte(body)); err == nil {
				t.Fatal("unsafe read-only YAML unexpectedly accepted")
			}
		})
	}
}

func TestCompareRedisConfigYAMLDoesNotReturnSensitiveValuesInErrors(t *testing.T) {
	const secret = "never-print-this-writer-password"
	writer := []byte("node:\n  redis_client:\n    host: redis:6379\n    password: " + secret + "\n")
	readOnly := []byte("node:\n  redis_client:\n    host: other:6379\n    maint_notifications: disabled\n")
	_, err := CompareRedisConfigYAML(writer, readOnly)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "redis:6379") {
		t.Fatalf("unsafe error=%q", err)
	}
}
