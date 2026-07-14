package auth

import "testing"

func TestDSCallbackDomainRequiresFixedIssuerAndAudience(t *testing.T) {
	valid := Config{
		Issuer: DSCallbackIssuer, Audience: DSCallbackAudience,
		Secret: []byte("ds-callback-domain-test-secret-32-bytes"),
	}
	if _, err := NewDSCallbackSigner(valid); err != nil {
		t.Fatalf("valid callback signer: %v", err)
	}
	if _, err := NewDSCallbackVerifier(valid); err != nil {
		t.Fatalf("valid callback verifier: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"player issuer":   func(c *Config) { c.Issuer = "pandora-login" },
		"player audience": func(c *Config) { c.Audience = "pandora-client" },
		"empty issuer":    func(c *Config) { c.Issuer = "" },
		"empty audience":  func(c *Config) { c.Audience = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if _, err := NewDSCallbackSigner(cfg); err == nil {
				t.Fatal("callback signer accepted another trust domain")
			}
			if _, err := NewDSCallbackVerifier(cfg); err == nil {
				t.Fatal("callback verifier accepted another trust domain")
			}
		})
	}
}
