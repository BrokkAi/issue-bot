package issuebot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/runner"
	"github.com/BrokkAi/issue-bot/internal/osrun"
)

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := osrun.Run(context.Background(), dir, map[string]string{"GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.test", "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.test"}, append([]string{"git"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

type fakeSource struct {
	mu          sync.Mutex
	notes       map[int][]issueComment
	links       map[int]*LinkedPull
	nextComment int64
	failEdit    bool
	failPost    bool
	items       []Issue
	prs         map[int]*PullRequest
	creates     int
	failCreate  bool
	cfg         Config
	root        string
}

func (f *fakeSource) issues(context.Context) ([]Issue, error) { return f.items, nil }
func (f *fakeSource) issue(_ context.Context, n int) (Issue, error) {
	for _, i := range f.items {
		if i.Number == n {
			return i, nil
		}
	}
	return Issue{}, errors.New("missing issue")
}
func (f *fakeSource) pull(_ context.Context, j *Job) (*PullRequest, error) {
	return f.prs[j.Issue.Number], nil
}
func (f *fakeSource) create(ctx context.Context, j *Job) (*PullRequest, error) {
	f.creates++
	head, err := (checkout{f.cfg}).git(ctx, "rev-parse", "refs/heads/"+j.Branch)
	if err != nil {
		return nil, err
	}
	p := &PullRequest{Number: j.Issue.Number + 100, URL: "https://github.com/o/r/pull/101", State: "open", Body: marker(f.cfg, j)}
	p.Head.Ref = j.Branch
	p.Head.SHA = head
	p.Head.Repo.FullName = f.cfg.GitHubRepo()
	p.Base.Ref = f.cfg.Branch
	p.Base.Repo.FullName = f.cfg.GitHubRepo()
	f.prs[j.Issue.Number] = p
	if f.failCreate {
		return nil, errors.New("connection lost after PR creation")
	}
	return p, nil
}

type scriptedAgent func(context.Context, string) (Result, error)

func (a scriptedAgent) Execute(ctx context.Context, p string) (Result, error) { return a(ctx, p) }

type fixture struct {
	e      engine
	s      *State
	source *fakeSource
	calls  int
	t      *testing.T
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := canonicalTestDir(t)
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	localGit(t, root, "init", "--bare", "--initial-branch=main", remote)
	localGit(t, root, "clone", remote, seed)
	localGit(t, seed, "config", "user.name", "Fixture")
	localGit(t, seed, "config", "user.email", "fixture@example.test")
	writeTestFile(t, filepath.Join(seed, "README.md"), "fixture\n")
	localGit(t, seed, "add", ".")
	localGit(t, seed, "commit", "-m", "Initial")
	localGit(t, seed, "push", "origin", "main")
	cfg := DefaultConfig()
	cfg.Remote = remote
	cfg.Branch = "main"
	cfg.Directory = filepath.Join(root, "checkout")
	cfg.StateDirectory = filepath.Join(root, "state")
	cfg.GitHub.Repo = "o/r"
	if err := os.MkdirAll(cfg.StateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, s: &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Repo: cfg.GitHubRepo(), Host: cfg.GitHub.Host, Jobs: map[int]*Job{}}}
	f.source = &fakeSource{cfg: cfg, items: []Issue{{Number: 1, Title: "First", State: "open"}, {Number: 2, Title: "Second", State: "open"}}, prs: map[int]*PullRequest{}, notes: map[int][]issueComment{}, links: map[int]*LinkedPull{}}
	f.e = engine{config: cfg, source: f.source, log: slog.New(slog.NewTextHandler(io.Discard, nil)), wait: func(context.Context, time.Duration) error { return nil }, now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	f.e.agent = func(c Config) Agent {
		return scriptedAgent(func(ctx context.Context, p string) (Result, error) {
			f.calls++
			localGit(t, c.Directory, "config", "user.name", "Fixture")
			localGit(t, c.Directory, "config", "user.email", "fixture@example.test")
			writeTestFile(t, filepath.Join(c.Directory, "fix.txt"), "fixed\n")
			localGit(t, c.Directory, "add", ".")
			localGit(t, c.Directory, "commit", "-m", "Fix issue")
			return Result{Status: "solved", Title: "Fix issue", Detail: "Fixed the bug", Tests: []string{"fixture check passed"}}, nil
		})
	}
	return f
}
func TestWalkCreatesPRsAndPreservesBase(t *testing.T) {
	f := newFixture(t)
	for range 2 {
		worked, err := f.e.step(context.Background(), f.s)
		if err != nil || !worked {
			t.Fatalf("step: %v %v", worked, err)
		}
	}
	if f.calls != 2 || f.source.creates != 2 || f.s.Jobs[1].Status != "submitted" || f.s.Jobs[2].Status != "submitted" {
		t.Fatalf("unexpected state %+v", f.s)
	}
	if worked, err := f.e.step(context.Background(), f.s); err != nil || worked {
		t.Fatalf("repeated work %v %v", worked, err)
	}
	if got := localGit(t, f.e.config.Directory, "show", "origin/main:README.md"); got != "fixture" {
		t.Fatal(got)
	}
	if _, err := ReadState(f.e.config); err != nil {
		t.Fatal(err)
	}
}
func TestLostPRResponseReconcilesAfterRestartAtBudgetLimit(t *testing.T) {
	f := newFixture(t)
	f.source.items = f.source.items[:1]
	f.source.failCreate = true
	f.e.config.Attempts = 1
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
	saved, err := ReadState(f.e.config)
	if err != nil {
		t.Fatal(err)
	}
	f.source.items = nil // issue may already have been closed by the time of restart
	if _, err := f.e.step(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if saved.Jobs[1].Status != "submitted" || f.calls != 1 || f.source.creates != 1 {
		t.Fatal("recovery duplicated work")
	}
}
func TestBlockedIssueDoesNotStarveNextIssueAndRetryRetainsWork(t *testing.T) {
	f := newFixture(t)
	normal := f.e.agent
	f.e.agent = func(c Config) Agent {
		return scriptedAgent(func(context.Context, string) (Result, error) {
			writeTestFile(t, filepath.Join(c.Directory, "unfinished.txt"), "keep me")
			return Result{Status: "blocked", Detail: "Needs clarification"}, nil
		})
	}
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
	f.e.agent = normal
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
	if f.s.Jobs[1].Status != "blocked" || f.s.Jobs[2].Status != "submitted" {
		t.Fatal("blocked issue starved queue")
	}
	cfg := f.e.config
	cfg.Issue = 1
	if err := Retry(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := ReadState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Jobs[1].Status != "pending" || saved.Jobs[1].Tries != 0 {
		t.Fatal("retry did not reset")
	}
	data, err := os.ReadFile(filepath.Join((checkout{cfg}).workdir(saved.Jobs[1]), "unfinished.txt"))
	if err != nil || string(data) != "keep me" {
		t.Fatal("retry lost work")
	}
}
func TestSetupFailureDoesNotSpendAttempt(t *testing.T) {
	f := newFixture(t)
	f.e.agent = func(Config) Agent {
		return scriptedAgent(func(context.Context, string) (Result, error) {
			return Result{}, &runner.SetupError{Err: errors.New("bad model")}
		})
	}
	if _, err := f.e.step(context.Background(), f.s); err == nil {
		t.Fatal("setup should stop daemon")
	}
	if f.s.Jobs[1].Tries != 0 {
		t.Fatal("spent attempt before prompting")
	}
}
func TestVerificationFailureDoesNotPush(t *testing.T) {
	f := newFixture(t)
	f.e.config.Verify = []string{"sh", "-c", "exit 1"}
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
	if f.source.creates != 0 || !strings.Contains(f.s.Jobs[1].Failure, "operator verification") {
		t.Fatal("published unverified work")
	}
	refs := localGit(t, f.e.config.Directory, "ls-remote", "--heads", "origin", "refs/heads/issue-bot/*")
	if refs != "" {
		t.Fatal("pushed before verification")
	}
}
func TestLocksAndStateIdentity(t *testing.T) {
	f := newFixture(t)
	unlock, err := lockConfig(f.e.config)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := lockConfig(f.e.config); err == nil {
		t.Fatal("concurrent daemon lock accepted")
	}
	if err := writeState(f.e.config, f.s); err != nil {
		t.Fatal(err)
	}
	cfg := f.e.config
	cfg.GitHub.Repo = "different/repo"
	if _, err := ReadState(cfg); err == nil {
		t.Fatal("state accepted for another repository")
	}
}

func (f *fakeSource) linkedPull(_ context.Context, n int) (*LinkedPull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.links[n], nil
}
func (f *fakeSource) comments(_ context.Context, n int) ([]issueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]issueComment(nil), f.notes[n]...), nil
}
func (f *fakeSource) postComment(_ context.Context, n int, body string) (issueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextComment++
	c := issueComment{ID: f.nextComment, Body: body}
	f.notes[n] = append(f.notes[n], c)
	if f.failPost {
		return issueComment{}, errors.New("lost comment response")
	}
	return c, nil
}
func (f *fakeSource) editComment(_ context.Context, id int64, body string) (issueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEdit {
		return issueComment{}, errors.New("comment edit unavailable")
	}
	for n, notes := range f.notes {
		for i, c := range notes {
			if c.ID == id {
				c.Body = body
				f.notes[n][i] = c
				return c, nil
			}
		}
	}
	return issueComment{}, errors.New("comment not found")
}
