package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

const validReadOnlyConfig = `node:
  redis_client:
    host: 127.0.0.1:6379
    db: 0
    dial_timeout: 2s
    read_timeout: 2s
    write_timeout: 2s
    maint_notifications: disabled
`

func TestDecodeCleanupInputStrictAndCredentialFree(t *testing.T) {
	password := strings.Repeat("p", minimumSecretLength)
	body := fmt.Sprintf(`{"read_only_config_base64":%q,"preflight_password_base64":%q}`,
		base64.StdEncoding.EncodeToString([]byte(validReadOnlyConfig)),
		base64.StdEncoding.EncodeToString([]byte(password)))
	rc, decoded, err := decodeCleanupInput(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decodeCleanupInput() error = %v", err)
	}
	defer zero(decoded)
	if rc.Host != "127.0.0.1:6379" || string(decoded) != password {
		t.Fatalf("unexpected decoded input: host=%q passwordLength=%d", rc.Host, len(decoded))
	}

	for name, invalid := range map[string]string{
		"unknown JSON field": strings.TrimSuffix(body, "}") + `,"admin_password":"secret"}`,
		"trailing JSON":      body + `{}`,
		"credential in YAML": fmt.Sprintf(`{"read_only_config_base64":%q,"preflight_password_base64":%q}`,
			base64.StdEncoding.EncodeToString([]byte(validReadOnlyConfig+"    password: forbidden\n")),
			base64.StdEncoding.EncodeToString([]byte(password))),
	} {
		t.Run(name, func(t *testing.T) {
			if _, secret, err := decodeCleanupInput(strings.NewReader(invalid)); err == nil {
				zero(secret)
				t.Fatal("decodeCleanupInput() unexpectedly accepted invalid input")
			}
		})
	}
}

func TestLoadControlCredentialsRequiresThreeDistinctSecrets(t *testing.T) {
	const (
		usernameEnv = "TEST_ACL_ADMIN_USERNAME"
		passwordEnv = "TEST_ACL_ADMIN_PASSWORD"
		runtimeEnv  = "TEST_REDIS_RUNTIME_PASSWORD"
	)
	preflight := []byte(strings.Repeat("p", minimumSecretLength))
	t.Setenv(usernameEnv, "pandora-release-control")
	t.Setenv(passwordEnv, strings.Repeat("a", minimumSecretLength))
	t.Setenv(runtimeEnv, strings.Repeat("r", minimumSecretLength))
	credentials, err := loadControlCredentials(usernameEnv, passwordEnv, runtimeEnv, preflight)
	if err != nil {
		t.Fatalf("loadControlCredentials() error = %v", err)
	}
	if credentials.username != "pandora-release-control" || len(credentials.password) != minimumSecretLength {
		t.Fatal("unexpected control credentials")
	}

	t.Setenv(passwordEnv, string(preflight))
	if _, err := loadControlCredentials(usernameEnv, passwordEnv, runtimeEnv, preflight); err == nil {
		t.Fatal("preflight password reuse was accepted")
	}
	t.Setenv(passwordEnv, strings.Repeat("r", minimumSecretLength))
	if _, err := loadControlCredentials(usernameEnv, passwordEnv, runtimeEnv, preflight); err == nil {
		t.Fatal("runtime password reuse was accepted")
	}
	if _, err := loadControlCredentials(usernameEnv, passwordEnv, passwordEnv, preflight); err == nil {
		t.Fatal("duplicate credential environment names were accepted")
	}
}

type scriptedCommander struct {
	t        *testing.T
	commands [][]interface{}
	results  []scriptedResult
}

type scriptedResult struct {
	value interface{}
	err   error
}

func (s *scriptedCommander) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	s.t.Helper()
	s.commands = append(s.commands, append([]interface{}(nil), args...))
	if len(s.results) == 0 {
		s.t.Fatalf("unexpected Redis command: %#v", args)
	}
	result := s.results[0]
	s.results = s.results[1:]
	cmd := redis.NewCmd(ctx, args...)
	cmd.SetVal(result.value)
	cmd.SetErr(result.err)
	return cmd
}

func TestCleanupNodeDeletesOnlyFixedTemporaryUserAndReadsBack(t *testing.T) {
	commander := &scriptedCommander{t: t, results: []scriptedResult{
		{value: "pandora-release-control"},
		{value: int64(1)},
		{err: redis.Nil},
	}}
	if err := cleanupNode(context.Background(), commander, "pandora-release-control", true); err != nil {
		t.Fatalf("cleanupNode() error = %v", err)
	}
	want := [][]interface{}{
		{"ACL", "WHOAMI"},
		{"ACL", "DELUSER", targetACLUser},
		{"ACL", "GETUSER", targetACLUser},
	}
	if fmt.Sprint(commander.commands) != fmt.Sprint(want) {
		t.Fatalf("commands = %#v, want %#v", commander.commands, want)
	}
}

func TestCleanupNodeIsIdempotentAndFailsClosed(t *testing.T) {
	t.Run("already absent", func(t *testing.T) {
		commander := &scriptedCommander{t: t, results: []scriptedResult{
			{value: "pandora-release-control"}, {value: int64(0)}, {err: redis.Nil},
		}}
		if err := cleanupNode(context.Background(), commander, "pandora-release-control", true); err != nil {
			t.Fatalf("cleanupNode() error = %v", err)
		}
	})
	t.Run("wrong authenticated identity", func(t *testing.T) {
		commander := &scriptedCommander{t: t, results: []scriptedResult{{value: "default"}}}
		if err := cleanupNode(context.Background(), commander, "pandora-release-control", true); err == nil {
			t.Fatal("wrong ACL WHOAMI was accepted")
		}
		if len(commander.commands) != 1 {
			t.Fatalf("mutating command ran after identity mismatch: %#v", commander.commands)
		}
	})
	t.Run("user remains after delete", func(t *testing.T) {
		commander := &scriptedCommander{t: t, results: []scriptedResult{
			{value: "pandora-release-control"}, {value: int64(1)}, {value: []interface{}{"flags", []interface{}{"on"}}},
		}}
		if err := cleanupNode(context.Background(), commander, "pandora-release-control", true); !errors.Is(err, errTargetExists) {
			t.Fatalf("cleanupNode() error = %v, want errTargetExists", err)
		}
	})
}

func TestCleanupInputLimitDoesNotEchoSecrets(t *testing.T) {
	secret := strings.Repeat("do-not-log-me", 1024)
	_, decoded, err := decodeCleanupInput(bytes.NewBufferString(secret))
	zero(decoded)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("invalid input was accepted or secret was included in the error")
	}
}
