// Package middleware provides HTTP middleware for the service.
package middleware

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/mikelear/leartech-go-common/pkg/auth"
	"github.com/rs/zerolog/log"
)

// BearerAuth returns the leartech-go-common bearer middleware.
//
// Current behaviour (as of go-common 0.1.0): presence check on the
// Authorization header — full JWT validation lives in go-common and
// will tighten here when that lands (per golden-standard contract #5).
//
// If cfg.TokenURL is empty, returns a no-op middleware (local dev).
func BearerAuth(cfg auth.Config) gin.HandlerFunc {
	client, err := auth.NewServiceClient(context.Background(), cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("auth: failed to initialise service client")
	}
	return client.Middleware(nil)
}
