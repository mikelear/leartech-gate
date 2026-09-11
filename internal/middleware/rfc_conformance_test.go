package middleware

// Conformance matrix for inbound token validation.
//
//	go test -run TestRFC -v ./internal/middleware/
//
// lists every standard this service enforces on a bearer token, and every way
// it must refuse. The test names are the specification: read them instead of a
// document, and they cannot drift from the behaviour because they fail.
//
// Inherited from the go service template and kept deliberately: it is the
// executable statement of what this service's auth does, it needs no edit to
// keep working, and it is the difference between auth that is audited and auth
// that is assumed. If leartech-gate gains scopes, add the matching pair here.
//
// Each property has a PASS case and a REFUSE case differing by exactly one
// variable. A lone refusal proves nothing — everything refuses when the rig is
// misconfigured, so an unpaired refusal test is indistinguishable from a broken
// harness.

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/mikelear/leartech-go-common/pkg/auth"
)

const (
	thisService  = "leartech-gate"
	peerService  = "leartech-some-other-service"
	conformanceK = "conformance-key"
)

type rig struct {
	issuer string
	sign   func(t *testing.T, mutate func(jwt.MapClaims)) string
	router *gin.Engine
}

// newRig builds a real issuer, a JWKS endpoint the Verifier genuinely fetches,
// and a router carrying the same BearerAuth call cmd/server/router.go makes.
// The probe handler is a bare 200, so the status reports the AUTH decision and
// nothing else: 401 refused, 403 authenticated but not authorised, 200 admitted.
func newRig(t *testing.T, audience string, perms auth.Permissions) *rig {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	ks := jwkset.NewMemoryStorage()
	jwk, err := jwkset.NewJWKFromKey(priv, jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: conformanceK, USE: jwkset.UseSig},
	})
	if err != nil {
		t.Fatalf("build JWK: %v", err)
	}
	if err := ks.KeyWrite(t.Context(), jwk); err != nil {
		t.Fatalf("write JWK: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		raw, err := ks.JSONPublic(t.Context())
		if err != nil {
			t.Errorf("JSONPublic: %v", err)
			return
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	bearer, err := BearerAuth(auth.VerifierConfig{Issuer: srv.URL, Audience: audience}, perms)
	if err != nil {
		t.Fatalf("BearerAuth: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/probe", bearer, func(c *gin.Context) { c.Status(http.StatusOK) })

	return &rig{
		issuer: srv.URL,
		router: r,
		sign: func(t *testing.T, mutate func(jwt.MapClaims)) string {
			t.Helper()
			claims := jwt.MapClaims{
				"sub": "conformance-subject",
				"iss": srv.URL,
				"aud": []string{audience},
				"iat": time.Now().Unix(),
				"exp": time.Now().Add(5 * time.Minute).Unix(),
				"scp": []string{"leartechapi"},
				"ext": map[string]interface{}{"Permissions": []string{"User"}},
			}
			if mutate != nil {
				mutate(claims)
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
			tok.Header["kid"] = conformanceK
			s, err := tok.SignedString(priv)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			return s
		},
	}
}

func (rg *rig) status(t *testing.T, bearer string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	w := httptest.NewRecorder()
	rg.router.ServeHTTP(w, req)
	return w.Code
}

func (rg *rig) bearer(t *testing.T, mutate func(jwt.MapClaims)) int {
	t.Helper()
	return rg.status(t, "Bearer "+rg.sign(t, mutate))
}

func mustAccept(t *testing.T, got int) {
	t.Helper()
	if got != http.StatusOK {
		t.Fatalf("token should have been ACCEPTED, got HTTP %d", got)
	}
}

func mustRefuse(t *testing.T, got int) {
	t.Helper()
	if got == http.StatusOK {
		t.Fatal("token should have been REFUSED, got HTTP 200")
	}
}

// defaultRig mirrors router.go: this service's own audience, nil Permissions.
func defaultRig(t *testing.T) *rig { return newRig(t, thisService, nil) }

// --- RFC 7515: the signature must come from the issuer's published JWKS -----

func TestRFC7515_SignatureFromTheIssuerJWKS_IsAccepted(t *testing.T) {
	mustAccept(t, defaultRig(t).bearer(t, nil))
}

func TestRFC7515_SignatureFromAForeignKey_IsRefused(t *testing.T) {
	rg := defaultRig(t)
	_, foreign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate foreign key: %v", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"sub": "attacker",
		"iss": rg.issuer,
		"aud": []string{thisService},
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"scp": []string{"leartechapi"},
	})
	tok.Header["kid"] = conformanceK
	signed, err := tok.SignedString(foreign)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	mustRefuse(t, rg.status(t, "Bearer "+signed))
}

// --- RFC 7519 §4.1.1: issuer -------------------------------------------------

func TestRFC7519_4_1_1_MatchingIssuer_IsAccepted(t *testing.T) {
	mustAccept(t, defaultRig(t).bearer(t, nil))
}

func TestRFC7519_4_1_1_ForeignIssuer_IsRefused(t *testing.T) {
	mustRefuse(t, defaultRig(t).bearer(t, func(c jwt.MapClaims) {
		c["iss"] = "https://hydra.someone-elses-cluster.example.com"
	}))
}

func TestRFC7519_4_1_1_MissingIssuer_IsRefused(t *testing.T) {
	mustRefuse(t, defaultRig(t).bearer(t, func(c jwt.MapClaims) { delete(c, "iss") }))
}

// --- RFC 7519 §4.1.4: expiry -------------------------------------------------

func TestRFC7519_4_1_4_UnexpiredToken_IsAccepted(t *testing.T) {
	mustAccept(t, defaultRig(t).bearer(t, nil))
}

func TestRFC7519_4_1_4_ExpiredToken_IsRefused(t *testing.T) {
	mustRefuse(t, defaultRig(t).bearer(t, func(c jwt.MapClaims) {
		c["exp"] = time.Now().Add(-1 * time.Minute).Unix()
	}))
}

// --- RFC 8707: audience binding ---------------------------------------------

func TestRFC8707_MatchingAudience_IsAccepted(t *testing.T) {
	mustAccept(t, defaultRig(t).bearer(t, nil))
}

// The load-bearing one: a genuine, unexpired, correctly-signed token from OUR
// issuer, minted for a peer service. Nothing about it is forged.
func TestRFC8707_AudienceForAnotherService_IsRefused(t *testing.T) {
	mustRefuse(t, defaultRig(t).bearer(t, func(c jwt.MapClaims) {
		c["aud"] = []string{peerService}
	}))
}

// `aud: []` is the pre-audience-binding shape of a user token. It must not be
// read as "unrestricted".
func TestRFC8707_EmptyAudience_IsRefused(t *testing.T) {
	mustRefuse(t, defaultRig(t).bearer(t, func(c jwt.MapClaims) {
		c["aud"] = []string{}
	}))
}

func TestRFC8707_MissingAudienceClaim_IsRefused(t *testing.T) {
	mustRefuse(t, defaultRig(t).bearer(t, func(c jwt.MapClaims) { delete(c, "aud") }))
}

// `aud` is a set: a token legitimately bound to several resources is valid at
// each of them.
func TestRFC8707_MultiAudienceTokenContainingOurs_IsAccepted(t *testing.T) {
	mustAccept(t, defaultRig(t).bearer(t, func(c jwt.MapClaims) {
		c["aud"] = []string{peerService, thisService}
	}))
}

// Scope and audience are independent gates. Holding every scope does not make a
// foreign-audience token spendable here.
func TestRFC8707_CorrectScopeDoesNotSubstituteForAudience(t *testing.T) {
	mustRefuse(t, defaultRig(t).bearer(t, func(c jwt.MapClaims) {
		c["aud"] = []string{peerService}
		c["scp"] = []string{"leartechapi", "leartechapi.internal_services"}
	}))
}

// --- RFC 6750: how the token is presented -----------------------------------

func TestRFC6750_BearerHeader_IsAccepted(t *testing.T) {
	mustAccept(t, defaultRig(t).bearer(t, nil))
}

func TestRFC6750_NoAuthorizationHeader_IsRefused(t *testing.T) {
	mustRefuse(t, defaultRig(t).status(t, ""))
}

func TestRFC6750_MalformedToken_IsRefused(t *testing.T) {
	mustRefuse(t, defaultRig(t).status(t, "Bearer not.a.jwt"))
}

// The scheme is part of the contract; a bare token is not a credential.
func TestRFC6750_TokenWithoutBearerScheme_IsRefused(t *testing.T) {
	rg := defaultRig(t)
	mustRefuse(t, rg.status(t, rg.sign(t, nil)))
}

// --- RFC 6749 §3.3: scope ----------------------------------------------------

// THIS SERVICE PASSES nil PERMISSIONS, so it authenticates without authorising:
// any token from the right issuer for the right audience is admitted, whatever
// its scopes. That is a deliberate template default, not an oversight — and it
// is pinned here so a cloned service sees it rather than assuming a protection
// that is not applied.
//
// If your service has per-capability scopes, pass them in router.go and replace
// this pair with the matching accept/refuse for each.
func TestRFC6749_3_3_NilPermissions_AdmitsAnyScope_ByDesign(t *testing.T) {
	rg := defaultRig(t)

	mustAccept(t, rg.bearer(t, func(c jwt.MapClaims) {
		delete(c, "scp")
		delete(c, "ext")
	}))

	// The audience gate still applies — nil permissions relaxes AUTHORISATION,
	// never authentication.
	mustRefuse(t, rg.bearer(t, func(c jwt.MapClaims) {
		c["aud"] = []string{peerService}
		delete(c, "scp")
		delete(c, "ext")
	}))
}

// And the paired proof that a NON-nil permission genuinely gates, so the
// default above is a choice with a visible alternative rather than the only
// behaviour available.
func TestRFC6749_3_3_RequiredPermission_IsEnforcedWhenSet(t *testing.T) {
	rg := newRig(t, thisService, auth.Permissions{auth.PermAdmin})

	mustAccept(t, rg.bearer(t, func(c jwt.MapClaims) {
		c["ext"] = map[string]interface{}{"Permissions": []string{"User", "Admin"}}
	}))
	mustRefuse(t, rg.bearer(t, func(c jwt.MapClaims) {
		c["ext"] = map[string]interface{}{"Permissions": []string{"User"}}
	}))
}
