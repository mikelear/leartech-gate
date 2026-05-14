package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestCompareDurations_FlagsBeyondBothThresholds(t *testing.T) {
	prev := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 60}}}
	curr := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 1080}}}
	got := compareDurations("end2end", prev, curr, 3.0, 500.0)
	if len(got) != 1 {
		t.Fatalf("want 1 regression, got %d", len(got))
	}
	if got[0].Name != "smoke" || got[0].Ratio != 18 || got[0].DeltaMs != 1020 {
		t.Errorf("unexpected regression: %+v", got[0])
	}
}

func TestCompareDurations_RatioOnly_BelowDeltaThreshold(t *testing.T) {
	// 60 → 200ms = 3.3× ratio but only +140ms delta — should NOT flag.
	prev := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 60}}}
	curr := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 200}}}
	got := compareDurations("end2end", prev, curr, 3.0, 500.0)
	if len(got) != 0 {
		t.Errorf("noisy small-duration shift should not flag: %+v", got)
	}
}

func TestCompareDurations_DeltaOnly_BelowRatioThreshold(t *testing.T) {
	// 10s → 11s = +1000ms delta but only 1.1× ratio — should NOT flag.
	prev := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 10000}}}
	curr := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 11000}}}
	got := compareDurations("end2end", prev, curr, 3.0, 500.0)
	if len(got) != 0 {
		t.Errorf("cold-start-style variance should not flag: %+v", got)
	}
}

func TestCompareDurations_NewTest_Skipped(t *testing.T) {
	prev := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 60}}}
	curr := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 60}, {Name: "newcheck", DurationMs: 5000}}}
	got := compareDurations("end2end", prev, curr, 3.0, 500.0)
	if len(got) != 0 {
		t.Errorf("new tests without baseline must be skipped, got %+v", got)
	}
}

func TestCompareDurations_RemovedTest_Skipped(t *testing.T) {
	prev := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 60}, {Name: "removed", DurationMs: 100}}}
	curr := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 60}}}
	got := compareDurations("end2end", prev, curr, 3.0, 500.0)
	if len(got) != 0 {
		t.Errorf("removed tests must be skipped by duration quill, got %+v", got)
	}
}

func TestCompareDurations_ZeroBaseline_Skipped(t *testing.T) {
	// Defensive: a 0ms baseline would produce +Inf ratio. Skip it.
	prev := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 0}}}
	curr := ResultsFile{Tests: []TestResult{{Name: "smoke", DurationMs: 1000}}}
	got := compareDurations("end2end", prev, curr, 3.0, 500.0)
	if len(got) != 0 {
		t.Errorf("zero baseline must be skipped to avoid divide-by-zero: %+v", got)
	}
}

func TestFetchResultsJSON_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"tests":[{"name":"smoke","status":"pass","duration_ms":60}]}`))
	}))
	defer srv.Close()
	got, err := fetchResultsJSON(context.Background(), http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !got.Success || len(got.Tests) != 1 || got.Tests[0].DurationMs != 60 {
		t.Errorf("parse mismatch: %+v", got)
	}
}

func TestFetchResultsJSON_HTTP404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := fetchResultsJSON(context.Background(), http.DefaultClient, srv.URL)
	if err == nil {
		t.Error("404 must surface as error so caller can skip pack")
	}
}

func TestFetchResultsJSON_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not valid json`))
	}))
	defer srv.Close()
	_, err := fetchResultsJSON(context.Background(), http.DefaultClient, srv.URL)
	if err == nil {
		t.Error("malformed JSON must surface as error")
	}
}

func TestPostDeployResultsURL(t *testing.T) {
	url, err := postDeployResultsURL(
		"test-artifacts-product-first",
		"results/v1/post-deploy/{{.Cluster}}/{{.Namespace}}/{{.Service}}/{{.Version}}/{{.Pack}}",
		"gcp", "jx-staging", "canary", "0.0.17", "end2end",
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := "https://storage.googleapis.com/test-artifacts-product-first/results/v1/post-deploy/gcp/jx-staging/canary/0.0.17/end2end/results.json"
	if url != want {
		t.Errorf("url:\n got  %s\n want %s", url, want)
	}
}

// arrivalScheme + buildArrival helpers — mirror the pattern from
// forensics-runner's main_test.go. Kept local to avoid a shared util.
func arrivalScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	gvk := arrivalGVR.GroupVersion().WithKind("Arrival")
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ArrivalList"), &unstructured.UnstructuredList{})
	return s
}

func buildArrival(name, service, version, phase, finalizedAt string, tests []map[string]any) *unstructured.Unstructured {
	a := &unstructured.Unstructured{}
	a.SetGroupVersionKind(arrivalGVR.GroupVersion().WithKind("Arrival"))
	a.SetName(name)
	a.SetNamespace("jx-staging")
	a.SetLabels(map[string]string{
		"qa.leartech.com/service": service,
		"qa.leartech.com/version": version,
	})
	_ = unstructured.SetNestedField(a.Object, service, "spec", "service")
	_ = unstructured.SetNestedField(a.Object, version, "spec", "version")
	if phase != "" {
		_ = unstructured.SetNestedField(a.Object, phase, "status", "phase")
	}
	if finalizedAt != "" {
		_ = unstructured.SetNestedField(a.Object, finalizedAt, "status", "finalizedAt")
	}
	if tests != nil {
		items := make([]any, 0, len(tests))
		for _, t := range tests {
			items = append(items, t)
		}
		_ = unstructured.SetNestedSlice(a.Object, items, "status", "tests")
	}
	return a
}

func TestFindPriorFinalizedArrival_HappyPath(t *testing.T) {
	a1 := buildArrival("canary-0.0.16", "canary", "0.0.16", "Passed", "2026-05-13T10:00:00Z", nil)
	a2 := buildArrival("canary-0.0.15", "canary", "0.0.15", "Passed", "2026-05-12T10:00:00Z", nil)
	dyn := dynamicfake.NewSimpleDynamicClient(arrivalScheme(), a1, a2)

	got, err := findPriorFinalizedArrival(context.Background(), dyn, "jx-staging", "canary", "0.0.17")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got == nil || got.GetName() != "canary-0.0.16" {
		t.Errorf("expected most-recent-finalized (canary-0.0.16), got %v", got)
	}
}

func TestFindPriorFinalizedArrival_SkipsSameVersion(t *testing.T) {
	a := buildArrival("canary-0.0.17", "canary", "0.0.17", "Passed", "2026-05-13T10:00:00Z", nil)
	dyn := dynamicfake.NewSimpleDynamicClient(arrivalScheme(), a)

	got, err := findPriorFinalizedArrival(context.Background(), dyn, "jx-staging", "canary", "0.0.17")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != nil {
		t.Errorf("an arrival of the same version must not be returned as prior")
	}
}

func TestFindPriorFinalizedArrival_SkipsNonFinalized(t *testing.T) {
	a := buildArrival("canary-0.0.16-pending", "canary", "0.0.16", "Testing", "", nil)
	dyn := dynamicfake.NewSimpleDynamicClient(arrivalScheme(), a)

	got, err := findPriorFinalizedArrival(context.Background(), dyn, "jx-staging", "canary", "0.0.17")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != nil {
		t.Errorf("non-finalized arrivals must not be returned as prior baseline")
	}
}

func TestEvaluateDurationRegressionQuill_NoPreviousPassThrough(t *testing.T) {
	// Only the current arrival exists — no baseline → pass-through.
	curr := buildArrival("canary-0.0.17", "canary", "0.0.17", "Passed", "2026-05-14T10:00:00Z", []map[string]any{
		{"name": "end2end"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(arrivalScheme(), curr)

	v := evaluateDurationRegressionQuill(
		context.Background(), dyn, http.DefaultClient,
		"bucket", "results/v1/post-deploy/{{.Cluster}}/{{.Namespace}}/{{.Service}}/{{.Version}}/{{.Pack}}",
		"gcp", "jx-staging", Release{Name: "canary", Version: "0.0.17"},
	)
	if !v.Pass {
		t.Errorf("no-baseline must pass-through, got fail: %s", v.Reason)
	}
	if !strings.Contains(v.Reason, "no previous arrival") {
		t.Errorf("reason should explain no-baseline, got %q", v.Reason)
	}
}

func TestEvaluateDurationRegressionQuill_HappyPathRegression(t *testing.T) {
	// Stub httpGetter matches the version segment in the URL path to
	// route current vs previous. Avoids httptest's https:// hostname
	// limitation — postDeployResultsURL hardcodes storage.googleapis.com
	// which can't be re-pointed at a local test server.
	httpc := &stubResultsClient{
		byVersion: map[string]string{
			"0.0.17": `{"success":true,"tests":[{"name":"smoke","status":"pass","duration_ms":1080}]}`,
			"0.0.16": `{"success":true,"tests":[{"name":"smoke","status":"pass","duration_ms":60}]}`,
		},
	}

	prev := buildArrival("canary-0.0.16", "canary", "0.0.16", "Passed", "2026-05-13T10:00:00Z", []map[string]any{
		{"name": "end2end"},
	})
	curr := buildArrival("canary-0.0.17", "canary", "0.0.17", "Passed", "2026-05-14T10:00:00Z", []map[string]any{
		{"name": "end2end"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(arrivalScheme(), prev, curr)

	v := evaluateDurationRegressionQuill(
		context.Background(), dyn, httpc,
		"test-artifacts-product-first",
		"results/v1/post-deploy/{{.Cluster}}/{{.Namespace}}/{{.Service}}/{{.Version}}/{{.Pack}}",
		"gcp", "jx-staging",
		Release{Name: "canary", Version: "0.0.17"},
	)
	if v.Pass {
		t.Fatalf("18× regression must fail, got pass with reason: %s", v.Reason)
	}
	if len(v.FailedTests) != 1 || !strings.Contains(v.FailedTests[0], "smoke") {
		t.Errorf("expected smoke regression in FailedTests, got %+v", v.FailedTests)
	}
	if !strings.Contains(v.FailedTests[0], "18.0×") {
		t.Errorf("expected 18× ratio in summary, got %s", v.FailedTests[0])
	}
}

// stubResultsClient routes by version segment in URL path. Synthesises
// a 200/JSON response for known versions, 404 for unknown.
type stubResultsClient struct {
	byVersion map[string]string
}

func (s *stubResultsClient) Do(req *http.Request) (*http.Response, error) {
	for ver, body := range s.byVersion {
		if strings.Contains(req.URL.Path, "/"+ver+"/") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}, nil
}

func TestFindPriorFinalizedArrival_PrefersMostRecentByFinalizedAt(t *testing.T) {
	// Two finalized prior arrivals — return the one with later finalizedAt.
	a1 := buildArrival("canary-0.0.14", "canary", "0.0.14", "Passed", "2026-05-10T10:00:00Z", nil)
	a2 := buildArrival("canary-0.0.15", "canary", "0.0.15", "Passed", "2026-05-12T10:00:00Z", nil)
	a3 := buildArrival("canary-0.0.16", "canary", "0.0.16", "Passed", "2026-05-13T10:00:00Z", nil)
	dyn := dynamicfake.NewSimpleDynamicClient(arrivalScheme(), a1, a2, a3)

	got, err := findPriorFinalizedArrival(context.Background(), dyn, "jx-staging", "canary", "0.0.17")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got == nil || got.GetName() != "canary-0.0.16" {
		t.Errorf("expected most-recent finalized prior (canary-0.0.16), got %v", got)
	}
	// Guard against API churn — verify finalizedAt is what we sorted by.
	fAt, _, _ := unstructured.NestedString(got.Object, "status", "finalizedAt")
	if _, err := time.Parse(time.RFC3339, fAt); err != nil {
		t.Errorf("finalizedAt didn't survive round trip: %v", err)
	}
}
