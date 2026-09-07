package issuebot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Exercise the actual gh argument construction and JSON pagination over a real
// subprocess, without network access or GitHub credentials.
func TestGitHubPeer(t *testing.T) {
	if os.Getenv("ISSUE_BOT_GH_PEER") != "1" {
		return
	}
	args := os.Args
	endpoint := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "repos/") {
			endpoint = arg
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		os.Exit(11)
	}
	if u.Query().Get("labels") != "bug,help wanted" {
		fmt.Fprintln(os.Stderr, "incorrect label query", u.RawQuery)
		os.Exit(12)
	}
	page, _ := strconv.Atoi(u.Query().Get("page"))
	var issues []Issue
	if page == 1 {
		for n := 1; n <= 100; n++ {
			issues = append(issues, Issue{Number: n, State: "open"})
		}
		issues[0].PullRequest = json.RawMessage(`{}`)
	} else if page == 2 {
		issues = []Issue{{Number: 101, State: "open"}}
	} else {
		os.Exit(13)
	}
	if err := json.NewEncoder(os.Stdout).Encode(issues); err != nil {
		os.Exit(14)
	}
	os.Exit(0)
}
func TestGitHubPaginationExcludesPullRequests(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestGitHubPeer$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("ISSUE_BOT_GH_PEER", "1")
	cfg := DefaultConfig()
	cfg.GitHub.Repo = "o/r"
	cfg.Labels = []string{"bug", "help wanted"}
	items, err := (githubClient{cfg}).issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 100 || items[0].Number != 2 || items[len(items)-1].Number != 101 {
		t.Fatalf("incomplete or contaminated issue list: %v", items)
	}
}
func TestPullRequestIdentity(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GitHub.Repo = "o/r"
	j := &Job{Issue: Issue{Number: 7}, Branch: branchName(cfg, 7)}
	p := &PullRequest{Number: 8, URL: "https://github.com/o/r/pull/8", Body: marker(cfg, j)}
	p.Head.Ref = j.Branch
	p.Head.Repo.FullName = "o/r"
	p.Base.Ref = cfg.Branch
	p.Base.Repo.FullName = "o/r"
	if err := validatePull(cfg, j, p); err != nil {
		t.Fatal(err)
	}
	p.Head.Repo.FullName = "someone-else/r"
	if validatePull(cfg, j, p) == nil {
		t.Fatal("foreign PR accepted")
	}
	p.Head.Repo.FullName = "o/r"
	p.Body = "unrelated PR"
	if validatePull(cfg, j, p) == nil {
		t.Fatal("unrelated issue accepted")
	}
}

func TestCoordinationPeer(t *testing.T) {
	mode := os.Getenv("ISSUE_BOT_COORDINATION_PEER")
	if mode == "" {
		return
	}
	endpoint := ""
	values := map[string]string{}
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "repos/") || arg == "graphql" {
			endpoint = arg
		}
		if k, v, ok := strings.Cut(arg, "="); ok {
			values[k] = v
		}
	}
	var response any
	if mode == "comments" {
		parsed, _ := url.Parse(endpoint)
		if parsed.Query().Get("page") == "1" {
			comments := make([]issueComment, 100)
			for i := range comments {
				comments[i] = issueComment{ID: int64(i + 1), Body: "ordinary comment"}
			}
			response = comments
		} else if parsed.Query().Get("page") == "2" {
			response = []issueComment{{ID: 101, Body: "last comment"}}
		} else {
			if values["body"] != "First line\n\nSecond line" {
				os.Exit(11)
			}
			response = issueComment{ID: 102, Body: values["body"]}
		}
	} else if mode == "linked-error" {
		response = map[string]any{"errors": []any{map[string]string{"message": "permission denied"}}}
	} else {
		nodes := []any{}
		pageInfo := map[string]any{"hasNextPage": true, "endCursor": "page-two"}
		if values["cursor"] == "page-two" {
			nodes = []any{map[string]any{"source": LinkedPull{Number: 9, URL: "https://github.com/fork/repo/pull/9", State: "OPEN"}}}
			pageInfo = map[string]any{"hasNextPage": false, "endCursor": "end"}
		}
		closed := []LinkedPull{}
		if mode == "linked-direct" {
			closed = []LinkedPull{{Number: 12, URL: "https://github.com/o/r/pull/12", State: "CLOSED"}}
		}
		response = map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"closedByPullRequestsReferences": map[string]any{"nodes": closed}, "timelineItems": map[string]any{"nodes": nodes, "pageInfo": pageInfo}}}}}
	}
	if json.NewEncoder(os.Stdout).Encode(response) != nil {
		os.Exit(12)
	}
	os.Exit(0)
}
func coordinationClient(t *testing.T, mode string) githubClient {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestCoordinationPeer$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("ISSUE_BOT_COORDINATION_PEER", mode)
	cfg := DefaultConfig()
	cfg.GitHub.Repo = "o/r"
	return githubClient{cfg}
}
func TestGitHubCommentsPaginateAndPreserveBodies(t *testing.T) {
	g := coordinationClient(t, "comments")
	comments, err := g.comments(context.Background(), 7)
	if err != nil || len(comments) != 101 {
		t.Fatalf("%d comments, %v", len(comments), err)
	}
	body := "First line\n\nSecond line"
	c, err := g.postComment(context.Background(), 7, body)
	if err != nil || c.ID != 102 || c.Body != body {
		t.Fatalf("POST %+v %v", c, err)
	}
	c, err = g.editComment(context.Background(), 102, body)
	if err != nil || c.Body != body {
		t.Fatalf("PATCH %+v %v", c, err)
	}
}
func TestLinkedPRLookupIncludesTimelineAndManualLinks(t *testing.T) {
	for _, mode := range []string{"linked", "linked-direct", "linked-error"} {
		t.Run(mode, func(t *testing.T) {
			g := coordinationClient(t, mode)
			p, err := g.linkedPull(context.Background(), 7)
			if mode == "linked-error" {
				if err == nil {
					t.Fatal("GraphQL errors ignored")
				}
				return
			}
			if err != nil || p == nil {
				t.Fatalf("missing PR: %v", err)
			}
			expected := 9
			if mode == "linked-direct" {
				expected = 12
			}
			if p.Number != expected {
				t.Fatalf("wrong PR %+v", p)
			}
		})
	}
}
