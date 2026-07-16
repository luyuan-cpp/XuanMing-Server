package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestPlayerLocatorDeploymentForbidsMixedVersionWriters(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	manifest := filepath.Clean(filepath.Join(filepath.Dir(currentFile),
		"..", "..", "..", "..", "..", "deploy", "k8s", "services", "services.yaml"))
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}

	var deployment string
	for _, document := range regexp.MustCompile("(?m)^---\\s*$").Split(string(body), -1) {
		if strings.Contains(document, "kind: Deployment") &&
			strings.Contains(document, "metadata: { name: player-locator,") {
			deployment = document
			break
		}
	}
	if deployment == "" {
		t.Fatal("player-locator Deployment not found")
	}
	if !regexp.MustCompile("(?m)^\\s{2}strategy:\\s*\\r?\\n\\s{4}type:\\s*Recreate\\s*$").
		MatchString(deployment) {
		t.Fatal("player-locator must use strategy.type=Recreate during the placement protocol rollout")
	}
	initStart := strings.Index(deployment, "\n      initContainers:\n")
	if initStart < 0 {
		t.Fatal("player-locator must mechanically run placement preflight as an initContainer")
	}
	containerRel := strings.Index(deployment[initStart+1:], "\n      containers:\n")
	if containerRel < 0 {
		t.Fatal("player-locator initContainer must precede the serving container")
	}
	initBlock := deployment[initStart : initStart+1+containerRel]
	if got := len(regexp.MustCompile(`(?m)^\s*-\s*name:\s*[^\s]+\s*$`).FindAllString(initBlock, -1)); got != 1 {
		t.Fatalf("player-locator must declare exactly one release-gate initContainer, got %d", got)
	}
	for _, required := range []string{
		"- name: placement-preflight",
		"image: pandora/player-locator:dev",
		"- \"-placement-preflight\"",
		"- \"-placement-preflight-timeout=10m\"",
		"- \"-placement-preflight-scan-count=1000\"",
		"subPath: player-locator.yaml",
		"readOnly: true",
	} {
		if !strings.Contains(initBlock, required) {
			t.Fatalf("player-locator preflight initContainer missing executable contract %q", required)
		}
	}
	if strings.Contains(initBlock, "command:") {
		t.Fatal("preflight must use the serving image ENTRYPOINT; an alternate command can reference a binary absent from the image")
	}
	servingBlock := deployment[initStart+1+containerRel:]
	if !regexp.MustCompile(`(?m)^\s*- name: player-locator\s*\r?\n\s*image: pandora/player-locator:dev\s*$`).MatchString(servingBlock) {
		t.Fatal("player-locator serving container and placement preflight must use the same kustomize image identity")
	}
}
