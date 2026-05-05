// Package main is the leartech-gate CLI invoked by Tekton on promotion PRs.
//
// Reads a helmfile that's been mutated by an auto-promotion (one or more
// service version bumps), looks up each promoted service's required tests
// in leartech-qa-management, and checks the result-store for matching
// per-test result JSONs at the SHA-keyed GCS path. Aggregates verdict;
// posts a PR check status + comment via GitHub API; exits 0 (pass) or
// 1 (fail) so Lighthouse picks up the check.
//
// Spike scope (Session 0 Step 6): one quill — shift-left-tests. No
// beaver-lib helmfile diff parsing yet (uses simple yq-style yaml read of
// the WHOLE helmfile rather than just the diff). No multi-quill framework.
// No risk-driven override stiffening. Phase 1 hardening adds those.
//
// Required env (Tekton task supplies these):
//   HELMFILE_PATH       — path to the helmfile to inspect
//   QA_MANAGEMENT_RAW   — raw GitHub URL prefix for qa-management
//                         (e.g. https://raw.githubusercontent.com/mikelear/leartech-qa-management/main)
//   RESULT_STORE_BUCKET — GCS bucket name (e.g. test-artifacts-product-first)
//   RESULT_STORE_PREFIX — GCS path prefix (e.g. results/v1/)
//   CLUSTER_TAG         — gcp / az
//   GITHUB_TOKEN        — for PR check + comment posting
//   REPO_OWNER, REPO_NAME, PULL_NUMBER, PULL_PULL_SHA — Tekton injects these
//   GCS_KEY_FILE        — path to GCS service-account key (mounted from secret)
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

// RequiredTests is qa-management's required-tests/<service>.yaml schema.
type RequiredTests struct {
	SchemaVersion string         `yaml:"schema_version"`
	Service       string         `yaml:"service"`
	Type          string         `yaml:"type"`
	Required      []RequiredTest `yaml:"required_tests"`
}

type RequiredTest struct {
	Name     string `yaml:"name"`
	TestPack string `yaml:"test_pack"`
	Blocking bool   `yaml:"blocking"`
	Suite    string `yaml:"suite"`
}

// ResultJSON is the per-test result schema written by the end2end task.
type ResultJSON struct {
	SchemaVersion string `json:"schema_version"`
	SHA           string `json:"sha"`
	Repo          string `json:"repo"`
	Cluster       string `json:"cluster"`
	TestName      string `json:"test_name"`
	TestPack      string `json:"test_pack"`
	Status        string `json:"status"`
	DurationMS    int    `json:"duration_ms"`
	Message       string `json:"message"`
}

// ServiceVerdict captures one service's gate evaluation.
type ServiceVerdict struct {
	Service       string
	Version       string
	Pass          bool
	MissingTests  []string // tests that should exist but don't (no result-json in GCS)
	FailedTests   []string // tests that exist but have status: Failure
	Reason        string   // human-readable explanation
}

func main() {
	var (
		helmfilePath     = flag.String("helmfile", envOr("HELMFILE_PATH", ""), "path to helmfile.yaml")
		qaMgmtRaw        = flag.String("qa-management", envOr("QA_MANAGEMENT_RAW", "https://raw.githubusercontent.com/mikelear/leartech-qa-management/main"), "raw GitHub URL prefix for qa-management")
		bucket           = flag.String("bucket", envOr("RESULT_STORE_BUCKET", "test-artifacts-product-first"), "GCS bucket name")
		prefix           = flag.String("prefix", envOr("RESULT_STORE_PREFIX", "results/v1"), "GCS path prefix (no trailing slash)")
		cluster          = flag.String("cluster", envOr("CLUSTER_TAG", "unknown"), "cluster tag (gcp/az)")
		dryRun           = flag.Bool("dry-run", false, "log decisions but don't post PR check")
		_                = flag.Bool("help", false, "show this message")
	)
	flag.Parse()

	if *helmfilePath == "" {
		fmt.Fprintln(os.Stderr, "FATAL: --helmfile or HELMFILE_PATH required")
		os.Exit(2)
	}

	logf := func(level, format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", level, fmt.Sprintf(format, args...))
	}

	logf("info", "starting leartech-gate (cluster=%s, bucket=%s, prefix=%s, dry-run=%t)", *cluster, *bucket, *prefix, *dryRun)

	// 1. Parse helmfile → list of services + versions
	hf, err := parseHelmfile(*helmfilePath)
	if err != nil {
		logf("fatal", "parse helmfile %s: %v", *helmfilePath, err)
		os.Exit(2)
	}
	logf("info", "helmfile %s lists %d releases", *helmfilePath, len(hf.Releases))

	// 2. For each release, evaluate the shift-left-tests quill
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	verdicts := make([]ServiceVerdict, 0, len(hf.Releases))
	allPass := true
	for _, rel := range hf.Releases {
		if rel.Name == "" || rel.Version == "" {
			logf("warn", "skipping release with empty name or version: %+v", rel)
			continue
		}
		v := evaluateShiftLeftQuill(ctx, *qaMgmtRaw, *bucket, *prefix, *cluster, rel)
		verdicts = append(verdicts, v)
		if !v.Pass {
			allPass = false
		}
	}

	// 3. Render verdict to stdout + (in real run) PR comment
	body := renderVerdictMarkdown(verdicts, allPass)
	fmt.Println(body)

	if *dryRun {
		logf("info", "dry-run: skipping PR comment + check status post")
	} else {
		if err := postPRCommentAndCheck(ctx, body, allPass, *cluster); err != nil {
			logf("warn", "PR post failed (non-fatal): %v", err)
		}
	}

	if !allPass {
		logf("info", "FAIL: at least one quill failed")
		os.Exit(1)
	}
	logf("info", "PASS: all quills green")
}

// parseHelmfile reads + unmarshals the helmfile.yaml.
func parseHelmfile(path string) (*Helmfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	var hf Helmfile
	if err := yaml.Unmarshal(data, &hf); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	return &hf, nil
}

// evaluateShiftLeftQuill runs the single quill for one release.
func evaluateShiftLeftQuill(ctx context.Context, qaMgmtRaw, bucket, prefix, cluster string, rel Release) ServiceVerdict {
	v := ServiceVerdict{
		Service: rel.Name,
		Version: rel.Version,
		Pass:    true, // optimistic
	}

	// 1. Fetch required-tests/<service>.yaml from qa-management
	url := fmt.Sprintf("%s/required-tests/%s.yaml", strings.TrimRight(qaMgmtRaw, "/"), rel.Name)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		v.Pass = false
		v.Reason = fmt.Sprintf("could not build request for %s: %v", url, err)
		return v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		v.Pass = false
		v.Reason = fmt.Sprintf("fetch %s: %v", url, err)
		return v
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No required-tests entry for this service → not gated. Pass through.
		v.Reason = "no required-tests entry in qa-management; not gated"
		return v
	}
	if resp.StatusCode != http.StatusOK {
		v.Pass = false
		v.Reason = fmt.Sprintf("fetch %s returned status %d", url, resp.StatusCode)
		return v
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		v.Pass = false
		v.Reason = fmt.Sprintf("read response body: %v", err)
		return v
	}

	var rt RequiredTests
	if err := yaml.Unmarshal(body, &rt); err != nil {
		v.Pass = false
		v.Reason = fmt.Sprintf("parse required-tests yaml: %v", err)
		return v
	}

	if len(rt.Required) == 0 {
		v.Reason = "qa-management entry exists but no required tests declared; not gated"
		return v
	}

	// 2. For each required test, GCS-fetch the per-test result JSON
	// Path: gs://<bucket>/<prefix>/<repo>/<sha>/<cluster>/<test_pack>/<test_name>.json
	// We use HTTPS GCS endpoint; relies on bucket being publicly readable for the spike.
	// Phase 1 hardening adds authenticated reads via service-account key.
	for _, t := range rt.Required {
		objURL := fmt.Sprintf("https://storage.googleapis.com/%s/%s/%s/%s/%s/%s/%s.json",
			bucket,
			strings.TrimRight(prefix, "/"),
			rel.Name,
			rel.Version,
			cluster,
			t.TestPack,
			t.Name,
		)
		objReq, err := http.NewRequestWithContext(ctx, "GET", objURL, nil)
		if err != nil {
			v.Pass = false
			v.MissingTests = append(v.MissingTests, t.Name)
			v.Reason = fmt.Sprintf("internal: build object request: %v", err)
			continue
		}
		objResp, err := http.DefaultClient.Do(objReq)
		if err != nil {
			v.Pass = false
			v.MissingTests = append(v.MissingTests, t.Name)
			continue
		}
		objResp.Body.Close()

		if objResp.StatusCode == http.StatusNotFound {
			v.Pass = false
			v.MissingTests = append(v.MissingTests, t.Name)
			continue
		}
		if objResp.StatusCode != http.StatusOK {
			v.Pass = false
			v.MissingTests = append(v.MissingTests, t.Name)
			continue
		}

		// Re-do with body to parse status
		objReq2, _ := http.NewRequestWithContext(ctx, "GET", objURL, nil)
		objResp2, err := http.DefaultClient.Do(objReq2)
		if err != nil {
			v.Pass = false
			v.MissingTests = append(v.MissingTests, t.Name)
			continue
		}
		objBody, _ := io.ReadAll(objResp2.Body)
		objResp2.Body.Close()

		var result ResultJSON
		if err := json.Unmarshal(objBody, &result); err != nil {
			v.Pass = false
			v.FailedTests = append(v.FailedTests, t.Name+" (malformed JSON)")
			continue
		}
		if result.Status != "Success" {
			v.Pass = false
			v.FailedTests = append(v.FailedTests, fmt.Sprintf("%s (status=%s)", t.Name, result.Status))
		}
	}

	if v.Pass {
		v.Reason = fmt.Sprintf("%d required tests passed", len(rt.Required))
	} else {
		parts := []string{}
		if len(v.MissingTests) > 0 {
			parts = append(parts, fmt.Sprintf("%d missing", len(v.MissingTests)))
		}
		if len(v.FailedTests) > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", len(v.FailedTests)))
		}
		v.Reason = strings.Join(parts, ", ")
	}
	return v
}

// renderVerdictMarkdown produces the body of the PR sticky comment.
func renderVerdictMarkdown(verdicts []ServiceVerdict, allPass bool) string {
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
		b.WriteString(fmt.Sprintf("| `%s` | `%s` | %s | %s |\n", v.Service, v.Version, icon, v.Reason))
		if len(v.MissingTests) > 0 {
			b.WriteString(fmt.Sprintf("|  |  |  | Missing: `%s` |\n", strings.Join(v.MissingTests, "`, `")))
		}
		if len(v.FailedTests) > 0 {
			b.WriteString(fmt.Sprintf("|  |  |  | Failed: `%s` |\n", strings.Join(v.FailedTests, "`, `")))
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

	// List existing comments; replace if our marker exists
	listURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s/comments", owner, name, pullNumber)
	req, err := http.NewRequestWithContext(ctx, "GET", listURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
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
		req, err := http.NewRequestWithContext(ctx, "PATCH", patchURL, strings.NewReader(string(payloadBytes)))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("PATCH comment %d returned status %d", existingID, resp.StatusCode)
		}
	} else {
		req, err := http.NewRequestWithContext(ctx, "POST", listURL, strings.NewReader(string(payloadBytes)))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
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
