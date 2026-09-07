package issuebot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BrokkAi/issue-bot/internal/osrun"
)

type checkout struct{ config Config }

func (g checkout) git(ctx context.Context, args ...string) (string, error) {
	return osrun.Run(ctx, g.config.Directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"}, append([]string{"git"}, args...)...)
}
func (g checkout) open(ctx context.Context) error {
	if _, err := os.Stat(g.config.Directory); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(g.config.Directory), 0700); err != nil {
			return err
		}
		if _, err := osrun.Run(ctx, "", map[string]string{"GIT_TERMINAL_PROMPT": "0"}, "git", "clone", "--branch", g.config.Branch, "--", g.config.Remote, g.config.Directory); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	root, err := g.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if root != g.config.Directory {
		return errors.New("directory must be the root of a managed clone")
	}
	remote, err := g.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if remote != g.config.Remote {
		return errors.New("managed clone origin differs from configuration")
	}
	if _, err := g.git(ctx, "check-ref-format", "refs/heads/"+g.config.Branch); err != nil {
		return err
	}
	_, err = g.git(ctx, "fetch", "--prune", "origin", "+refs/heads/"+g.config.Branch+":refs/remotes/origin/"+g.config.Branch)
	return err
}
func branchName(cfg Config, n int) string {
	hash := sha256.Sum256([]byte(cfg.Branch))
	return fmt.Sprintf("issue-bot/%x/%d", hash[:4], n)
}
func (g checkout) workdir(j *Job) string {
	return filepath.Join(g.config.Directory+"-issues", fmt.Sprint(j.Issue.Number))
}
func (g checkout) prepare(ctx context.Context, j *Job) (checkout, error) {
	work := g
	work.config.Directory = g.workdir(j)
	if j.Branch != branchName(g.config, j.Issue.Number) {
		return work, errors.New("saved issue branch does not match configuration")
	}
	if _, err := os.Stat(work.config.Directory); err == nil {
		root, err := work.git(ctx, "rev-parse", "--show-toplevel")
		if err != nil {
			return work, err
		}
		if root != work.config.Directory {
			return work, errors.New("issue directory is not a worktree root")
		}
		common, err := work.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return work, err
		}
		expected, err := g.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return work, err
		}
		if common != expected {
			return work, errors.New("issue directory belongs to another repository")
		}
		branch, err := work.git(ctx, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			return work, err
		}
		if branch != j.Branch {
			return work, errors.New("issue worktree changed branches; inspect before retry")
		}
		return work, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return work, err
	}
	if err := os.MkdirAll(filepath.Dir(work.config.Directory), 0700); err != nil {
		return work, err
	}
	if _, err := g.git(ctx, "show-ref", "--verify", "refs/heads/"+j.Branch); err == nil {
		_, err = g.git(ctx, "worktree", "add", "--", work.config.Directory, j.Branch)
		return work, err
	}
	// Recover a pushed branch even if the local workspace was lost.
	remote, err := g.git(ctx, "ls-remote", "--heads", "origin", "refs/heads/"+j.Branch)
	if err != nil {
		return work, err
	}
	base := j.Base
	if remote != "" {
		if _, err := g.git(ctx, "fetch", "origin", "refs/heads/"+j.Branch+":refs/remotes/origin/"+j.Branch); err != nil {
			return work, err
		}
		base = "refs/remotes/origin/" + j.Branch
	}
	_, err = g.git(ctx, "worktree", "add", "-b", j.Branch, "--", work.config.Directory, base)
	return work, err
}
func (g checkout) verify(ctx context.Context, j *Job) (string, error) {
	branch, err := g.git(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if branch != j.Branch {
		return "", errors.New("agent left the issue branch")
	}
	status, err := g.git(ctx, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", errors.New("agent left uncommitted changes")
	}
	if _, err := g.git(ctx, "merge-base", "--is-ancestor", j.Base, "HEAD"); err != nil {
		return "", errors.New("issue branch no longer contains its starting commit")
	}
	diff, err := g.git(ctx, "diff", "--stat", j.Base, "HEAD")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(diff) == "" {
		return "", errors.New("issue branch has no changes")
	}
	if len(g.config.Verify) > 0 {
		if _, err := osrun.Run(ctx, g.config.Directory, map[string]string{"ISSUE_NUMBER": fmt.Sprint(j.Issue.Number)}, g.config.Verify...); err != nil {
			return "", fmt.Errorf("operator verification: %w", err)
		}
	}
	return g.git(ctx, "rev-parse", "HEAD")
}
func (g checkout) push(ctx context.Context, j *Job) error {
	remote, err := g.git(ctx, "remote", "get-url", "--push", "origin")
	if err != nil {
		return err
	}
	if remote != g.config.Remote {
		return errors.New("push URL differs from configured remote")
	}
	_, err = g.git(ctx, "push", "origin", "HEAD:refs/heads/"+j.Branch)
	return err
}
