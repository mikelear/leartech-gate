package middleware

import (
	"testing"

	"github.com/mikelear/leartech-go-common/pkg/auth"
)

// The go-common floor for this service is v1.3.3.
//
// WHY A TEST AND NOT JUST go.mod. A floor recorded only in go.mod walks back
// silently. A `go mod tidy` in a stale checkout, a merge resolved the wrong
// way, or a `replace` left behind after local debugging all downgrade it
// without failing anything, and nothing notices until the auth work reaches
// for GatedScopes and finds it missing. Not hypothetical: this repo sat on
// v1.2.0 for months while the shared Renovate preset carried an automerge rule
// for go-common that had never once fired, because the repo was absent from
// the cluster's Renovate repositories list.
//
// HOW IT HOLDS. The method expressions below resolve at COMPILE time, so this
// file does not build against v1.2.0, where ScopeSurface, GatedScopes and
// UngatedScopes do not exist. The protection survives even if the assertion in
// the test body is later deleted.
//
// WHAT IT DELIBERATELY DOES NOT DO. It does not test go-common's behaviour.
// That is go-common's job -- see pkg/auth/require_scope_test.go and
// scope_surface_test.go there -- and restating it here would drift from the
// real thing while looking like coverage.
var (
	_ = (*auth.Verifier).GatedScopes
	_ = (*auth.Verifier).UngatedScopes
	_ = (*auth.Verifier).ScopeSurface
)

func TestGoCommonFloorSupportsTheScopeSurface(t *testing.T) {
	// A zero surface agrees with itself: nothing declared, nothing published,
	// nothing gated. The assertion matters less than the compile above it --
	// if this file builds, the resolved go-common can express the
	// enforce/issue seam the auth roadmap needs.
	if !(auth.ScopeSurface{}).Empty() {
		t.Fatal("a zero ScopeSurface must report Empty; go-common's contract changed")
	}
}
