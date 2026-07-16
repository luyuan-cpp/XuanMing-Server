package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunPodUIDConfigCompareReturnsOnlyMachineSafeIdentity(t *testing.T) {
	writer := "node:\n  redis_client:\n    host: redis.internal:6379\n    password: writer-secret\n"
	readOnly := "node:\n  redis_client:\n    host: redis.internal:6379\n    maint_notifications: disabled\n"
	input, err := json.Marshal(podUIDConfigCompareInput{
		WriterConfigBase64:   base64.StdEncoding.EncodeToString([]byte(writer)),
		ReadOnlyConfigBase64: base64.StdEncoding.EncodeToString([]byte(readOnly)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runPodUIDConfigCompare(bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(output.String())
	if strings.Contains(line, "writer-secret") || strings.Contains(line, "redis.internal") {
		t.Fatalf("output leaked sensitive config: %s", line)
	}
	var decoded podUIDConfigCompareOutput
	if err := json.Unmarshal([]byte(line), &decoded); err != nil || !decoded.Matched ||
		!strings.HasPrefix(decoded.RedisConfigIdentity, "sha256:") || decoded.RedisTopology != "standalone" {
		t.Fatalf("output=%q decoded=%+v err=%v", line, decoded, err)
	}
}

func TestRunPodUIDConfigCompareFailsClosedWithoutEchoingInput(t *testing.T) {
	for _, input := range []string{
		`{"writer_config_base64":"not-base64","read_only_config_base64":"also-bad"}`,
		`{"writer_config_base64":"","read_only_config_base64":"","unexpected":true}`,
		`{} {}`,
	} {
		var output bytes.Buffer
		err := runPodUIDConfigCompare(strings.NewReader(input), &output)
		if err == nil || output.Len() != 0 || strings.Contains(err.Error(), input) {
			t.Fatalf("input=%q output=%q err=%v", input, output.String(), err)
		}
	}
}
