package main

import (
	"strings"
	"testing"
)

func TestRenderPostDeployPathPrefix_Default(t *testing.T) {
	got, err := renderPostDeployPathPrefix(
		"results/v1/post-deploy/{{.Cluster}}/{{.Namespace}}/{{.Service}}/{{.Version}}/{{.Pack}}",
		pathVars{
			Cluster: "gcp", Namespace: "jx-staging",
			Service: "leartech-auth-ui", Version: "0.0.36", Pack: "end2end-ui",
		},
	)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "results/v1/post-deploy/gcp/jx-staging/leartech-auth-ui/0.0.36/end2end-ui"
	if got != want {
		t.Errorf("\n got:  %s\n want: %s", got, want)
	}
}

func TestRenderPostDeployPathPrefix_Empty(t *testing.T) {
	got, err := renderPostDeployPathPrefix("", pathVars{})
	if err != nil || got != "" {
		t.Errorf("empty template: got (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestRenderPostDeployPathPrefix_MissingKeyErrors(t *testing.T) {
	_, err := renderPostDeployPathPrefix("{{.Wrongkey}}", pathVars{Cluster: "gcp"})
	if err == nil {
		t.Error("expected error on unknown template key, got nil")
	}
}

func TestRenderPlaywrightReportURL(t *testing.T) {
	got := renderPlaywrightReportURL("test-artifacts-product-first", "results/v1/post-deploy/gcp/jx-staging/leartech-auth-ui/0.0.36/end2end-ui")
	want := "https://storage.googleapis.com/test-artifacts-product-first/results/v1/post-deploy/gcp/jx-staging/leartech-auth-ui/0.0.36/end2end-ui/playwright-report/index.html"
	if got != want {
		t.Errorf("\n got:  %s\n want: %s", got, want)
	}
}

func TestRenderPlaywrightReportURL_EmptyInputs(t *testing.T) {
	if got := renderPlaywrightReportURL("", "x"); got != "" {
		t.Errorf("empty bucket should return empty URL, got %q", got)
	}
	if got := renderPlaywrightReportURL("b", ""); got != "" {
		t.Errorf("empty prefix should return empty URL, got %q", got)
	}
}

func TestRenderTestResultsListingURL(t *testing.T) {
	got := renderTestResultsListingURL("test-artifacts-product-first", "results/v1/post-deploy/gcp/jx-staging/x/0.1/end2end-ui")
	if !strings.Contains(got, "/storage/v1/b/test-artifacts-product-first/o?prefix=") {
		t.Errorf("listing URL missing GCS XML API path: %s", got)
	}
	if !strings.Contains(got, "test-results/") {
		t.Errorf("listing URL missing test-results/ filter: %s", got)
	}
}
