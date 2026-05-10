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
