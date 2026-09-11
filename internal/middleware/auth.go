// Package middleware provides HTTP middleware for the service.
package middleware

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/mikelear/leartech-go-common/pkg/auth"
)

// BearerAuth returns the go-common bearer middleware for this service's
// inbound API.
//
// VERIFIER, NOT SERVICECLIENT. leartech-gate only VALIDATES tokens — it spends
// none, and its outbound work is to GitHub with a PAT, not to a leartech peer.
// auth.Verifier is the constructor for that role and asks for Issuer +
// Audience alone. auth.NewServiceClient is the dual-role constructor, and its
// validateConfig also demands ClientID + ClientSecret, credentials this
// service has no use for. Building one here made an unused client identity
// load-bearing at startup, which is what took plan-api off the air on
// 2026-08-13.
//
// FAIL CLOSED, AND RETURN THE ERROR. NewVerifier errors unless BOTH Issuer and
// Audience are set; there is no noop, no pass-through, no runtime disable. The
// previous version called log.Fatal here, which is fail-closed but kills the
// process before /health can explain itself — returning the error lets the
// caller refuse to build the router and report why.
func BearerAuth(cfg auth.VerifierConfig, perms auth.Permissions) (gin.HandlerFunc, error) {
	verifier, err := auth.NewVerifier(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("bearer middleware: %w", err)
	}
	return auth.Middleware(verifier, perms), nil
}
