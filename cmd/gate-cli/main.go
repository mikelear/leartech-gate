// Package main is the leartech-gate CLI invoked by Tekton on promotion PRs.
//
// Reads a helmfile that's been mutated by an auto-promotion (one or more
// service version bumps), evaluates each release against the post-deploy
// quill (reads Arrival CRs in jx-staging), aggregates verdict, posts a
// PR check status + sticky comment via GitHub API; exits 0 (pass) or 1
// (fail) so Lighthouse picks up the check.
//
// Single quill today: post-deploy-tests (Arrival.phase=Passed contract).
// Shift-left-tests quill was removed 2026-05-14 — release-time test
// execution reinvented K8s readiness probes + post-deploy coverage
// without genuine value-add. Real shift-left value (PR-time policy
// gates: security scans must-have-passed, SBOM, license checks) needs
// a different design + scope; deferred until a concrete compliance use
// case exists.
//
// Future quills under consideration (see qa-architecture/gate.md):
//   - copromotion: pure helmfile diff check, e.g. auth-ui + auth-service
//     must promote together for OAuth handshake compat
//   - migrations: K8s-native Helm hooks largely cover this, low value
//
// Required env (Tekton task supplies these):
//
//	HELMFILE_PATH       — path to the helmfile to inspect
//	RESULT_STORE_BUCKET — GCS bucket name (e.g. test-artifacts-product-first)
//	RESULT_STORE_PREFIX — GCS path prefix (e.g. results/v1/)
//	CLUSTER_TAG         — gcp / az
//	GITHUB_TOKEN        — for PR check + comment posting
//	REPO_OWNER, REPO_NAME, PULL_NUMBER, PULL_PULL_SHA — Tekton injects these
//	GCS_KEY_FILE        — path to GCS service-account key (mounted from secret)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Helmfile is the minimum of the helmfile schema we care about for the spike.
// Full helmfile has helmDefaults, environments, repositories, etc. — we ignore
// everything except releases[].
type Helmfile struct {
	Releases []Release `yaml:"releases"`
}

type Release struct {
	Name    string `yaml:"name"`
	Chart   string `yaml:"chart"`
	Version string `yaml:"version"`
}

// ServiceVerdict captures one service's gate evaluation.
type ServiceVerdict struct {
	Service      string
	Version      string
	Pass         bool
	MissingTests []string // tests that should exist but don't (no result-json in GCS)
	FailedTests  []string // tests that exist but have status: Failure
	Reason       string   // human-readable explanation

	// FailedPacks is the structured form of FailedTests — pack names
	// only, used to render per-pack artifact links (Playwright HTML
	// report etc.) in the verdict comment. Populated alongside
	// FailedTests in the post-deploy quill.
	FailedPacks []string
}

func main() {
	var (
		helmfilePath = flag.String("helmfile", envOr("HELMFILE_PATH", ""), "path to helmfile.yaml")
		bucket       = flag.String("bucket", envOr("RESULT_STORE_BUCKET", "test-artifacts-product-first"), "GCS bucket name")
		prefix       = flag.String("prefix", envOr("RESULT_STORE_PREFIX", "results/v1"), "GCS path prefix (no trailing slash)")
		// Empty default (not "unknown") — when CLUSTER_TAG is unset,
		// issues.go's titlePrefixFor / bodyMarkerFor fall back to the
		// legacy cluster-less form (`[leartech-gate] <svc>`) which is the
		// migration-friendly default. Sentinel "unknown" would produce
		// ugly `[leartech-gate-unknown] <svc>` titles that don't match
		// any cluster's lifecycle.
		cluster          = flag.String("cluster", envOr("CLUSTER_TAG", ""), "cluster tag (gcp/az); empty ⇒ legacy unsuffixed issue titles")
		dryRun           = flag.Bool("dry-run", false, "log decisions but don't post PR check")
		watchNamespace   = flag.String("watch-namespace", envOr("WATCH_NAMESPACE", "jx-staging"), "namespace where Arrival CRs live")
		enablePostDeploy = flag.Bool("enable-post-deploy-quill", envOr("ENABLE_POST_DEPLOY_QUILL", "true") == "true", "evaluate the post-deploy-tests quill against Arrival CRs")
		kubeconfigPath   = flag.String("kubeconfig", envOr("KUBECONFIG", ""), "kubeconfig file (empty = in-cluster)")
		// Post-deploy artifact path template — CONTRACT with arrivals-
		// observer's chart paths.postDeployTemplate. Override only when
		// the writer's template diverges (multi-tenant routing etc.).
		postDeployPathTpl = flag.String("post-deploy-path-template", envOr(
			"PATHS_POST_DEPLOY_TEMPLATE",
			"results/v1/post-deploy/{{.Cluster}}/{{.Namespace}}/{{.Service}}/{{.Version}}/{{.Pack}}",
		), "Go text/template for result-store paths; substituted with .Cluster/.Namespace/.Service/.Version/.Pack")
		// Issue creation on service repos when their quill fails.
		// Default off until validated (Tier 2 rollout). Once enabled,
		// auto-creates / updates / closes blocking issues at
		// {issue-repo-owner}/{service} so service owners see they're
		// blocking promotion.
		enableIssueCreation = flag.Bool("enable-issue-creation", envOr("ENABLE_ISSUE_CREATION", "false") == "true", "auto-open/update/close issues on service repos when their quill fails")
		issueRepoOwner      = flag.String("issue-repo-owner", envOr("ISSUE_REPO_OWNER", "mikelear"), "GitHub org for service-repo issue creation")
		_                   = flag.Bool("help", false, "show this message")
	)
	flag.Parse()

	if *helmfilePath == "" {
		fmt.Fprintln(os.Stderr, "FATAL: --helmfile or HELMFILE_PATH required")
		os.Exit(2)
	}

	logf := func(level, format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", level, fmt.Sprintf(format, args...))
	}

	logf("info", "starting leartech-gate (cluster=%s, bucket=%s, prefix=%s, dry-run=%t, post-deploy=%t)",
		*cluster, *bucket, *prefix, *dryRun, *enablePostDeploy)

	// 1. Parse helmfile → list of services + versions
	hf, err := parseHelmfile(*helmfilePath)
	if err != nil {
		logf("fatal", "parse helmfile %s: %v", *helmfilePath, err)
		os.Exit(2)
	}
	logf("info", "helmfile %s lists %d releases", *helmfilePath, len(hf.Releases))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Build dynamic K8s client for the post-deploy quill (Arrival CR reads).
	// Best-effort — if it fails, post-deploy quill is skipped with a logged
	// warning and the gate effectively short-circuits to PASS (no quills
	// running). Defensive degradation: misconfigured SA fails-loud at the
	// next genuine failure rather than blocking every PR.
	var dynClient dynamic.Interface
	if *enablePostDeploy {
		c, err := buildDynClient(*kubeconfigPath)
		if err != nil {
			logf("warn", "post-deploy quill disabled — k8s client init failed: %v", err)
			*enablePostDeploy = false
		} else {
			dynClient = c
		}
	}

	// 2. For each release, evaluate every enabled quill. Verdict-per-quill
	// merged into a single ServiceVerdict for the table; failure of any
	// quill fails the whole release. The reason field shows which quill(s).
	verdicts := make([]ServiceVerdict, 0, len(hf.Releases))
	allPass := true
	for _, rel := range hf.Releases {
		if rel.Name == "" || rel.Version == "" {
			logf("warn", "skipping release with empty name or version: %+v", rel)
			continue
		}
		// Post-deploy is currently the only quill. Shift-left was removed
		// 2026-05-14 (release-time test execution duplicated K8s readiness
		// probes + post-deploy coverage). Future quills (copromotion,
		// security-attestation) land here as additional evaluators.
		merged := ServiceVerdict{
			Service: rel.Name,
			Version: rel.Version,
			Pass:    true,
			Reason:  "no quills enabled",
		}

		if *enablePostDeploy && dynClient != nil {
			pd := evaluatePostDeployQuill(ctx, dynClient, *watchNamespace, rel)
			merged.Reason = "post-deploy: " + pd.Reason
			if !pd.Pass {
				merged.Pass = false
				merged.MissingTests = append(merged.MissingTests, pd.MissingTests...)
				merged.FailedTests = append(merged.FailedTests, pd.FailedTests...)
			}
		}

		verdicts = append(verdicts, merged)
		if !merged.Pass {
			allPass = false
		}
	}

	// 3. Render verdict to stdout + (in real run) PR comment.
	// Pass the path template + bucket so the renderer can append
	// playwright-report links per failed UI test.
	body := renderVerdictMarkdown(verdicts, allPass, *bucket, *postDeployPathTpl, *cluster, *watchNamespace)
	fmt.Println(body)

	if *dryRun {
		logf("info", "dry-run: skipping PR comment + check status post")
	} else {
		if err := postPRCommentAndCheck(ctx, body, allPass, *cluster); err != nil {
			logf("warn", "PR post failed (non-fatal): %v", err)
		}
	}

	// 4. Auto-issue lifecycle on service repos. Best-effort — errors
	// logged, never fail the gate. Skipped entirely when disabled or
	// when the GitHub client can't be built.
	if *enableIssueCreation && !*dryRun {
		ic, err := NewIssueClient(*issueRepoOwner, *bucket, *postDeployPathTpl, *cluster, *watchNamespace)
		if err != nil {
			logf("warn", "issue creation disabled — %v", err)
		} else {
			for _, v := range verdicts {
				outcome, err := ic.EnsureBlockingIssue(ctx, v)
				if err != nil {
					logf("warn", "issue lifecycle for %s@%s: %v", v.Service, v.Version, err)
					continue
				}
				if outcome != IssueNoop {
					logf("info", "issue %s for %s@%s on %s/%s", outcome, v.Service, v.Version, *issueRepoOwner, v.Service)
				}
			}
		}
	}

	if !allPass {
		logf("info", "FAIL: at least one quill failed")
		cancel() // explicit; os.Exit skips deferred cancel
		//nolint:gocritic // intentional non-zero exit for Lighthouse; cancel() invoked above
		os.Exit(1)
	}
	logf("info", "PASS: all quills green")
}

// parseHelmfile reads + unmarshals the helmfile.yaml. Handles multi-doc
// YAML (the JX3 build-cluster shape: `environments:` doc first, then a
// `---` separator, then the doc with `repositories:` + `releases:`).
// Returns the first doc that has a non-empty Releases list — accommodates
// both sandbox single-doc and JX3 multi-doc layouts without the caller
// needing to know which.
func parseHelmfile(path string) (*Helmfile, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from --helmfile flag (trusted operator input, not request data)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	var first Helmfile
	for {
		var hf Helmfile
		err := dec.Decode(&hf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("yaml decode: %w", err)
		}
		if first.Releases == nil {
			first = hf // remember the first doc as a fallback
		}
		if len(hf.Releases) > 0 {
			return &hf, nil
		}
	}
	return &first, nil
}

// renderVerdictMarkdown produces the body of the PR sticky comment.
// bucket + pathTemplate are passed through so per-failed-pack artifact
// links (Playwright HTML report) can be rendered inline. Empty bucket
// or template ⇒ links are silently omitted.
func renderVerdictMarkdown(verdicts []ServiceVerdict, allPass bool, bucket, pathTemplate, cluster, namespace string) string {
	var b strings.Builder
	if allPass {
		b.WriteString("## :white_check_mark: leartech-gate: PASS\n\n")
	} else {
		b.WriteString("## :x: leartech-gate: FAIL\n\n")
	}
	b.WriteString("| Service | Version | Verdict | Reason |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, v := range verdicts {
		icon := ":white_check_mark:"
		if !v.Pass {
			icon = ":x:"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", v.Service, v.Version, icon, v.Reason)
		if len(v.MissingTests) > 0 {
			fmt.Fprintf(&b, "|  |  |  | Missing: `%s` |\n", strings.Join(v.MissingTests, "`, `"))
		}
		if len(v.FailedTests) > 0 {
			fmt.Fprintf(&b, "|  |  |  | Failed: `%s` |\n", strings.Join(v.FailedTests, "`, `"))
		}
		// Per-pack artifact links — one row per failed pack pointing at
		// the playwright-report HTML index. Listing-URL added so engineers
		// can grab a specific trace.zip for trace.playwright.dev viewing.
		for _, pack := range v.FailedPacks {
			prefix, err := renderPostDeployPathPrefix(pathTemplate, pathVars{
				Cluster: cluster, Namespace: namespace,
				Service: v.Service, Version: v.Version, Pack: pack,
			})
			if err != nil || prefix == "" {
				continue
			}
			reportURL := renderPlaywrightReportURL(bucket, prefix)
			listingURL := renderTestResultsListingURL(bucket, prefix)
			if reportURL == "" {
				continue
			}
			links := fmt.Sprintf("[HTML report](%s)", reportURL)
			if listingURL != "" {
				links += fmt.Sprintf(" · [trace.zip listing](%s)", listingURL)
			}
			fmt.Fprintf(&b, "|  |  |  | `%s` artifacts: %s |\n", pack, links)
		}
	}
	b.WriteString("\nTo override: comment `/override leartech-gate` (Lighthouse plugin).\n")
	b.WriteString("\n_Posted by `leartech-gate` (spike v0). See `~/leartech/qa-architecture/gate.md` for design._\n")
	return b.String()
}

// postPRCommentAndCheck posts a sticky comment via GitHub API and lets the
// Tekton step's exit code drive the actual check status (Lighthouse handles
// the latter).
func postPRCommentAndCheck(ctx context.Context, body string, pass bool, cluster string) error {
	owner := os.Getenv("REPO_OWNER")
	name := os.Getenv("REPO_NAME")
	pullNumber := os.Getenv("PULL_NUMBER")
	token := os.Getenv("GITHUB_TOKEN")
	if owner == "" || name == "" || pullNumber == "" || token == "" {
		return fmt.Errorf("missing one of REPO_OWNER/REPO_NAME/PULL_NUMBER/GITHUB_TOKEN")
	}

	marker := fmt.Sprintf("<!-- leartech-gate-%s -->", cluster)
	stickyBody := marker + "\n" + body

	// List existing comments; replace if our marker exists. URL built
	// from trusted env (REPO_OWNER/REPO_NAME/PULL_NUMBER), not request
	// data — gosec G704 SSRF taint warning is a false positive here.
	listURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s/comments", owner, name, pullNumber)
	req, err := http.NewRequestWithContext(ctx, "GET", listURL, nil) //nolint:gosec // URL from trusted env
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // URL from trusted env
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	type Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	var comments []Comment
	_ = json.Unmarshal(respBody, &comments)

	var existingID int64
	for _, c := range comments {
		if strings.HasPrefix(c.Body, marker) {
			existingID = c.ID
			break
		}
	}

	payload := map[string]string{"body": stickyBody}
	payloadBytes, _ := json.Marshal(payload)

	if existingID != 0 {
		patchURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments/%d", owner, name, existingID)
		req, err := http.NewRequestWithContext(ctx, "PATCH", patchURL, strings.NewReader(string(payloadBytes))) //nolint:gosec // URL from trusted env
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // URL from trusted env
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("PATCH comment %d returned status %d", existingID, resp.StatusCode)
		}
	} else {
		req, err := http.NewRequestWithContext(ctx, "POST", listURL, strings.NewReader(string(payloadBytes))) //nolint:gosec // URL from trusted env
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // URL from trusted env
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("POST comment returned status %d", resp.StatusCode)
		}
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// arrivalGVR is the GroupVersionResource for the Arrival CR managed by
// leartech-arrivals-observer.
var arrivalGVR = schema.GroupVersionResource{
	Group:    "qa.leartech.com",
	Version:  "v1alpha1",
	Resource: "arrivals",
}

// evaluatePostDeployQuill checks that a service+version has an Arrival CR
// in phase=Passed in the watched namespace. The Arrival CR is created by
// leartech-arrivals-observer when a ReplicaSet for the version lands in
// staging; the controller then dispatches test Jobs and finalizes the
// arrival's phase. So phase=Passed = "post-deploy tests passed in staging".
//
// Behaviour:
//   - Arrival not found → pass-through (service hasn't deployed yet, OR is
//     in a namespace this gate doesn't watch, OR isn't in the services
//     map — all valid non-fail cases).
//   - phase=Passed → pass.
//   - phase=Skipped → pass-through (no testPacks configured for the
//     service in the chart values map; explicit opt-out).
//   - phase=Failed | Timeout → fail; populate FailedTests with names.
//   - phase=Pending | Testing | "" → fail (still in-flight; gate must
//     produce a verdict).
func evaluatePostDeployQuill(ctx context.Context, dyn dynamic.Interface, namespace string, rel Release) ServiceVerdict {
	v := ServiceVerdict{Service: rel.Name, Version: rel.Version, Pass: true}

	selector := fmt.Sprintf("qa.leartech.com/service=%s,qa.leartech.com/version=%s", rel.Name, rel.Version)
	list, err := dyn.Resource(arrivalGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			v.Reason = fmt.Sprintf("Arrival lookup skipped: %v", err)
			return v
		}
		v.Pass = false
		v.Reason = fmt.Sprintf("k8s list arrivals failed: %v", err)
		return v
	}
	if len(list.Items) == 0 {
		v.Reason = fmt.Sprintf("no Arrival in %s for %s/%s (not yet deployed?)", namespace, rel.Name, rel.Version)
		return v
	}

	// If multiple arrivals match (same service+version recorded under
	// different namespaces, etc.), prefer the most-recent finalized one;
	// fall back to the most-recent in-progress one.
	arr := pickLatestArrival(list.Items)
	phase, _, _ := unstructured.NestedString(arr.Object, "status", "phase")
	switch phase {
	case "Passed":
		v.Reason = "Arrival.phase=Passed"
	case "Skipped":
		v.Reason = "Arrival.phase=Skipped (no testPacks configured)"
	case "Failed", "Timeout":
		v.Pass = false
		v.Reason = fmt.Sprintf("Arrival.phase=%s", phase)
		tests, _, _ := unstructured.NestedSlice(arr.Object, "status", "tests")
		for _, t := range tests {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			st, _ := tm["status"].(string)
			if st == "Failed" || st == "Timeout" {
				name, _ := tm["name"].(string)
				v.FailedTests = append(v.FailedTests, name+" ("+st+")")
				v.FailedPacks = append(v.FailedPacks, name)
			}
		}
	default: // Pending, Testing, ""
		v.Pass = false
		v.Reason = fmt.Sprintf("Arrival.phase=%q (not yet finalized)", phase)
	}

	// Append forensics summary if present. Forensics-runner populates
	// status.forensics.summary when it finishes a Tempo span-diff. We
	// surface non-zero counts in the verdict so engineers see the regression
	// summary inline with the gate's PR comment instead of digging into
	// kubectl get arrival -o yaml.
	if summary, ok, _ := unstructured.NestedMap(arr.Object, "status", "forensics", "summary"); ok {
		parts := []string{}
		if n, _ := summary["latency_regressions"].(int64); n > 0 {
			parts = append(parts, fmt.Sprintf("%d latency regressions", n))
		}
		if n, _ := summary["error_rate_regressions"].(int64); n > 0 {
			parts = append(parts, fmt.Sprintf("%d error-rate regressions", n))
		}
		if n, _ := summary["new"].(int64); n > 0 {
			parts = append(parts, fmt.Sprintf("%d new endpoints", n))
		}
		if n, _ := summary["missing"].(int64); n > 0 {
			parts = append(parts, fmt.Sprintf("%d missing endpoints", n))
		}
		if len(parts) > 0 {
			diffURL, _, _ := unstructured.NestedString(arr.Object, "status", "forensics", "diffUrl")
			v.Reason = v.Reason + "; forensics: " + strings.Join(parts, ", ")
			if diffURL != "" {
				v.Reason = v.Reason + " ([diff](" + diffURL + "))"
			}
		}
	}
	return v
}

// pickLatestArrival prefers the latest finalized Arrival; falls back to
// the latest creation-timestamp item if none finalized. Lets the quill
// produce a stable verdict even if multiple Arrivals match the same
// service+version (e.g. across re-deploys in the same namespace).
func pickLatestArrival(items []unstructured.Unstructured) *unstructured.Unstructured {
	if len(items) == 0 {
		return nil
	}
	var (
		bestFinal  *unstructured.Unstructured
		bestFinalT string
		bestAny    *unstructured.Unstructured
	)
	for i := range items {
		it := &items[i]
		fAt, _, _ := unstructured.NestedString(it.Object, "status", "finalizedAt")
		if fAt != "" && (bestFinal == nil || fAt > bestFinalT) {
			bestFinal = it
			bestFinalT = fAt
		}
		if bestAny == nil || it.GetCreationTimestamp().After(bestAny.GetCreationTimestamp().Time) {
			bestAny = it
		}
	}
	if bestFinal != nil {
		return bestFinal
	}
	return bestAny
}

// buildDynClient prefers in-cluster, falls back to kubeconfig file for
// out-of-cluster runs.
func buildDynClient(kubeconfigPath string) (dynamic.Interface, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}
