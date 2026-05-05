package config

import (
	"os"
	"testing"
)

// TestLoadDefaults confirms envconfig supplies defaults when env is empty.
// DATABASE_URL is intentionally not required (shell mode).
func TestLoadDefaults(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CLUSTER_ID", "")
	os.Unsetenv("PORT")
	os.Unsetenv("DATABASE_URL")
	os.Unsetenv("CLUSTER_ID")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil (DATABASE_URL is optional)", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.DatabaseURL != "" {
		t.Errorf("DatabaseURL = %q, want empty (shell mode)", cfg.DatabaseURL)
	}
}

// TestLoadOverrides confirms env values override defaults.
func TestLoadOverrides(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("CLUSTER_ID", "gcp")
	t.Setenv("DATABASE_URL", "postgres://x")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != "9090" {
		t.Errorf("Port = %q, want 9090", cfg.Port)
	}
	if cfg.ClusterID != "gcp" {
		t.Errorf("ClusterID = %q, want gcp", cfg.ClusterID)
	}
	if cfg.DatabaseURL != "postgres://x" {
		t.Errorf("DatabaseURL = %q, want postgres://x", cfg.DatabaseURL)
	}
}
