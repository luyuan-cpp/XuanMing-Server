package conf

import "testing"

func TestValidateLifecyclePublicationConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "redis authority requires broker",
			cfg: Config{
				Mode: ModeAgones,
			},
			wantErr: true,
		},
		{
			name: "redis authority accepts nonblank broker",
			cfg: Config{
				Mode: ModeAgones,
			},
		},
		{
			name: "agones enforce legacy also requires broker",
			cfg: Config{
				Mode: ModeAgones,
			},
			wantErr: true,
		},
		{
			name: "local off explicitly permits no lifecycle transport",
			cfg: Config{
				Mode: ModeLocal,
			},
		},
		{
			name: "mock off explicitly permits no lifecycle transport",
			cfg: Config{
				Mode: ModeMock,
			},
		},
	}

	// Fill fields after construction so each case remains easy to read.
	tests[0].cfg.DSAuth.AuthorityMode = "redis"
	tests[0].cfg.DSAuth.Mode = "enforce"
	tests[1].cfg.DSAuth.AuthorityMode = "redis"
	tests[1].cfg.DSAuth.Mode = "enforce"
	tests[1].cfg.Kafka.Brokers = []string{" kafka-1:9092 "}
	tests[2].cfg.DSAuth.AuthorityMode = "legacy"
	tests[2].cfg.DSAuth.Mode = "enforce"
	tests[2].cfg.Kafka.Brokers = []string{"", "  "}
	tests[3].cfg.DSAuth.AuthorityMode = "legacy"
	tests[3].cfg.DSAuth.Mode = "off"
	tests[4].cfg.DSAuth.AuthorityMode = "legacy"
	tests[4].cfg.DSAuth.Mode = "off"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.ValidateLifecyclePublicationConfig(); (err != nil) != tt.wantErr {
				t.Fatalf("ValidateLifecyclePublicationConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRequiresReliableLifecyclePublicationCaseInsensitive(t *testing.T) {
	cfg := Config{Mode: "AGONES"}
	cfg.DSAuth.AuthorityMode = "LEGACY"
	cfg.DSAuth.Mode = "Enforce"
	if !cfg.RequiresReliableLifecyclePublication() {
		t.Fatal("Agones enforce must require reliable lifecycle publication")
	}
}
