// Auto-issue creation on the service repo when a service's quill fails
// the gate. Mirrors the ai-reviewer cron pattern: gives owners
// visibility that they're blocking promotion to production, with a
// back-link to the GitOps PR + reason from the verdict table.
//
// Idempotent by title prefix `[leartech-gate] <service>@<version>` so
// repeated gate runs on the same PR don't spam. When the verdict flips
// to PASS for a (service, version) with an open blocking issue, the
// issue is closed automatically.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// githubAPIBase is the GitHub REST API root. Configurable via
// GITHUB_API_URL env (mirrors the gh CLI convention) for GHE deployments;
// defaults to the public endpoint.
var githubAPIBase = func() string {
	if v := os.Getenv("GITHUB_API_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.github.com"
}()

// IssueClient wraps GitHub REST calls for issue management. Reuses the
// same env vars (GITHUB_TOKEN, REPO_OWNER, REPO_NAME, PULL_NUMBER) as
// the verdict-comment poster — tekton-bot's GitHub App token already
// has issue-write scope across mikelear/* org.
type IssueClient struct {
	Token        string
	Owner        string // GitHub org for service repos (default mikelear)
	GitOpsRepo   string // owner/name of the GitOps repo (for back-link)
	GitOpsPullNo string // PR number on the GitOps repo
	HTTP         *http.Client

	// Artifact-link rendering — same path layout as the verdict
	// comment. Empty Bucket/PathTemplate ⇒ links omitted from issue
	// body (e.g. dry-run / local invocations without a result-store).
	Bucket       string
	PathTemplate string
	Cluster      string
	Namespace    string
}

// NewIssueClient builds an IssueClient from the same env vars used by
// postPRCommentAndCheck. Returns nil + error if any required var missing
// — caller should treat as a soft-disable (log + continue).
//
// bucket/pathTemplate/cluster/namespace mirror the verdict-comment
// renderer args; passed through so issue bodies carry the same
// per-failed-pack artifact links (HTML report + trace.zip listing).
// Empty bucket or template ⇒ links silently omitted.
func NewIssueClient(serviceRepoOwner, bucket, pathTemplate, cluster, namespace string) (*IssueClient, error) {
	token := os.Getenv("GITHUB_TOKEN")
	gitOpsOwner := os.Getenv("REPO_OWNER")
	gitOpsRepo := os.Getenv("REPO_NAME")
	pullNo := os.Getenv("PULL_NUMBER")
	if token == "" || gitOpsOwner == "" || gitOpsRepo == "" || pullNo == "" {
		return nil, fmt.Errorf("missing GITHUB_TOKEN / REPO_OWNER / REPO_NAME / PULL_NUMBER")
	}
	if serviceRepoOwner == "" {
		serviceRepoOwner = gitOpsOwner
	}
	return &IssueClient{
		Token:        token,
		Owner:        serviceRepoOwner,
		GitOpsRepo:   gitOpsOwner + "/" + gitOpsRepo,
		GitOpsPullNo: pullNo,
		HTTP:         &http.Client{Timeout: 15 * time.Second},
		Bucket:       bucket,
		PathTemplate: pathTemplate,
		Cluster:      cluster,
		Namespace:    namespace,
	}, nil
}

// IssueOutcome reports what ensureBlockingIssue did for telemetry.
type IssueOutcome int

const (
	// IssueNoop = nothing to do (verdict pass, no existing issue, etc.)
	IssueNoop IssueOutcome = iota
	IssueCreated
	IssueUpdated
	IssueClosed
	IssueErrored
)

func (o IssueOutcome) String() string {
	switch o {
	case IssueNoop:
		return "noop"
	case IssueCreated:
		return "created"
	case IssueUpdated:
		return "updated"
	case IssueClosed:
		return "closed"
	default:
		return "errored"
	}
}

// EnsureBlockingIssue handles the open/update/close lifecycle for one
// service's verdict against its repo. Best-effort: errors are returned
// but the caller logs and continues; gate verdict is not affected by
// issue-API outcomes.
//
// One issue per service per repo PER CLUSTER. Title is version-agnostic
// but cluster-suffixed (`[leartech-gate-<cluster>] <service> blocking
// promotion to production`) so each cluster manages its own lifecycle
// independently — without the suffix, GCP's PASS verdict could close
// AZ's blocking issue (or vice-versa) when the same service genuinely
// fails on only one cluster. Mirrors the verdict-comment marker pattern
// (`<!-- leartech-gate-gcp -->`).
//
// State machine (per cluster):
//   - verdict.Pass  + no open issue → noop
//   - verdict.Pass  + open issue    → close (auto-resolve)
//   - !verdict.Pass + no open issue → create
//   - !verdict.Pass + open issue    → update body (idempotent — only
//     POSTs an update comment if reason or version changed)
func (c *IssueClient) EnsureBlockingIssue(ctx context.Context, v ServiceVerdict) (IssueOutcome, error) {
	repo := c.Owner + "/" + v.Service
	titlePrefix := c.titlePrefixFor(v.Service)

	existing, err := c.findOpenIssue(ctx, repo, titlePrefix)
	if err != nil {
		return IssueErrored, fmt.Errorf("find existing: %w", err)
	}

	if v.Pass {
		if existing == nil {
			return IssueNoop, nil
		}
		// Auto-close on verdict flip back to green. The issue body
		// will reference the failing version; close comment names the
		// passing version that resolved it for clarity.
		closeBody := fmt.Sprintf(
			"Resolved by %s — gate now reports `%s@%s` as PASS. Closing automatically.",
			c.gitOpsPRLink(), v.Service, v.Version,
		)
		if err := c.commentOnIssue(ctx, repo, existing.Number, closeBody); err != nil {
			return IssueErrored, fmt.Errorf("comment-on-close: %w", err)
		}
		if err := c.closeIssue(ctx, repo, existing.Number); err != nil {
			return IssueErrored, fmt.Errorf("close: %w", err)
		}
		return IssueClosed, nil
	}

	// Verdict failing.
	body := c.renderIssueBody(v)
	if existing == nil {
		title := fmt.Sprintf("%s blocking promotion to production", titlePrefix)
		num, err := c.createIssue(ctx, repo, title, body)
		if err != nil {
			return IssueErrored, fmt.Errorf("create: %w", err)
		}
		_ = num
		return IssueCreated, nil
	}

	// Existing issue — only post an update comment if the body
	// (containing reason + version) actually changed. Skip noisy
	// "still failing" updates when nothing's different from last run.
	// Match either the cluster-suffixed marker (current bodies) or the
	// legacy unsuffixed marker (bodies written by older gate-cli before
	// per-cluster isolation landed).
	hasMarker := strings.Contains(existing.Body, c.bodyMarkerFor()) ||
		strings.Contains(existing.Body, gateBodyMarker)
	if hasMarker &&
		bodyReasonMatches(existing.Body, v.Reason) &&
		strings.Contains(existing.Body, "@"+v.Version+"`") {
		return IssueNoop, nil
	}
	updateComment := fmt.Sprintf(
		"Verdict re-evaluated by %s for `%s@%s` — still blocking. Latest reason:\n\n> %s\n",
		c.gitOpsPRLink(), v.Service, v.Version, v.Reason,
	)
	if err := c.commentOnIssue(ctx, repo, existing.Number, updateComment); err != nil {
		return IssueErrored, fmt.Errorf("update-comment: %w", err)
	}
	// Also patch the body so the marker carries the latest version+reason
	// (lets the next gate run skip if both still match).
	if err := c.patchIssueBody(ctx, repo, existing.Number, body); err != nil {
		return IssueErrored, fmt.Errorf("patch-body: %w", err)
	}
	return IssueUpdated, nil
}

// titlePrefixFor returns the per-cluster issue title prefix. Cluster
// suffix isolates each cluster's lifecycle — without it, one cluster's
// PASS verdict would close the other cluster's still-failing blocking
// issue (verified failure mode 2026-05-10). Falls back to the legacy
// suffix-less prefix when cluster is empty (older invocations / tests).
func (c *IssueClient) titlePrefixFor(service string) string {
	if c.Cluster == "" {
		return fmt.Sprintf("[leartech-gate] %s", service)
	}
	return fmt.Sprintf("[leartech-gate-%s] %s", c.Cluster, service)
}

// bodyMarkerFor mirrors titlePrefixFor for the body marker — same
// per-cluster isolation. Used by the idempotency check so a body
// containing one cluster's marker isn't matched by the other cluster's
// EnsureBlockingIssue call.
func (c *IssueClient) bodyMarkerFor() string {
	if c.Cluster == "" {
		return "<!-- leartech-gate-blocking-issue -->"
	}
	return fmt.Sprintf("<!-- leartech-gate-blocking-issue-%s -->", c.Cluster)
}

// gateBodyMarker is the legacy cluster-agnostic marker — kept as a
// constant so existing-issue migration logic can detect bodies created
// by older gate-cli versions. New bodies use bodyMarkerFor() with the
// cluster suffix.
const gateBodyMarker = "<!-- leartech-gate-blocking-issue -->"

func (c *IssueClient) renderIssueBody(v ServiceVerdict) string {
	var b strings.Builder
	fmt.Fprintln(&b, c.bodyMarkerFor())
	fmt.Fprintf(&b, "## :x: `%s@%s` is blocking promotion to production\n\n", v.Service, v.Version)
	fmt.Fprintf(&b, "**Verdict reason:**\n\n> %s\n\n", v.Reason)
	if len(v.FailedTests) > 0 {
		fmt.Fprintln(&b, "**Failed tests:**")
		fmt.Fprintln(&b)
		for _, t := range v.FailedTests {
			fmt.Fprintf(&b, "- `%s`\n", t)
		}
		fmt.Fprintln(&b)
	}
	if len(v.MissingTests) > 0 {
		fmt.Fprintln(&b, "**Missing required tests (not run / not uploaded):**")
		fmt.Fprintln(&b)
		for _, t := range v.MissingTests {
			fmt.Fprintf(&b, "- `%s`\n", t)
		}
		fmt.Fprintln(&b)
	}
	// Per-failed-pack artifact links — same content as the verdict
	// comment but rendered into the issue body so service-repo owners
	// can drill into failures without round-tripping to the GitOps PR.
	c.appendArtifactLinks(&b, v)
	fmt.Fprintf(&b, "**Blocking PR:** %s\n\n", c.gitOpsPRLink())
	fmt.Fprintln(&b, "---")
	fmt.Fprintln(&b, "_This issue is opened and managed by `leartech-gate` when a quill fails. It auto-closes when the gate reports PASS for the same `service`. Comment `/override leartech-gate` on the GitOps PR to bypass the gate without fixing the underlying issue._")
	return b.String()
}

// appendArtifactLinks writes a section linking to the Playwright HTML
// report + GCS test-results listing for each failed pack. Silently
// no-ops when the IssueClient wasn't configured with a bucket/template
// (e.g. in tests, or dry-run mode).
func (c *IssueClient) appendArtifactLinks(b *strings.Builder, v ServiceVerdict) {
	if c.Bucket == "" || c.PathTemplate == "" || len(v.FailedPacks) == 0 {
		return
	}
	rendered := false
	for _, pack := range v.FailedPacks {
		prefix, err := renderPostDeployPathPrefix(c.PathTemplate, pathVars{
			Cluster: c.Cluster, Namespace: c.Namespace,
			Service: v.Service, Version: v.Version, Pack: pack,
		})
		if err != nil || prefix == "" {
			continue
		}
		reportURL := renderPlaywrightReportURL(c.Bucket, prefix)
		listingURL := renderTestResultsListingURL(c.Bucket, prefix)
		if reportURL == "" {
			continue
		}
		if !rendered {
			fmt.Fprintln(b, "**Artifacts** (per failed pack):")
			fmt.Fprintln(b)
			rendered = true
		}
		fmt.Fprintf(b, "- `%s`: [HTML report](%s)", pack, reportURL)
		if listingURL != "" {
			fmt.Fprintf(b, " · [trace.zip listing](%s)", listingURL)
		}
		fmt.Fprintln(b)
	}
	if rendered {
		fmt.Fprintln(b)
		fmt.Fprintln(b, "_Open trace.zip files in https://trace.playwright.dev/ for an interactive timeline of each failure._")
		fmt.Fprintln(b)
	}
}

func (c *IssueClient) gitOpsPRLink() string {
	return fmt.Sprintf("https://github.com/%s/pull/%s", c.GitOpsRepo, c.GitOpsPullNo)
}

// bodyReasonMatches checks whether the existing issue body already
// references the same reason text — used to suppress duplicate "still
// failing" updates.
func bodyReasonMatches(existingBody, reason string) bool {
	// Naive but effective: render block contains the verdict text. Reason
	// is short enough that exact match works.
	return strings.Contains(existingBody, "> "+reason+"\n")
}

// minimal issue shape for our needs.
//
// PullRequest is present ONLY on pull requests: GitHub's list-issues endpoint
// returns PRs alongside issues, and the only reliable discriminator is the
// presence of this object. The search API this code used to call could say
// `is:issue`; the list API cannot, so the filtering moves here.
type ghIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	PullRequest *map[string]any `json:"pull_request,omitempty"`
}

func (i ghIssue) isPullRequest() bool { return i.PullRequest != nil }

// listIssuesPageSize is GitHub's maximum for the list-issues endpoint.
const listIssuesPageSize = 100

// maxIssuePages bounds pagination. 10 pages = 1000 open issues in one repo; if
// a repo ever exceeds that, findOpenIssue logs rather than silently returning
// "no existing issue" and creating a duplicate.
const maxIssuePages = 10

// findOpenIssue looks for an open issue in `repo` whose title starts with
// titlePrefix.
//
// THIS USES THE LIST API, NOT THE SEARCH API, AND THAT IS THE WHOLE POINT.
//
// It used to call GET /search/issues with `repo:… is:issue is:open in:title …`.
// Measured 2026-09-11 on both clusters: every one of the 35 calls a qa-gate run
// makes returned 403, on every run, so the issue lifecycle had never worked —
// no issue was ever found, updated, deduped or closed.
//
// The cause is not the token. LeartechKeeperBot's PAT carries `repo` and the
// same request returns 200 when made alone. GitHub rate-limits SEARCH at
//
//	x-ratelimit-limit: 30    per MINUTE
//
// while the core REST API allows 5000 per HOUR. The gate iterates ~35 services
// per run, so it exceeded the search budget by construction even with a full
// allowance — and both clusters' gates share this one bot token, so whichever
// ran second got 403 on its FIRST call. That is why the failure looked like a
// permissions problem rather than throttling: there were no early successes to
// suggest a budget running out.
//
// GET /repos/{owner}/{repo}/issues is on the core limit, which makes 35 calls
// per run a rounding error instead of 117% of the budget.
//
// Two behaviours have to be reproduced by hand because the list API cannot
// express them as query terms:
//
//   - `is:issue` — the list endpoint returns pull requests too, filtered via
//     isPullRequest() below.
//   - `in:title <prefix>` — matched client-side, which this function already
//     did anyway because the search API's title matching is fuzzy.
//
// A non-existent repo (chart-only deps like auth-postgresql / auth-mongodb)
// returns 404 here, which the caller already treats as "no issue". Under the
// search API those returned 403 like everything else, so the 404 branch was
// dead code and every chart dep produced a warning too.
func (c *IssueClient) findOpenIssue(ctx context.Context, repo, titlePrefix string) (*ghIssue, error) {
	for page := 1; page <= maxIssuePages; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/issues?state=open&per_page=%d&page=%d",
			githubAPIBase, repo, listIssuesPageSize, page)

		body, err := c.getJSON(ctx, apiURL)
		if err != nil {
			// Repo doesn't exist (chart-only deps that aren't real leartech
			// repos). Treat as "no issue" rather than an error so we don't spam
			// warnings on every gate run for every chart dep.
			if strings.Contains(err.Error(), "→ 404") || strings.Contains(err.Error(), "→ 422") {
				return nil, nil
			}
			return nil, err
		}

		var items []ghIssue
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("parse issue list for %s: %w", repo, err)
		}

		for i := range items {
			if items[i].isPullRequest() {
				continue
			}
			if strings.HasPrefix(items[i].Title, titlePrefix) {
				return &items[i], nil
			}
		}

		// Short page means last page.
		if len(items) < listIssuesPageSize {
			return nil, nil
		}
	}

	// Bounded scan exhausted. Say so: silently returning nil here would create a
	// duplicate issue on every run for a repo this busy.
	return nil, fmt.Errorf("scanned %d pages (%d open issues) of %s without finding %q and more remain; "+
		"raise maxIssuePages or label gate issues so they can be filtered server-side",
		maxIssuePages, maxIssuePages*listIssuesPageSize, repo, titlePrefix)
}

func (c *IssueClient) createIssue(ctx context.Context, repo, title, body string) (int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/issues", githubAPIBase, repo)
	payload, err := json.Marshal(map[string]any{"title": title, "body": body})
	if err != nil {
		return 0, fmt.Errorf("marshal issue payload: %w", err)
	}
	resp, err := c.postJSON(ctx, apiURL, payload, http.StatusCreated)
	if err != nil {
		return 0, err
	}
	var out struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return 0, fmt.Errorf("parse create-issue response: %w", err)
	}
	return out.Number, nil
}

func (c *IssueClient) commentOnIssue(ctx context.Context, repo string, number int, body string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/issues/%d/comments", githubAPIBase, repo, number)
	payload, err := json.Marshal(map[string]any{"body": body})
	if err != nil {
		return fmt.Errorf("marshal comment payload: %w", err)
	}
	_, err = c.postJSON(ctx, apiURL, payload, http.StatusCreated)
	return err
}

func (c *IssueClient) patchIssueBody(ctx context.Context, repo string, number int, body string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/issues/%d", githubAPIBase, repo, number)
	payload, err := json.Marshal(map[string]any{"body": body})
	if err != nil {
		return fmt.Errorf("marshal patch payload: %w", err)
	}
	_, err = c.patchJSON(ctx, apiURL, payload, http.StatusOK)
	return err
}

func (c *IssueClient) closeIssue(ctx context.Context, repo string, number int) error {
	apiURL := fmt.Sprintf("%s/repos/%s/issues/%d", githubAPIBase, repo, number)
	payload, err := json.Marshal(map[string]any{"state": "closed"})
	if err != nil {
		return fmt.Errorf("marshal close payload: %w", err)
	}
	_, err = c.patchJSON(ctx, apiURL, payload, http.StatusOK)
	return err
}

// HTTP helpers.

// githubMessage extracts GitHub's `message` field for error text, formatted as
// a parenthetical suffix. Returns "" if absent or unparseable, so a malformed
// error response degrades to bare status text rather than failing the caller.
func githubMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &e); err != nil || strings.TrimSpace(e.Message) == "" {
		return ""
	}
	return " (" + e.Message + ")"
}

func (c *IssueClient) getJSON(ctx context.Context, apiURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.setAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Include GitHub's `message` field ONLY — never the raw body or the
		// headers. The original version omitted everything, on the reasoning
		// that status text suffices and that echoing the body risks leaking
		// Authorization reflections or rate-limit headers. The first half of
		// that was wrong and it cost real time: a 403 from THROTTLING and a 403
		// from a MISSING SCOPE are the same status text and need opposite
		// fixes, so "403 Forbidden" alone sent the 2026-09-11 investigation
		// after the token when the token was fine. GitHub says
		// "API rate limit exceeded for ..." vs "Resource not accessible by ...",
		// which resolves it immediately.
		//
		// `message` is a fixed, human-readable field; it never contains
		// credentials, and parsing just that key means a future GitHub response
		// shape cannot widen what gets logged.
		return nil, fmt.Errorf("GET %s → %d %s%s",
			apiURL, resp.StatusCode, http.StatusText(resp.StatusCode), githubMessage(body))
	}
	return body, nil
}

func (c *IssueClient) postJSON(ctx context.Context, apiURL string, payload []byte, wantStatus int) ([]byte, error) {
	return c.bodyJSON(ctx, "POST", apiURL, payload, wantStatus)
}

func (c *IssueClient) patchJSON(ctx context.Context, apiURL string, payload []byte, wantStatus int) ([]byte, error) {
	return c.bodyJSON(ctx, "PATCH", apiURL, payload, wantStatus)
}

func (c *IssueClient) bodyJSON(ctx context.Context, method, apiURL string, payload []byte, wantStatus int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != wantStatus {
		// Same body-omission policy as getJSON above.
		return nil, fmt.Errorf("%s %s → %d %s", method, apiURL, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return body, nil
}

func (c *IssueClient) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}
