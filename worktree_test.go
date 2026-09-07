package issuebot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateWorktreesLeaveSourceAndOtherIssuesAlone(t *testing.T) {
	source, _ := discoveryRepo(t)
	other := filepath.Join(canonicalTestDir(t), "other-bot")
	localGit(t, source, "worktree", "add", "-b", "other-bot", other, "main")
	writeTestFile(t, filepath.Join(source, "README.md"), "staged user edits")
	localGit(t, source, "add", "README.md")
	writeTestFile(t, filepath.Join(source, "README.md"), "unstaged user edits")
	writeTestFile(t, filepath.Join(other, "README.md"), "another bot's edits")
	refs := localGit(t, source, "show-ref")
	worktrees := localGit(t, source, "worktree", "list", "--porcelain")
	index := localGit(t, source, "diff", "--cached")
	config := localGit(t, source, "config", "--local", "--list")
	checkSource := func() {
		t.Helper()
		if localGit(t, source, "show-ref") != refs || localGit(t, source, "worktree", "list", "--porcelain") != worktrees || localGit(t, source, "diff", "--cached") != index || localGit(t, source, "config", "--local", "--list") != config {
			t.Fatal("bot changed source repository metadata")
		}
	}
	t.Setenv("XDG_STATE_HOME", canonicalTestDir(t))
	cfg, err := Discover(context.Background(), other, "")
	if err != nil {
		t.Fatal(err)
	}
	g := checkout{cfg}
	ctx := context.Background()
	if err := g.open(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(cfg.Directory, ".git"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("management checkout is not a linked worktree: %v %v", info, err)
	}
	if common := localGit(t, cfg.Directory, "rev-parse", "--path-format=absolute", "--git-common-dir"); common != g.repositoryDirectory() {
		t.Fatalf("Git storage is not private: %s", common)
	}
	if branch := localGit(t, cfg.Directory, "branch", "--show-current"); branch != "" {
		t.Fatal("management worktree should be detached")
	}
	base := localGit(t, cfg.Directory, "rev-parse", "HEAD")
	j1 := &Job{Issue: Issue{Number: 1}, Branch: branchName(cfg, 1), Base: base}
	first, err := g.prepare(ctx, j1)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.config.Directory, "first-fix"), "issue one's fix")
	localGit(t, first.config.Directory, "add", ".")
	localGit(t, first.config.Directory, "commit", "-m", "First issue")
	localGit(t, first.config.Directory, "tag", "bot-only-tag")
	localGit(t, first.config.Directory, "config", "user.name", "Issue bot")
	writeTestFile(t, filepath.Join(first.config.Directory, "unfinished"), "keep first issue work")
	checkSource()

	// A later issue must start from the latest remote, not an earlier issue's branch.
	localGit(t, source, "commit", "-m", "Publish staged remote change")
	localGit(t, source, "push", "origin", "HEAD:main")
	remote := localGit(t, source, "rev-parse", "HEAD")
	refs = localGit(t, source, "show-ref")
	worktrees = localGit(t, source, "worktree", "list", "--porcelain")
	index = localGit(t, source, "diff", "--cached")
	if err := g.open(ctx); err != nil {
		t.Fatal(err)
	}
	j2 := &Job{Issue: Issue{Number: 2}, Branch: branchName(cfg, 2), Base: remote}
	second, err := g.prepare(ctx, j2)
	if err != nil {
		t.Fatal(err)
	}
	if localGit(t, second.config.Directory, "rev-parse", "HEAD") != remote {
		t.Fatal("new issue missed concurrent remote changes")
	}
	if _, err := os.Stat(filepath.Join(second.config.Directory, "first-fix")); !os.IsNotExist(err) {
		t.Fatal("second issue inherited the first issue's fix")
	}
	if _, err := g.prepare(ctx, j1); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(source, "README.md"):                  "unstaged user edits",
		filepath.Join(other, "README.md"):                   "another bot's edits",
		filepath.Join(first.config.Directory, "unfinished"): "keep first issue work",
	} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("changed another workspace's file: %s %s %v", path, data, err)
		}
	}
	checkSource()
	if localGit(t, cfg.Directory, "rev-parse", "HEAD") != base || localGit(t, first.config.Directory, "branch", "--show-current") != j1.Branch {
		t.Fatal("bot moved the management checkout or another issue branch")
	}
}

func TestLegacyCloneWorktreesResumeAndExternalStorageIsRejected(t *testing.T) {
	f := newFixture(t)
	g := checkout{f.e.config}
	ctx := context.Background()
	localGit(t, filepath.Dir(g.config.Directory), "clone", g.config.Remote, g.config.Directory)
	if err := g.open(ctx); err != nil {
		t.Fatal(err)
	}
	j := &Job{Issue: Issue{Number: 1}, Branch: branchName(g.config, 1), Base: localGit(t, g.config.Directory, "rev-parse", "HEAD")}
	work, err := g.prepare(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(work.config.Directory, "unfinished"), "legacy edits")
	localGit(t, work.config.Directory, "add", "unfinished")
	if err := g.open(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := g.prepare(ctx, j); err != nil {
		t.Fatal(err)
	}
	if localGit(t, work.config.Directory, "diff", "--cached") == "" {
		t.Fatal("lost staged legacy work")
	}
	if _, err := os.Stat(g.repositoryDirectory()); !os.IsNotExist(err) {
		t.Fatal("legacy repository was migrated unexpectedly")
	}
	external := filepath.Join(canonicalTestDir(t), "shared")
	localGit(t, g.config.Directory, "worktree", "add", "--detach", external)
	g.config.Directory = external
	if err := g.open(ctx); err == nil || !strings.Contains(err.Error(), "shares Git metadata") {
		t.Fatalf("accepted another repository's worktree: %v", err)
	}
}

func TestRetryPreservesEditsAndMergeConflictWhileBaseAdvances(t *testing.T) {
	f := newFixture(t)
	f.source.items = f.source.items[:1]
	seed := filepath.Join(filepath.Dir(f.e.config.Directory), "seed")
	now := f.e.now()
	f.e.now = func() time.Time { return now }
	calls := 0
	var dir, branch string
	f.e.agent = func(cfg Config) Agent {
		return scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
			calls++
			if !strings.Contains(prompt, cfg.Directory) || cfg.Directory == f.e.config.Directory {
				t.Fatal("agent context did not identify its issue worktree")
			}
			if calls == 1 {
				dir, branch = cfg.Directory, localGit(t, cfg.Directory, "branch", "--show-current")
				writeTestFile(t, filepath.Join(dir, "README.md"), "issue fix")
				localGit(t, dir, "add", ".")
				localGit(t, dir, "commit", "-m", "Issue fix")
				writeTestFile(t, filepath.Join(dir, "staged"), "keep staged edits")
				localGit(t, dir, "add", "staged")
				writeTestFile(t, filepath.Join(dir, "unstaged"), "keep unfinished edits")
				return Result{}, errors.New("interrupted preparation")
			}
			if cfg.Directory != dir || localGit(t, dir, "branch", "--show-current") != branch {
				t.Fatal("retry changed workspace or branch")
			}
			if calls == 2 {
				if localGit(t, dir, "show", ":staged") != "keep staged edits" {
					t.Fatal("retry lost staged edits")
				}
				data, _ := os.ReadFile(filepath.Join(dir, "unstaged"))
				if string(data) != "keep unfinished edits" {
					t.Fatal("retry lost unfinished edits")
				}
				localGit(t, dir, "add", ".")
				localGit(t, dir, "commit", "-m", "Finish pending edits")
				merge := exec.Command("git", "merge", "--no-commit", "origin/main")
				merge.Dir = dir
				merge.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
				if output, err := merge.CombinedOutput(); err == nil || !strings.Contains(string(output), "CONFLICT") {
					t.Fatalf("expected merge conflict: %s %v", output, err)
				}
				return Result{}, errors.New("interrupted conflict resolution")
			}
			localGit(t, dir, "rev-parse", "--verify", "MERGE_HEAD")
			if localGit(t, dir, "diff", "--name-only", "--diff-filter=U") != "README.md" {
				t.Fatal("retry lost the unfinished merge")
			}
			writeTestFile(t, filepath.Join(dir, "README.md"), "issue fix and concurrent remote change")
			localGit(t, dir, "add", "README.md")
			localGit(t, dir, "commit", "-m", "Resolve both changes")
			localGit(t, dir, "merge", "--no-edit", "origin/main")
			return Result{Status: "solved", Title: "Fix issue", Detail: "Preserved both changes", Tests: []string{"fixture verified"}}, nil
		})
	}
	for attempt := range 3 {
		if _, err := f.e.step(context.Background(), f.s); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			writeTestFile(t, filepath.Join(seed, "README.md"), "concurrent remote change")
			localGit(t, seed, "commit", "-am", "Other bot's change")
			localGit(t, seed, "push", "origin", "main")
		} else if attempt == 1 {
			writeTestFile(t, filepath.Join(seed, "later"), "another remote change")
			localGit(t, seed, "add", ".")
			localGit(t, seed, "commit", "-m", "Remote advances during restart")
			localGit(t, seed, "push", "origin", "main")
		}
		now = now.Add(time.Duration(f.e.config.RetryDelay) + time.Second)
		saved, err := ReadState(f.e.config)
		if err != nil {
			t.Fatal(err)
		}
		f.s = saved
	}
	if calls != 3 || f.source.creates != 1 || f.s.Jobs[1].Status != "submitted" {
		t.Fatalf("recovery did not produce one PR: calls=%d state=%+v", calls, f.s.Jobs[1])
	}
	localGit(t, dir, "merge-base", "--is-ancestor", localGit(t, seed, "rev-parse", "HEAD"), "HEAD")
}
