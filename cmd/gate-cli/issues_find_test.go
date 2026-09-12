package main

// findOpenIssue, and specifically the fact that it must not touch the SEARCH
// API.
//
// Context: on 2026-09-11 the gate's issue lifecycle was found to have never
// worked. Every qa-gate run made ~35 GET /search/issues calls and every one
// returned 403, on both clusters, so no issue was ever found, deduped, updated
// or closed. GitHub limits search to 30 requests per MINUTE (core REST is 5000
// per HOUR), so iterating 35 services exceeded the budget by construction —
// and because both clusters' gates share one bot token, whichever ran second
// was already at zero and failed on its FIRST call.
//
// That last detail is why it was misread as a permissions problem for so long:
// there were no early successes to suggest a budget running out, and the error
// text was a bare "403 Forbidden" with GitHub's explanatory message stripped.
//
// So the load-bearing test here is TestFindOpenIssue_NeverCallsTheSearchAPI.
// The behavioural tests below would all pass just as well against the old
// search-based implementation — they describe what the function returns, not
// which endpoint it spends a scarce budget on.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testPrefix = "[leartech-gate-gcp] leartech-catalog-mcp"

// issueStub is the subset of GitHub's list-issues response we care about.
type issueStub struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	PullRequest *map[string]any `json:"pull_request,omitempty"`
}

// newGitHubStub serves GET /repos/{owner}/{repo}/issues and records every path
// it was asked for, so a test can assert on which endpoints were used.
func newGitHubStub(t *testing.T, pages map[int][]issueStub) (*IssueClient, *[]string) {
	t.Helper()
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)

		if strings.HasPrefix(r.URL.Path, "/search/") {
			// Mirror the real failure so a regression reproduces it exactly
			// rather than silently passing against a forgiving stub.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for user ID 1."}`))
			return
		}

		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			_, _ = fmt.Sscanf(p, "%d", &page)
		}
		body, err := json.Marshal(pages[page])
		if err != nil {
			t.Errorf("marshal stub page: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })

	return &IssueClient{Token: "test-token", HTTP: srv.Client()}, &seen
}

// THE LOAD-BEARING TEST. Everything else here would pass on the old
// implementation too.
func TestFindOpenIssue_NeverCallsTheSearchAPI(t *testing.T) {
	c, seen := newGitHubStub(t, map[int][]issueStub{
		1: {{Number: 2, Title: testPrefix + " failing", State: "open"}},
	})

	if _, err := c.findOpenIssue(t.Context(), "mikelear/leartech-catalog-mcp", testPrefix); err != nil {
		t.Fatalf("findOpenIssue: %v", err)
	}

	if len(*seen) == 0 {
		t.Fatal("no HTTP calls recorded — the stub was not used, so this test proves nothing")
	}
	for _, path := range *seen {
		if strings.Contains(path, "/search/") {
			t.Fatalf("findOpenIssue called the SEARCH API (%s).\n\n"+
				"Search is limited to 30 requests per MINUTE; core REST allows 5000 per HOUR. "+
				"A gate run iterates ~35 services, so search cannot work here even with a full "+
				"budget — and both clusters share one bot token. This is the exact regression "+
				"that left the issue lifecycle silently broken until 2026-09-11.", path)
		}
	}
}

func TestFindOpenIssue_FindsAnIssueMatchingThePrefix(t *testing.T) {
	c, _ := newGitHubStub(t, map[int][]issueStub{
		1: {
			{Number: 1, Title: "unrelated bug report", State: "open"},
			{Number: 7, Title: testPrefix + " failing since 0.0.9", State: "open"},
		},
	})

	got, err := c.findOpenIssue(t.Context(), "mikelear/leartech-catalog-mcp", testPrefix)
	if err != nil {
		t.Fatalf("findOpenIssue: %v", err)
	}
	if got == nil {
		t.Fatal("existing gate issue not found — the gate would open a duplicate")
	}
	if got.Number != 7 {
		t.Errorf("found issue #%d, want #7", got.Number)
	}
}

// The control. Without it, "finds the issue" would pass equally on a function
// that returned the first issue it saw.
func TestFindOpenIssue_IgnoresIssuesWithADifferentPrefix(t *testing.T) {
	c, _ := newGitHubStub(t, map[int][]issueStub{
		1: {
			{Number: 1, Title: "unrelated bug report", State: "open"},
			// Same shape, different cluster — must NOT match, or the two
			// clusters' gates would fight over one issue.
			{Number: 9, Title: "[leartech-gate-az] leartech-catalog-mcp failing", State: "open"},
		},
	})

	got, err := c.findOpenIssue(t.Context(), "mikelear/leartech-catalog-mcp", testPrefix)
	if err != nil {
		t.Fatalf("findOpenIssue: %v", err)
	}
	if got != nil {
		t.Fatalf("matched issue #%d (%q) despite a different prefix — the gcp and az "+
			"gates would overwrite each other's issues", got.Number, got.Title)
	}
}

// GitHub's list-issues endpoint returns PULL REQUESTS as well as issues. The
// search API could say `is:issue`; the list API cannot, so the filter has to be
// applied in our code — and if it is not, a PR titled like a gate issue gets
// treated as one and commented on.
func TestFindOpenIssue_SkipsPullRequests(t *testing.T) {
	pr := map[string]any{"url": "https://api.github.com/repos/x/y/pulls/5"}
	c, _ := newGitHubStub(t, map[int][]issueStub{
		1: {
			{Number: 5, Title: testPrefix + " fix attempt", State: "open", PullRequest: &pr},
			{Number: 6, Title: testPrefix + " failing", State: "open"},
		},
	})

	got, err := c.findOpenIssue(t.Context(), "mikelear/leartech-catalog-mcp", testPrefix)
	if err != nil {
		t.Fatalf("findOpenIssue: %v", err)
	}
	if got == nil {
		t.Fatal("no issue found; the non-PR issue #6 should have matched")
	}
	if got.Number == 5 {
		t.Fatal("returned PR #5 as an issue — the list API includes PRs and the " +
			"`is:issue` filter the search API gave us for free must be reapplied here")
	}
	if got.Number != 6 {
		t.Errorf("found #%d, want #6", got.Number)
	}
}

// Chart-only dependencies (auth-postgresql, auth-mongodb) are not leartech
// repos. They must read as "no issue", not as an error, or every gate run warns
// once per chart dep. Under the search API these returned 403 like everything
// else, so this branch was dead code.
func TestFindOpenIssue_NonExistentRepoIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })

	c := &IssueClient{Token: "test-token", HTTP: srv.Client()}

	got, err := c.findOpenIssue(t.Context(), "mikelear/auth-postgresql", testPrefix)
	if err != nil {
		t.Fatalf("a missing repo produced an error rather than 'no issue': %v", err)
	}
	if got != nil {
		t.Fatalf("a missing repo produced issue #%d", got.Number)
	}
}

// A full page must be followed by a request for the next one, or an existing
// issue sitting past #100 is missed and duplicated.
func TestFindOpenIssue_PaginatesPastAFullPage(t *testing.T) {
	full := make([]issueStub, listIssuesPageSize)
	for i := range full {
		full[i] = issueStub{Number: i + 1, Title: "noise", State: "open"}
	}
	c, seen := newGitHubStub(t, map[int][]issueStub{
		1: full,
		2: {{Number: 999, Title: testPrefix + " failing", State: "open"}},
	})

	got, err := c.findOpenIssue(t.Context(), "mikelear/leartech-catalog-mcp", testPrefix)
	if err != nil {
		t.Fatalf("findOpenIssue: %v", err)
	}
	if got == nil || got.Number != 999 {
		t.Fatalf("issue on page 2 not found (got %v) — a repo with >100 open issues "+
			"would get a duplicate gate issue every run", got)
	}
	if len(*seen) < 2 {
		t.Fatalf("only %d request(s) made; pagination did not happen", len(*seen))
	}
}

// And the inverse: a short page must STOP the scan. Otherwise every lookup
// costs maxIssuePages requests and the core budget goes the way of the search
// budget.
func TestFindOpenIssue_StopsOnAShortPage(t *testing.T) {
	c, seen := newGitHubStub(t, map[int][]issueStub{
		1: {{Number: 1, Title: "noise", State: "open"}},
	})

	if _, err := c.findOpenIssue(t.Context(), "mikelear/leartech-catalog-mcp", testPrefix); err != nil {
		t.Fatalf("findOpenIssue: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("made %d requests for a single short page; should stop at 1", len(*seen))
	}
}

// The error text is the diagnostic. A bare "403 Forbidden" is what sent the
// 2026-09-11 investigation after the token for hours when the token was fine —
// throttling and a missing scope share a status code and need opposite fixes.
func TestAPIError_CarriesGitHubsExplanation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for user ID 1."}`))
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })

	c := &IssueClient{Token: "test-token", HTTP: srv.Client()}

	_, err := c.findOpenIssue(t.Context(), "mikelear/leartech-catalog-mcp", testPrefix)
	if err == nil {
		t.Fatal("a 403 was swallowed")
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Fatalf("error does not say WHY it failed: %q\n\n"+
			"Throttling and a missing scope are both 403. Without GitHub's message the "+
			"next person debugging this starts by rotating a working token.", err.Error())
	}
}

// Nothing in an error may echo the credential.
func TestAPIError_NeverLeaksTheToken(t *testing.T) {
	const secret = "ghp_supersecrettokenvalue"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// A hostile or naive server reflecting the auth header back at us.
		_, _ = w.Write([]byte(`{"message":"denied","auth":"` + r.Header.Get("Authorization") + `"}`))
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })

	c := &IssueClient{Token: secret, HTTP: srv.Client()}

	_, err := c.findOpenIssue(t.Context(), "mikelear/leartech-catalog-mcp", testPrefix)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the token appears in the error text: %q", err.Error())
	}
}
