// Path-template rendering for the result-store. CONTRACT — must match
// the writer side (leartech-arrivals-observer/charts/.../values.yaml
// paths.postDeployTemplate). Renders the per-(service, version, pack)
// path under which the dispatched runner Job uploaded results.json,
// trace.zip, screenshots, video, and the Playwright HTML report.
//
// Default mirrors arrivals-observer's chart default. Override via
// --post-deploy-path-template flag or PATHS_POST_DEPLOY_TEMPLATE env
// when the writer's template diverges.
package main

import (
	"bytes"
	"fmt"
	"text/template"
)

// pathVars is the substitution context for the path template.
type pathVars struct {
	Cluster   string
	Namespace string
	Service   string
	Version   string
	Pack      string
}

// renderPostDeployPathPrefix substitutes the template against the
// per-test-pack vars. Empty template ⇒ empty string + no error.
func renderPostDeployPathPrefix(tmpl string, v pathVars) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	t, err := template.New("path").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", tmpl, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// renderPlaywrightReportURL returns the public HTTPS URL of the
// Playwright HTML report for a given test pack. Bucket is publicly
// readable (allUsers/objectViewer per Product-First_GCP/test-artifacts.tf),
// so this URL is directly browseable. Empty bucket/prefix ⇒ "" so the
// caller can omit the link in the verdict comment.
func renderPlaywrightReportURL(bucket, pathPrefix string) string {
	if bucket == "" || pathPrefix == "" {
		return ""
	}
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s/playwright-report/index.html", bucket, pathPrefix)
}

// renderTestResultsListingURL returns the GCS object-listing URL
// (gsutil ls equivalent) for the per-pack test-results directory.
// Useful for engineers who want to manually pull a specific trace.zip
// when there are many failed tests to triage. Empty bucket/prefix ⇒ "".
func renderTestResultsListingURL(bucket, pathPrefix string) string {
	if bucket == "" || pathPrefix == "" {
		return ""
	}
	// Public GCS XML listing — works without auth thanks to allUsers/
	// objectViewer on the bucket. ?prefix= filters to just our pack.
	return fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o?prefix=%s/test-results/", bucket, pathPrefix)
}
