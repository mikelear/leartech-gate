// Package config loads 12-factor configuration via envconfig.
// Per golden-standard: no config files baked into the image.
package config

import (
	"fmt"
	"os"
	"strings"

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

	// Auth is the go-common VERIFIER config — inbound token validation only.
	//
	// leartech-gate is an OAuth2 resource server: it validates tokens and
	// spends none. Its only outbound work is to GitHub with a PAT, which is
	// not an OAuth2 client-credentials leg. The comment this replaces claimed
	// the config was used "for outbound auth to other leartech services",
	// which was never true here.
	//
	// NOT populated by envconfig, and the `-` tag is load-bearing. go-common
	// tags its config with `env:` (caarlos0), NOT `envconfig:`, so
	// `envconfig:"AUTH"` bound AUTH_SERVERURL / AUTH_CLIENTID / AUTH_AUDIENCE
	// — names derived from Go field names that match nothing any other service
	// in the estate renders and nothing go-common documents. Populated
	// explicitly in Load() from the estate-standard LEARTECH_AUTH_* names.
	Auth auth.VerifierConfig `envconfig:"-"`
}

// Load reads env and returns a populated Config.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return nil, fmt.Errorf("envconfig: %w", err)
	}
	c.Auth = loadAuthFromEnv()
	return &c, nil
}

// loadAuthFromEnv builds the inbound VerifierConfig from the estate-standard
// LEARTECH_AUTH_* names. Empty values pass through unchanged: NewVerifier is
// what decides whether the result is serviceable, and it refuses rather than
// degrading.
//
// LEARTECH_AUTH_ISSUER is what the value IS — an identity the token must claim,
// enforced under RFC 7519 4.1.1. Not a location you might be able to skip,
// which is how "server URL" ended up empty and failing open elsewhere.
func loadAuthFromEnv() auth.VerifierConfig {
	return auth.VerifierConfig{
		Issuer:   os.Getenv("LEARTECH_AUTH_ISSUER"),
		Audience: os.Getenv("LEARTECH_AUTH_AUDIENCE"),
	}
}

// inertAuthEnv lists LEARTECH_AUTH_* variables this service reads NO meaning
// from, so a set-but-ignored value is reported at boot rather than discarded.
// SERVER_URL is the one worth naming: it reads like the issuer knob and is the
// go-common TOKEN ENDPOINT, which a resource server never posts to.
var inertAuthEnv = []string{
	"LEARTECH_AUTH_CLIENT_ID",
	"LEARTECH_AUTH_CLIENT_SECRET",
	"LEARTECH_AUTH_TARGET_AUDIENCE",
	"LEARTECH_AUTH_SERVER_URL",
	"LEARTECH_AUTH_REQUIRED",
	"LEARTECH_AUTH_REQUIRED_SCOPES",
}

// InertAuthEnvSet returns the inert LEARTECH_AUTH_* variables actually present
// and non-empty. Never fatal — an inert value is untidy, not unsafe.
func InertAuthEnvSet() []string {
	var set []string
	for _, k := range inertAuthEnv {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			set = append(set, k)
		}
	}
	return set
}
