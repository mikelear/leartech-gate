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
	"net/url"
	"os"
	"strings"
	"time"
)

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
}

// NewIssueClient builds an IssueClient from the same env vars used by
// postPRCommentAndCheck. Returns nil + error if any required var missing
// — caller should treat as a soft-disable (log + continue).
func NewIssueClient(serviceRepoOwner string) (*IssueClient, error) {
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
// (service, version) verdict against the service's repo. Best-effort:
// errors are returned but the caller logs and continues; gate verdict
// is not affected by issue-API outcomes.
//
// State machine:
//   - verdict.Pass  + no open issue → noop
//   - verdict.Pass  + open issue    → close (auto-resolve)
//   - !verdict.Pass + no open issue → create
//   - !verdict.Pass + open issue    → update body (idempotent — only
//     POSTs an update comment if the reason text changed)
func (c *IssueClient) EnsureBlockingIssue(ctx context.Context, v ServiceVerdict) (IssueOutcome, error) {
	repo := c.Owner + "/" + v.Service
	titlePrefix := fmt.Sprintf("[leartech-gate] %s@%s", v.Service, v.Version)

	existing, err := c.findOpenIssue(ctx, repo, titlePrefix)
	if err != nil {
		return IssueErrored, fmt.Errorf("find existing: %w", err)
	}

	if v.Pass {
		if existing == nil {
			return IssueNoop, nil
		}
		// Auto-close on verdict flip back to green.
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

	// Existing issue — only post an update comment if the reason text
	// changed. Compare against the issue body (which carries the marker
	// + reason). Skip noisy "still failing" updates.
	if strings.Contains(existing.Body, gateBodyMarker) && bodyReasonMatches(existing.Body, v.Reason) {
		return IssueNoop, nil
	}
	updateComment := fmt.Sprintf(
		"Verdict re-evaluated by %s — still blocking. Latest reason:\n\n> %s\n",
		c.gitOpsPRLink(), v.Reason,
	)
	if err := c.commentOnIssue(ctx, repo, existing.Number, updateComment); err != nil {
		return IssueErrored, fmt.Errorf("update-comment: %w", err)
	}
	// Also patch the body so the marker carries the latest reason
	// (lets the next gate run skip if reason still matches).
	if err := c.patchIssueBody(ctx, repo, existing.Number, body); err != nil {
		return IssueErrored, fmt.Errorf("patch-body: %w", err)
	}
	return IssueUpdated, nil
}

// gateBodyMarker is embedded in the issue body so future runs can detect
// our prior content and compare reason without parsing markdown. Marker
// includes the reason hash for idempotent comparison.
const gateBodyMarker = "<!-- leartech-gate-blocking-issue -->"

func (c *IssueClient) renderIssueBody(v ServiceVerdict) string {
	var b strings.Builder
	fmt.Fprintln(&b, gateBodyMarker)
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
	fmt.Fprintf(&b, "**Blocking PR:** %s\n\n", c.gitOpsPRLink())
	fmt.Fprintln(&b, "---")
	fmt.Fprintln(&b, "_This issue is opened and managed by `leartech-gate` when a quill fails. It auto-closes when the gate reports PASS for the same `service@version`. Comment `/override leartech-gate` on the GitOps PR to bypass the gate without fixing the underlying issue._")
	return b.String()
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
type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

func (c *IssueClient) findOpenIssue(ctx context.Context, repo, titlePrefix string) (*ghIssue, error) {
	// Search restricted to the service repo + open state + our title prefix.
	q := fmt.Sprintf("repo:%s is:issue is:open in:title %s", repo, titlePrefix)
	apiURL := "https://api.github.com/search/issues?q=" + url.QueryEscape(q)
	body, err := c.getJSON(ctx, apiURL)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []ghIssue `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Items {
		if strings.HasPrefix(resp.Items[i].Title, titlePrefix) {
			return &resp.Items[i], nil
		}
	}
	return nil, nil
}

func (c *IssueClient) createIssue(ctx context.Context, repo, title, body string) (int, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/issues", repo)
	payload, _ := json.Marshal(map[string]any{"title": title, "body": body})
	resp, err := c.postJSON(ctx, apiURL, payload, http.StatusCreated)
	if err != nil {
		return 0, err
	}
	var out struct {
		Number int `json:"number"`
	}
	_ = json.Unmarshal(resp, &out)
	return out.Number, nil
}

func (c *IssueClient) commentOnIssue(ctx context.Context, repo string, number int, body string) error {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments", repo, number)
	payload, _ := json.Marshal(map[string]any{"body": body})
	_, err := c.postJSON(ctx, apiURL, payload, http.StatusCreated)
	return err
}

func (c *IssueClient) patchIssueBody(ctx context.Context, repo string, number int, body string) error {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d", repo, number)
	payload, _ := json.Marshal(map[string]any{"body": body})
	_, err := c.patchJSON(ctx, apiURL, payload, http.StatusOK)
	return err
}

func (c *IssueClient) closeIssue(ctx context.Context, repo string, number int) error {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d", repo, number)
	payload, _ := json.Marshal(map[string]any{"state": "closed"})
	_, err := c.patchJSON(ctx, apiURL, payload, http.StatusOK)
	return err
}

// HTTP helpers.

func (c *IssueClient) getJSON(ctx context.Context, apiURL string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	c.setAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s → %d: %s", apiURL, resp.StatusCode, string(body))
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
	req, _ := http.NewRequestWithContext(ctx, method, apiURL, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		return nil, fmt.Errorf("%s %s → %d: %s", method, apiURL, resp.StatusCode, string(body))
	}
	return body, nil
}

func (c *IssueClient) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}
