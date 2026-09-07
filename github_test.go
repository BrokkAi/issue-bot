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
