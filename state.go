package issuebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Result struct {
	Status string   `json:"status"`
	Title  string   `json:"title,omitempty"`
	Detail string   `json:"detail"`
	Tests  []string `json:"tests,omitempty"`
}
type Job struct {
	ClaimPending bool      `json:"claim_pending,omitempty"`
	Claim        *Claim    `json:"claim,omitempty"`
	Issue        Issue     `json:"issue"`
	Branch       string    `json:"branch"`
	Base         string    `json:"base,omitempty"`
	Tries        int       `json:"tries"`
	RetryAt      time.Time `json:"retry_at,omitempty"`
	Failure      string    `json:"failure,omitempty"`
	Status       string    `json:"status"`
	URL          string    `json:"url,omitempty"`
	Result       *Result   `json:"result,omitempty"`
	// Superseded lists pull requests Brokk Town closed after review and asked
	// the bot to replace. They never count as this issue's existing PR again.
	Superseded []int `json:"superseded,omitempty"`
}

func (j *Job) superseded(n int) bool {
	for _, s := range j.Superseded {
		if s == n {
			return true
		}
	}
	return false
}

type State struct {
	Format    int          `json:"format"`
	Remote    string       `json:"remote"`
	Branch    string       `json:"branch"`
	Directory string       `json:"directory"`
	Repo      string       `json:"repo"`
	Host      string       `json:"host"`
	Jobs      map[int]*Job `json:"jobs"`
}

func ReadState(cfg Config) (*State, error) {
	data, err := os.ReadFile(filepath.Join(cfg.StateDirectory, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid state, refusing to guess issue progress: %w", err)
	}
	if s.Format != 1 || s.Remote != cfg.Remote || s.Branch != cfg.Branch || s.Directory != cfg.Directory || s.Repo != cfg.GitHubRepo() || s.Host != cfg.GitHub.Host {
		return nil, errors.New("state version or repository identity does not match this configuration")
	}
	if s.Jobs == nil {
		s.Jobs = make(map[int]*Job)
	}
	for n, j := range s.Jobs {
		if j == nil || j.Issue.Number != n || n < 1 {
			return nil, errors.New("invalid saved issue job")
		}
	}
	return &s, nil
}
func writeState(cfg Config, state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(cfg.StateDirectory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(cfg.StateDirectory, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(cfg.StateDirectory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another process holds %s: %w", path, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func lockConfig(cfg Config) (func(), error) {
	stateUnlock, err := lockFile(filepath.Join(cfg.StateDirectory, "daemon.lock"))
	if err != nil {
		return nil, err
	}
	checkoutUnlock, err := lockFile(cfg.Directory + ".issue-bot.lock")
	if err != nil {
		stateUnlock()
		return nil, err
	}
	return func() { checkoutUnlock(); stateUnlock() }, nil
}

// Requeue prepares one issue for a fresh implementation after Town closed its
// pull request. The job returns to pending with a clean attempt budget, the
// closed PR is recorded so it never counts as an existing PR again, and any
// open claim is released so the next attempt can claim the issue anew.
func Requeue(cfg Config, pr int) error {
	if cfg.Issue < 1 || pr < 1 {
		return errors.New("requeue requires an issue and the superseded pull request number")
	}
	unlock, err := lockConfig(cfg)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadState(cfg)
	if err != nil {
		return err
	}
	if s == nil {
		s = &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Repo: cfg.GitHubRepo(), Host: cfg.GitHub.Host, Jobs: map[int]*Job{}}
	}
	j := s.Jobs[cfg.Issue]
	if j == nil {
		j = &Job{Issue: Issue{Number: cfg.Issue}, Branch: branchName(cfg, cfg.Issue)}
		s.Jobs[cfg.Issue] = j
	}
	if j.Claim != nil && j.Claim.Status == "working" {
		return errors.New("issue is being worked on; requeue it after the attempt ends")
	}
	if !j.superseded(pr) {
		j.Superseded = append(j.Superseded, pr)
	}
	j.Status = "pending"
	j.Tries = 0
	j.RetryAt = time.Time{}
	j.Failure = ""
	j.URL = ""
	j.Result = nil
	j.Base = ""
	if err := discardWork(cfg, j); err != nil {
		return err
	}
	if j.Claim != nil && j.Claim.Status != "released" {
		j.Claim.Status = "released"
		j.Claim.Detail = fmt.Sprintf("Brokk Town closed pull request #%d after review and requeued the issue for a fresh attempt.", pr)
		j.ClaimPending = true
	}
	return writeState(cfg, s)
}

// discardWork removes the superseded attempt's worktree and local branch so the
// next attempt starts from the base branch rather than on top of the closed PR.
func discardWork(cfg Config, j *Job) error {
	if _, err := os.Stat(cfg.Directory); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	g := checkout{cfg}
	work := g.workdir(j)
	if _, err := os.Stat(work); err == nil {
		if _, err := g.git(ctx, "worktree", "remove", "--force", "--", work); err != nil {
			return fmt.Errorf("discard superseded worktree: %w", err)
		}
	}
	_, _ = g.git(ctx, "worktree", "prune")
	if _, err := g.git(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+j.Branch); err == nil {
		if _, err := g.git(ctx, "branch", "-D", j.Branch); err != nil {
			return fmt.Errorf("discard superseded branch: %w", err)
		}
	}
	return nil
}

func Retry(cfg Config) error {
	unlock, err := lockConfig(cfg)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadState(cfg)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("no saved issues")
	}
	found := false
	for n, j := range s.Jobs {
		if (cfg.Issue == 0 || n == cfg.Issue) && (j.Status == "blocked" || j.Status == "pending") {
			j.Tries = 0
			j.RetryAt = time.Time{}
			j.Status = "pending"
			found = true
		}
	}
	if !found {
		return errors.New("no pending or blocked issues to retry")
	}
	return writeState(cfg, s)
}
