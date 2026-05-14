// Layer 1 duration-regression quill — reads per-test duration_ms from
// the current + previous arrivals' results.json (uploaded by the
// observer's dispatch wrapper to the post-deploy GCS path) and flags
// tests whose duration regressed beyond the configured thresholds.
//
// Why Layer 1: the per-test JSON contains the dispatched-test's actual
// inner duration (e.g. Playwright test wall-clock excluding K8s Job
// orchestration overhead). A 1s sleep in the SUT shows up cleanly as
// "smoke 60ms → 1080ms (18×)" — no Tempo query needed for the headline
// signal. Layer 2 (forensics-runner's diff.json) drills into per-
// endpoint p95 within the same test window when a regression is flagged.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// Default thresholds — a test must regress on BOTH ratio AND absolute
// delta to flag. The "AND" gates noisy tiny-duration fluctuations
// (60ms → 200ms is 3.3× but only +140ms — likely measurement noise,
// not a real regression) and absolute-only spikes (10s → 11s is +1000ms
// but only 1.1× — likely cold-start variance, not a real regression).
//
// 3× / +500ms is a starting point — tune from operational data.
const (
	defaultLatencyRegressionRatio = 3.0
	defaultLatencyRegressionDelta = 500.0 // ms
	httpFetchTimeout              = 10 * time.Second
)

// ResultsFile is the schema of results.json (written by the runner Job
// per pack). Mirrors the catalog end2end task's emission format.
type ResultsFile struct {
	Success bool         `json:"success"`
	Tests   []TestResult `json:"tests"`
}

// TestResult is one entry in ResultsFile.Tests.
type TestResult struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	DurationMs float64 `json:"duration_ms"`
	Message    string  `json:"message,omitempty"`
}

// DurationRegression records one flagged test for verdict rendering.
type DurationRegression struct {
	Pack    string  // test pack (e.g. "end2end")
	Name    string  // test name within the pack (e.g. "smoke")
	PrevMs  float64 // baseline duration from previous arrival's results.json
	CurrMs  float64 // current duration from this arrival's results.json
	Ratio   float64 // CurrMs / PrevMs
	DeltaMs float64 // CurrMs - PrevMs
}

// String renders a regression as the per-line summary that goes into
// the verdict reason / comment table.
func (r DurationRegression) String() string {
	return fmt.Sprintf("%s/%s: %.0fms → %.0fms (%.1f× / +%.0fms)",
		r.Pack, r.Name, r.PrevMs, r.CurrMs, r.Ratio, r.DeltaMs)
}

// httpGetter is the minimum of http.Client we need — abstracted for
// httptest-based unit tests. http.DefaultClient satisfies this.
type httpGetter interface {
	Do(req *http.Request) (*http.Response, error)
}

// evaluateDurationRegressionQuill is the Layer 1 evaluator. Compares
// per-test duration_ms between the current arrival and the most-recent
// finalized prior arrival of the same service. Graceful pass-through
// when no comparison is possible (first deploy, missing artifact, etc.) —
// "we can't tell" is not the same as "regression detected".
func evaluateDurationRegressionQuill(
	ctx context.Context,
	dyn dynamic.Interface,
	httpc httpGetter,
	bucket, postDeployTpl, cluster, namespace string,
	rel Release,
) ServiceVerdict {
	v := ServiceVerdict{Service: rel.Name, Version: rel.Version, Pass: true}

	// 1. Current arrival — needed for pack-name iteration.
	current, err := findArrivalByLabel(ctx, dyn, namespace, rel.Name, rel.Version)
	if err != nil {
		v.Reason = fmt.Sprintf("current arrival lookup failed: %v", err)
		return v
	}
	if current == nil {
		v.Reason = "no current arrival; nothing to compare"
		return v
	}
	packs := arrivalPackNames(current)
	if len(packs) == 0 {
		v.Reason = "current arrival has no tests; nothing to compare"
		return v
	}

	// 2. Previous arrival — different version, most recent finalized.
	prev, err := findPriorFinalizedArrival(ctx, dyn, namespace, rel.Name, rel.Version)
	if err != nil {
		v.Reason = fmt.Sprintf("previous arrival lookup failed: %v", err)
		return v
	}
	if prev == nil {
		v.Reason = "no previous arrival for baseline; can't compare durations"
		return v
	}
	prevVersion, _, _ := unstructured.NestedString(prev.Object, "spec", "version")

	// 3. Compare per pack. Missing results.json (HTTP 404, parse error,
	// etc.) is logged but not fatal — pack is skipped, others continue.
	var regressions []DurationRegression
	packsCompared := 0
	for _, pack := range packs {
		currURL, err := postDeployResultsURL(bucket, postDeployTpl, cluster, namespace, rel.Name, rel.Version, pack)
		if err != nil {
			continue
		}
		prevURL, err := postDeployResultsURL(bucket, postDeployTpl, cluster, namespace, rel.Name, prevVersion, pack)
		if err != nil {
			continue
		}
		currResults, err := fetchResultsJSON(ctx, httpc, currURL)
		if err != nil {
			continue
		}
		prevResults, err := fetchResultsJSON(ctx, httpc, prevURL)
		if err != nil {
			continue
		}
		packsCompared++
		regressions = append(regressions, compareDurations(
			pack, *prevResults, *currResults,
			defaultLatencyRegressionRatio, defaultLatencyRegressionDelta,
		)...)
	}

	if packsCompared == 0 {
		v.Reason = "no results.json artifacts available for comparison"
		return v
	}

	if len(regressions) == 0 {
		v.Reason = fmt.Sprintf("no duration regressions across %d pack(s) vs %s", packsCompared, prevVersion)
		return v
	}

	v.Pass = false
	v.Reason = fmt.Sprintf("%d duration regression(s) vs %s", len(regressions), prevVersion)
	for _, r := range regressions {
		v.FailedTests = append(v.FailedTests, r.String())
		v.FailedPacks = append(v.FailedPacks, r.Pack)
	}
	return v
}

// compareDurations finds per-test regressions where current vs prior
// exceeds BOTH ratio AND absolute-delta thresholds. Tests present in
// only one side are ignored (added or removed tests are surfaced by
// other mechanisms — PR diff for added, post-deploy fail for removed
// that should still exist).
func compareDurations(pack string, prev, curr ResultsFile, ratioThreshold, deltaThreshold float64) []DurationRegression {
	prevByName := make(map[string]float64, len(prev.Tests))
	for _, t := range prev.Tests {
		prevByName[t.Name] = t.DurationMs
	}
	var out []DurationRegression
	for _, t := range curr.Tests {
		prevMs, ok := prevByName[t.Name]
		if !ok || prevMs <= 0 {
			continue
		}
		delta := t.DurationMs - prevMs
		ratio := t.DurationMs / prevMs
		if ratio >= ratioThreshold && delta >= deltaThreshold {
			out = append(out, DurationRegression{
				Pack:    pack,
				Name:    t.Name,
				PrevMs:  prevMs,
				CurrMs:  t.DurationMs,
				Ratio:   ratio,
				DeltaMs: delta,
			})
		}
	}
	return out
}

// fetchResultsJSON HTTP-GETs the bucket's public object URL and parses
// it as ResultsFile. Bucket is publicly readable (allUsers/objectViewer
// per Product-First_GCP/test-artifacts.tf), so no auth is needed.
func fetchResultsJSON(ctx context.Context, httpc httpGetter, url string) (*ResultsFile, error) {
	reqCtx, cancel := context.WithTimeout(ctx, httpFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var r ResultsFile
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse results.json: %w", err)
	}
	return &r, nil
}

// postDeployResultsURL renders the public HTTPS URL of results.json for
// a given service+version+pack under the post-deploy path template.
func postDeployResultsURL(bucket, tpl, cluster, namespace, service, version, pack string) (string, error) {
	prefix, err := renderPostDeployPathPrefix(tpl, pathVars{
		Cluster: cluster, Namespace: namespace, Service: service, Version: version, Pack: pack,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s/results.json", bucket, prefix), nil
}

// arrivalPackNames extracts the list of test pack names from an
// Arrival's status.tests[].
func arrivalPackNames(arr *unstructured.Unstructured) []string {
	tests, _, _ := unstructured.NestedSlice(arr.Object, "status", "tests")
	out := make([]string, 0, len(tests))
	for _, t := range tests {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := tm["name"].(string); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// findArrivalByLabel looks up an Arrival by service+version label.
// Returns the most-recently-created match (handles rare reinstall case)
// or nil when no match.
func findArrivalByLabel(ctx context.Context, dyn dynamic.Interface, ns, service, version string) (*unstructured.Unstructured, error) {
	selector := fmt.Sprintf("qa.leartech.com/service=%s,qa.leartech.com/version=%s", service, version)
	list, err := dyn.Resource(arrivalGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	var best *unstructured.Unstructured
	for i := range list.Items {
		item := &list.Items[i]
		if best == nil || item.GetCreationTimestamp().After(best.GetCreationTimestamp().Time) {
			best = item
		}
	}
	return best, nil
}

// findPriorFinalizedArrival returns the most-recent finalized Arrival
// for the service whose version != currentVersion. Skipped phase is
// excluded (those reflect "no testPacks configured", not a real prior
// run worth comparing against).
func findPriorFinalizedArrival(ctx context.Context, dyn dynamic.Interface, ns, service, currentVersion string) (*unstructured.Unstructured, error) {
	selector := fmt.Sprintf("qa.leartech.com/service=%s", service)
	list, err := dyn.Resource(arrivalGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return nil, nil
		}
		return nil, err
	}
	var (
		best     *unstructured.Unstructured
		bestTime time.Time
	)
	for i := range list.Items {
		item := &list.Items[i]
		ver, _, _ := unstructured.NestedString(item.Object, "spec", "version")
		if ver == "" || ver == currentVersion {
			continue
		}
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if phase != "Passed" && phase != "Failed" && phase != "Timeout" {
			continue
		}
		fAt, _, _ := unstructured.NestedString(item.Object, "status", "finalizedAt")
		if fAt == "" {
			continue
		}
		ft, err := time.Parse(time.RFC3339, fAt)
		if err != nil {
			continue
		}
		if ft.After(bestTime) {
			bestTime = ft
			best = item
		}
	}
	return best, nil
}
