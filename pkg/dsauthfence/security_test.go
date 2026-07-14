package dsauthfence

import (
	"strings"
	"testing"
)

func secureTestConfig() ClientSecurity {
	return ClientSecurity{
		RequireMTLS:         true,
		CAFile:              "ca.pem",
		CertFile:            "client.pem",
		KeyFile:             "client-key.pem",
		ServerName:          "etcd.internal",
		ClientIdentity:      "pandora-dsauth-auditor",
		IdentityRevision:    "r7",
		RequireAuth:         true,
		ForbiddenReadPrefix: "/pandora/not-ds-auth/",
	}
}

func TestValidateClientSecurityRequiresMTLSCustomCAAuthAndNegativeACL(t *testing.T) {
	endpoints := []string{"https://etcd.internal:2379"}
	if err := validateClientSecurity(endpoints, DefaultPrefix, secureTestConfig()); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		edit func(*ClientSecurity)
		want string
	}{
		{name: "no_mtls", edit: func(s *ClientSecurity) { s.RequireMTLS = false }, want: "requires mTLS"},
		{name: "no_custom_ca", edit: func(s *ClientSecurity) { s.CAFile = "" }, want: "custom CA"},
		{name: "no_server_name", edit: func(s *ClientSecurity) { s.ServerName = "" }, want: "server name"},
		{name: "no_client_identity", edit: func(s *ClientSecurity) { s.ClientIdentity = "" }, want: "certificate identity"},
		{name: "bad_identity_revision", edit: func(s *ClientSecurity) { s.IdentityRevision = "7" }, want: "canonical rN"},
		{name: "auth_without_negative_acl", edit: func(s *ClientSecurity) { s.ForbiddenReadPrefix = "" }, want: "forbidden read prefix"},
		{name: "overlapping_acl", edit: func(s *ClientSecurity) { s.ForbiddenReadPrefix = "/pandora/ds-auth/capabilities/" }, want: "overlaps"},
		{name: "half_password_auth", edit: func(s *ClientSecurity) { s.UsernameFile = "username" }, want: "configured together"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := secureTestConfig()
			tc.edit(&config)
			if err := validateClientSecurity(endpoints, DefaultPrefix, config); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want substring %q", err, tc.want)
			}
		})
	}

	config := secureTestConfig()
	config.UsernameFile, config.PasswordFile = "username", "password"
	if err := validateClientSecurity(endpoints, DefaultPrefix, config); err != nil {
		t.Fatalf("optional username/password pair rejected: %v", err)
	}
	if err := validateClientSecurity([]string{"http://etcd.internal:2379"}, DefaultPrefix, config); err == nil {
		t.Fatal("plaintext endpoint accepted")
	}
	if err := validateClientSecurity([]string{"https://etcd.internal"}, DefaultPrefix, config); err == nil {
		t.Fatal("endpoint without explicit port accepted")
	}
	for _, endpoint := range []string{
		"https://etcd.internal:0",
		"https://etcd.internal:02379",
		"https://etcd.internal:65536",
		"https://etcd.internal:99999",
	} {
		if err := validateClientSecurity([]string{endpoint}, DefaultPrefix, config); err == nil {
			t.Fatalf("non-canonical/out-of-range endpoint accepted: %s", endpoint)
		}
	}
}

func TestClientSecurityFromEnvUsesStrictSwitches(t *testing.T) {
	for _, name := range []string{
		EnvEtcdRequireMTLS, EnvEtcdCAFile, EnvEtcdCertFile, EnvEtcdKeyFile,
		EnvEtcdServerName, EnvEtcdUsernameFile, EnvEtcdPasswordFile,
		EnvEtcdClientIdentity, EnvEtcdIdentityRevision,
		EnvEtcdRequireAuth, EnvEtcdForbiddenReadPrefix,
	} {
		t.Setenv(name, "")
	}
	t.Setenv(EnvEtcdRequireMTLS, "true")
	if _, err := ClientSecurityFromEnv(); err == nil {
		t.Fatal("non-canonical boolean accepted")
	}
	t.Setenv(EnvEtcdRequireMTLS, "1")
	t.Setenv(EnvEtcdRequireAuth, "1")
	t.Setenv(EnvEtcdCAFile, " /run/etcd/ca.crt ")
	security, err := ClientSecurityFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !security.RequireMTLS || !security.RequireAuth || security.CAFile != "/run/etcd/ca.crt" {
		t.Fatalf("unexpected environment mapping: %+v", security)
	}
}
