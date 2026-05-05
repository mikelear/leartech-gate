// Package config loads 12-factor configuration via envconfig.
// Per golden-standard: no config files baked into the image.
package config

import (
	"fmt"

	"github.com/kelseyhightower/envconfig"
	"github.com/mikelear/leartech-go-common/pkg/auth"
)

// Config is the envconfig-populated runtime configuration.
type Config struct {
	Port      string `envconfig:"PORT" default:"8080"`
	ClusterID string `envconfig:"CLUSTER_ID" default:""`

	// DatabaseURL is the postgres DSN. Format:
	// postgres://user:pass@host:port/dbname?sslmode=require
	//
	// Optional: if empty, the service runs in "shell mode" — no DB pool,
	// `/health/ready` returns ok without a DB ping. Used for the template's
	// own self-deploy to prove the chain end-to-end. Real services always
	// provide this via ExternalSecret.
	DatabaseURL string `envconfig:"DATABASE_URL"`

	// Auth is the go-common ServiceAuthClient config — used both for the
	// bearer middleware guarding our API and for outbound auth to other
	// leartech services.
	Auth auth.Config `envconfig:"AUTH"`
}

// Load reads env and returns a populated Config.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return nil, fmt.Errorf("envconfig: %w", err)
	}
	return &c, nil
}
