package main

import (
	"strings"
	"testing"
)

func TestRenderIssueBody_PassThroughVerdict(t *testing.T) {
	c := &IssueClient{Owner: "mikelear", GitOpsRepo: "mikelear/jx-build-cluster-gsm", GitOpsPullNo: "291"}
	v := ServiceVerdict{
		Service: "leartech-auth-ui", Version: "0.0.36",
		Pass:        false,
		Reason:      "shift-left: no required-tests entry; post-deploy: Arrival.phase=Failed",
		FailedTests: []string{"end2end-ui (Failed)"},
		FailedPacks: []string{"end2end-ui"},
	}
	body := c.renderIssueBody(v)

	if !strings.Contains(body, gateBodyMarker) {
		t.Error("body should contain marker for idempotency detection")
	}
	if !strings.Contains(body, "leartech-auth-ui@0.0.36") {
		t.Error("body should reference service@version")
	}
	if !strings.Contains(body, "Arrival.phase=Failed") {
		t.Error("body should include verdict reason")
	}
	if !strings.Contains(body, "end2end-ui (Failed)") {
		t.Error("body should list failed tests")
	}
	if !strings.Contains(body, "https://github.com/mikelear/jx-build-cluster-gsm/pull/291") {
		t.Error("body should back-link to GitOps PR")
	}
}

// TestRenderIssueBody_ArtifactLinks locks the contract that issue
// bodies carry per-failed-pack links to the Playwright HTML report +
// GCS listing when bucket+pathTemplate are configured. Without them
// (e.g. dry-run / unit tests with default IssueClient), no Artifacts
// section renders.
func TestRenderIssueBody_ArtifactLinks(t *testing.T) {
	c := &IssueClient{
		Owner: "mikelear", GitOpsRepo: "mikelear/jx-build-cluster-gsm", GitOpsPullNo: "291",
		Bucket:       "test-artifacts-product-first",
		PathTemplate: "results/v1/post-deploy/{{.Cluster}}/{{.Namespace}}/{{.Service}}/{{.Version}}/{{.Pack}}",
		Cluster:      "gcp",
		Namespace:    "jx-staging",
	}
	v := ServiceVerdict{
		Service: "leartech-auth-ui", Version: "0.0.36",
		Pass:        false,
		Reason:      "Arrival.phase=Failed",
		FailedTests: []string{"end2end-ui (Failed)"},
		FailedPacks: []string{"end2end-ui"},
	}
	body := c.renderIssueBody(v)

	wantHTML := "https://storage.googleapis.com/test-artifacts-product-first/results/v1/post-deploy/gcp/jx-staging/leartech-auth-ui/0.0.36/end2end-ui/playwright-report/index.html"
	if !strings.Contains(body, wantHTML) {
		t.Errorf("issue body missing HTML report link\n want: %s\n body: %s", wantHTML, body)
	}
	if !strings.Contains(body, "trace.playwright.dev") {
		t.Error("body should mention trace.playwright.dev for trace.zip viewing")
	}
	if !strings.Contains(body, "**Artifacts**") {
		t.Error("body should have an Artifacts section header")
	}
}

func TestRenderIssueBody_NoArtifactsWhenBucketUnset(t *testing.T) {
	// Default IssueClient (Bucket/PathTemplate empty) → no artifact section.
	c := &IssueClient{Owner: "mikelear", GitOpsRepo: "x/y", GitOpsPullNo: "1"}
	v := ServiceVerdict{
		Service: "x", Version: "0.1", Pass: false,
		FailedPacks: []string{"end2end-ui"},
	}
	body := c.renderIssueBody(v)
	if strings.Contains(body, "**Artifacts**") {
		t.Error("artifact section should not render when bucket/template empty")
	}
	if strings.Contains(body, "trace.playwright.dev") {
		t.Error("trace.playwright.dev mention should not render when bucket/template empty")
	}
}

func TestRenderIssueBody_MissingTestsRendered(t *testing.T) {
	c := &IssueClient{Owner: "mikelear", GitOpsRepo: "x/y", GitOpsPullNo: "1"}
	v := ServiceVerdict{
		Service: "leartech-qa-canary", Version: "0.0.5",
		Pass:         false,
		Reason:       "shift-left: 1 missing",
		MissingTests: []string{"smoke"},
	}
	body := c.renderIssueBody(v)
	if !strings.Contains(body, "Missing required tests") {
		t.Error("missing-tests section should render")
	}
	if !strings.Contains(body, "- `smoke`") {
		t.Error("missing test name should list")
	}
}

func TestBodyReasonMatches(t *testing.T) {
	body := "<!-- leartech-gate-blocking-issue -->\n## :x: blocking\n\n**Verdict reason:**\n\n> Arrival.phase=Failed\n\nmore stuff"
	if !bodyReasonMatches(body, "Arrival.phase=Failed") {
		t.Error("expected reason to match")
	}
	if bodyReasonMatches(body, "different reason") {
		t.Error("expected mismatch for different reason")
	}
}

// TestTitlePrefix_VersionAgnostic locks the contract that the title
// prefix is service-only (no @version suffix), so the issue lifecycle
// survives version bumps when teams fix bugs and ship the next
// release. HasPrefix matches against both old and new title formats:
//   - `[leartech-gate] leartech-auth-ui@0.0.36 blocking …` (legacy)
//   - `[leartech-gate] leartech-auth-ui blocking …`        (legacy unsuffixed)
//   - `[leartech-gate-gcp] leartech-auth-ui blocking …`    (current)
func TestTitlePrefix_VersionAgnostic(t *testing.T) {
	c := &IssueClient{}
	prefix := c.titlePrefixFor("leartech-auth-ui")

	legacyTitle := "[leartech-gate] leartech-auth-ui@0.0.36 blocking promotion to production"
	currentTitle := "[leartech-gate] leartech-auth-ui blocking promotion to production"
	otherTitle := "[leartech-gate] leartech-other-service blocking promotion to production"

	if !strings.HasPrefix(legacyTitle, prefix) {
		t.Error("legacy @<version> title should still match the version-agnostic prefix")
	}
	if !strings.HasPrefix(currentTitle, prefix) {
		t.Error("current title should match prefix")
	}
	if strings.HasPrefix(otherTitle, prefix) {
		t.Error("different service title should NOT match prefix")
	}
}

// TestTitlePrefix_PerClusterIsolation locks the cross-cluster
// non-interference contract. With cluster set, GCP's title prefix must
// not match an AZ-created issue (and vice versa) — otherwise a passing
// verdict on one cluster would close a still-failing issue on the other.
func TestTitlePrefix_PerClusterIsolation(t *testing.T) {
	gcp := &IssueClient{Cluster: "gcp"}
	az := &IssueClient{Cluster: "az"}

	gcpPrefix := gcp.titlePrefixFor("leartech-auth-ui")
	azPrefix := az.titlePrefixFor("leartech-auth-ui")

	if gcpPrefix == azPrefix {
		t.Fatalf("per-cluster prefixes must differ — got %q for both", gcpPrefix)
	}
	if !strings.Contains(gcpPrefix, "gcp") {
		t.Errorf("gcp prefix should embed cluster: %q", gcpPrefix)
	}
	if !strings.Contains(azPrefix, "az") {
		t.Errorf("az prefix should embed cluster: %q", azPrefix)
	}

	// Critical: GCP's prefix must NOT match an AZ-titled issue.
	azTitle := azPrefix + " blocking promotion to production"
	if strings.HasPrefix(azTitle, gcpPrefix) {
		t.Errorf("GCP prefix %q matched AZ title %q — would cross-close issues", gcpPrefix, azTitle)
	}
}

// TestBodyMarker_PerCluster locks the same isolation for body markers
// (used by the idempotency check).
func TestBodyMarker_PerCluster(t *testing.T) {
	gcp := &IssueClient{Cluster: "gcp"}
	az := &IssueClient{Cluster: "az"}
	if gcp.bodyMarkerFor() == az.bodyMarkerFor() {
		t.Errorf("per-cluster body markers must differ")
	}
	// Empty cluster falls back to legacy marker so older bodies still
	// match for migration.
	legacy := &IssueClient{}
	if legacy.bodyMarkerFor() != "<!-- leartech-gate-blocking-issue -->" {
		t.Errorf("legacy fallback marker mismatch: %q", legacy.bodyMarkerFor())
	}
}

func TestIssueOutcomeString(t *testing.T) {
	cases := map[IssueOutcome]string{
		IssueNoop:    "noop",
		IssueCreated: "created",
		IssueUpdated: "updated",
		IssueClosed:  "closed",
		IssueErrored: "errored",
	}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("IssueOutcome(%d).String() = %q, want %q", in, got, want)
		}
	}
}
