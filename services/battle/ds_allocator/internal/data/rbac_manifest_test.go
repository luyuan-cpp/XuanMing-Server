package data

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The allocation_uncertain reconciler uses DELETE on the GameServer
// collection with an allocation-id label selector. Kubernetes authorizes that
// as the distinct deletecollection verb, not delete.
func TestAllocatorGameServerRBACAllowsExactCollectionCleanup(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	manifest := filepath.Clean(filepath.Join(filepath.Dir(currentFile),
		"..", "..", "..", "..", "..", "deploy", "k8s", "agones", "10-rbac-allocator.yaml"))
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)resources:\s*\["gameservers"\]\s*\n\s*verbs:\s*\[([^]]+)\]`)
	match := re.FindStringSubmatch(string(body))
	if len(match) != 2 {
		t.Fatal("gameservers RBAC rule not found")
	}
	for _, required := range []string{"get", "list", "delete", "deletecollection", "patch"} {
		if !strings.Contains(match[1], `"`+required+`"`) {
			t.Fatalf("gameservers RBAC is missing %q: %s", required, match[1])
		}
	}
}

// Resolving the Pod UID is part of the allocator's durable exact-instance
// identity.  The shared allocator role needs only a named Pod GET; list/watch
// would unnecessarily broaden access to workload metadata.
func TestAllocatorPodRBACAllowsOnlyNamedGet(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	manifest := filepath.Clean(filepath.Join(filepath.Dir(currentFile),
		"..", "..", "..", "..", "..", "deploy", "k8s", "agones", "10-rbac-allocator.yaml"))
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)-\s*apiGroups:\s*\[""\]\s*\n\s*resources:\s*\["pods"\]\s*\n\s*verbs:\s*\[([^]]+)\]`)
	match := re.FindStringSubmatch(string(body))
	if len(match) != 2 {
		t.Fatal("core pods RBAC rule not found")
	}
	verbs := strings.FieldsFunc(match[1], func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '"'
	})
	if len(verbs) != 1 || verbs[0] != "get" {
		t.Fatalf("core pods RBAC must be the minimal named-get permission, got %q", match[1])
	}
}
