package issuebot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/BrokkAi/acp-go/runner"
)

type engine struct {
	config  Config
	source  issueSource
	log     *slog.Logger
	agent   func(Config) Agent
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
	lease   *lease
	observe func(Progress)
	active  *Job
}

func Run(ctx context.Context, cfg Config, log *slog.Logger, once bool) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.GitHubRepo() == "" {
		return errors.New("GitHub repository required; use a GitHub remote or github.repo")
	}
	if log == nil {
		log = slog.Default()
	}
	unlock, err := lockConfig(cfg)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := ReadState(cfg)
	if err != nil {
		return err
	}
	if state == nil {
		state = &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Repo: cfg.GitHubRepo(), Host: cfg.GitHub.Host, Jobs: map[int]*Job{}}
	}
	e := engine{config: cfg, source: githubClient{cfg}, log: log, now: time.Now, agent: func(c Config) Agent { return agentProcess{c, log} }}
	e.observe, _ = ctx.Value(progressKey{}).(func(Progress))
	e.report(state, "starting", "Loading saved issues")
	for {
		worked, err := e.step(ctx, state)
		if err != nil {
			e.report(state, "paused", err.Error())
			var setup *runner.SetupError
			if once || errors.As(err, &setup) || ctx.Err() != nil {
				return err
			}
			log.Error("Issue scan failed; will retry", "error", err)
			if err := pause(ctx, time.Duration(cfg.Poll)); err != nil {
				return err
			}
			continue
		}
		if once {
			return nil
		}
		if worked {
			continue
		}
		e.report(state, "waiting", "Waiting for eligible issues")
		log.Info("Waiting for eligible issues", "poll", time.Duration(cfg.Poll))
		timer := time.NewTimer(time.Duration(cfg.Poll))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (e engine) save(s *State) error {
	if err := writeState(e.config, s); err != nil {
		return err
	}
	e.report(s, "", "")
	return nil
}
func (e engine) manager() claimManager {
	return claimManager{source: e.source, config: e.config, now: e.now, wait: e.wait}
}
func (e engine) syncClaim(ctx context.Context, s *State, j *Job) error {
	if !j.ClaimPending || j.Claim == nil {
		return nil
	}
	if err := e.manager().sync(ctx, j.Claim); err != nil {
		return err
	}
	j.ClaimPending = false
	return e.save(s)
}
func (e engine) complete(ctx context.Context, s *State, j *Job, p *PullRequest) error {
	e.recordPull(j, p)
	if j.Claim == nil {
		j.Claim = newClaim(e.config, j.Issue.Number, e.now())
	}
	j.Claim.Status = "done"
	j.Claim.URL = p.URL
	j.Claim.Detail = ""
	j.ClaimPending = true
	if err := e.save(s); err != nil {
		return err
	}
	e.active = j
	e.report(s, "reconciled", "Reconciled PR: "+j.Issue.Title)
	return e.syncClaim(ctx, s, j)
}
func (e engine) step(ctx context.Context, s *State) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	e.report(s, "checking", "Reconciling saved issues and pull requests")
	// Deliver saved completion/release updates even if the issue was closed or
	// is no longer returned by the issue query. Never rerun an agent to do this.
	numbers := make([]int, 0, len(s.Jobs))
	for n := range s.Jobs {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	for _, n := range numbers {
		if e.config.Issue != 0 && n != e.config.Issue {
			continue
		}
		j := s.Jobs[n]
		if j.ClaimPending && j.Claim != nil && j.Claim.Status != "working" {
			if err := e.syncClaim(ctx, s, j); err != nil {
				e.log.Error("Status comment update failed; retaining it for retry", "issue", n, "error", err)
			}
		}
		if j.Status == "pending" && j.Tries > 0 {
			pr, err := e.source.pull(ctx, j)
			if err != nil {
				return false, err
			}
			if pr != nil {
				if err := e.complete(ctx, s, j, pr); err != nil {
					return false, err
				}
			}
		}
	}
	e.report(s, "fetching", "Fetching eligible GitHub issues")
	issues, err := e.source.issues(ctx)
	if err != nil {
		return false, err
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	for _, i := range issues {
		if !eligible(e.config, i) {
			continue
		}
		j := s.Jobs[i.Number]
		if j != nil && (j.Status != "pending" || j.ClaimPending || j.RetryAt.After(e.now())) {
			continue
		}
		if j == nil {
			j = &Job{Issue: i, Branch: branchName(e.config, i.Number), Status: "pending"}
			s.Jobs[i.Number] = j
		}
		pr, err := e.source.pull(ctx, j)
		if err != nil {
			return false, err
		}
		if pr != nil {
			if err := e.complete(ctx, s, j, pr); err != nil {
				return false, err
			}
			continue
		}
		linked, err := e.source.linkedPull(ctx, i.Number)
		if err != nil {
			return false, err
		}
		if linked != nil {
			j.Status = "has_pr"
			j.URL = linked.URL
			e.log.Info("Skipping issue with an existing PR", "issue", i.Number, "url", linked.URL)
			if err := e.save(s); err != nil {
				return false, err
			}
			continue
		}
		if j.Tries >= e.config.Attempts {
			j.Status = "blocked"
			if err := e.releaseClaim(ctx, s, j, "Attempt budget exhausted"); err != nil {
				return false, err
			}
			continue
		}
		latest, err := e.source.issue(ctx, i.Number)
		if err != nil {
			return false, err
		}
		if !eligible(e.config, latest) {
			continue
		}
		j.Issue = latest
		worked, err := e.claimedAttempt(ctx, s, j)
		if err != nil {
			return false, err
		}
		if worked {
			return true, nil
		}
	}
	e.active = nil
	e.report(s, "waiting", "Waiting for eligible issues")
	return false, nil
}
func (e engine) releaseClaim(ctx context.Context, s *State, j *Job, detail string) error {
	if j.Claim != nil {
		j.Claim.Status = "released"
		j.Claim.Detail = detail
		j.ClaimPending = true
	}
	if err := e.save(s); err != nil {
		return err
	}
	return e.syncClaim(ctx, s, j)
}
func (e engine) claimedAttempt(ctx context.Context, s *State, j *Job) (bool, error) {
	e.active = j
	e.report(s, "claiming", fmt.Sprintf("Claiming #%d: %s", j.Issue.Number, j.Issue.Title))
	if j.Claim == nil || j.Claim.Status != "working" || !j.Claim.ExpiresAt.After(e.now()) {
		j.Claim = newClaim(e.config, j.Issue.Number, e.now())
	}
	if err := e.save(s); err != nil {
		return false, err
	}
	acquired, err := e.manager().acquire(ctx, j.Claim)
	if err != nil {
		return false, err
	}
	if !acquired {
		e.log.Info("Issue claimed by another instance", "issue", j.Issue.Number)
		j.RetryAt = e.now().Add(time.Duration(e.config.Poll))
		return false, e.releaseClaim(ctx, s, j, "Another instance owns the active claim; yielding.")
	}
	if err := e.save(s); err != nil {
		return false, err
	}
	workCtx, lease := e.manager().start(ctx, *j.Claim)
	e.lease = lease
	attemptErr := e.attempt(workCtx, s, j)
	final := lease.close()
	j.Claim = &final
	if j.Status == "submitted" {
		j.Claim.Status = "done"
		j.Claim.URL = j.URL
	} else {
		j.Claim.Status = "released"
		j.Claim.Detail = "The attempt did not produce a PR. Progress and diagnostics are saved locally for retry."
		if j.Status == "blocked" && j.Result != nil {
			j.Claim.Detail = j.Result.Detail
		}
		if attemptErr != nil {
			j.Claim.Detail = "The attempt could not continue. The operator should check the local diagnostics."
		}
		if j.Status == "has_pr" {
			j.Claim.Detail = "A pull request already exists: " + j.URL
		}
		if j.Claim.Detail == "" {
			j.Claim.Detail = "The attempt ended; another instance may claim the issue."
		}
	}
	j.ClaimPending = true
	if err := e.save(s); err != nil {
		return true, errors.Join(attemptErr, err)
	}
	// Release promptly on cancellation, with its own short cleanup budget.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err = errors.Join(attemptErr, e.syncClaim(finishCtx, s, j))
	phase, task := "paused", j.Failure
	switch j.Status {
	case "submitted":
		phase, task = "complete", "PR submitted: "+j.Issue.Title
	case "blocked":
		phase = "blocked"
	case "has_pr", "skipped":
		phase, task = "skipped", "Issue skipped: "+j.Issue.Title
	}
	if err != nil {
		phase, task = "paused", err.Error()
	}
	e.report(s, phase, task)
	return true, err
}
func (e engine) readyToPublish(ctx context.Context, j *Job) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if e.lease == nil {
		return errors.New("cannot publish without an issue claim")
	}
	if err := e.lease.refresh(ctx, false); err != nil {
		return err
	}
	linked, err := e.source.linkedPull(ctx, j.Issue.Number)
	if err != nil {
		return err
	}
	if linked != nil {
		j.Status = "has_pr"
		j.URL = linked.URL
		return errExistingPR
	}
	return nil
}

var errExistingPR = errors.New("another pull request already references the issue")

func (e engine) recordPull(j *Job, p *PullRequest) {
	j.URL = p.URL
	j.RetryAt = time.Time{}
	if p.State == "closed" && p.MergedAt == nil {
		j.Status = "blocked"
		j.Failure = "Pull request was closed without merging; inspect it before retrying"
	} else {
		j.Status = "submitted"
		j.Failure = ""
	}
	e.log.Info("Reconciled pull request", "issue", j.Issue.Number, "url", j.URL, "status", j.Status)
}
func (e engine) attempt(ctx context.Context, s *State, j *Job) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.config.Timeout))
	defer cancel()
	e.report(s, "preparing", "Preparing issue worktree: "+j.Issue.Title)
	g := checkout{e.config}
	if err := g.open(ctx); err != nil {
		return err
	}
	if j.Base == "" {
		base, err := g.git(ctx, "rev-parse", "refs/remotes/origin/"+e.config.Branch)
		if err != nil {
			return err
		}
		j.Base = base
		if err := e.save(s); err != nil {
			return err
		}
	}
	work, err := g.prepare(ctx, j)
	if err != nil {
		return err
	}
	j.Tries++
	j.RetryAt = e.now().Add(time.Duration(e.config.RetryDelay))
	if err := e.save(s); err != nil {
		return err
	}
	e.report(s, "attempt", fmt.Sprintf("Working on #%d: %s", j.Issue.Number, j.Issue.Title))
	e.log.Info("Working on issue", "issue", j.Issue.Number, "title", j.Issue.Title, "attempt", j.Tries, "directory", work.config.Directory)
	result, err := e.agent(work.config).Execute(ctx, issuePrompt(work.config, j))
	if err != nil {
		var setup *runner.SetupError
		if errors.As(err, &setup) {
			j.Tries--
			j.Failure = err.Error()
			j.RetryAt = time.Time{}
			if saveErr := e.save(s); saveErr != nil {
				return saveErr
			}
			return err
		}
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		return e.failed(s, j, err)
	}
	j.Result = &result
	if result.Status == "blocked" {
		j.Status = "blocked"
		j.Failure = result.Detail
		return e.save(s)
	}
	// Keep the agent's evidence durable before attempting external operations.
	if err := e.save(s); err != nil {
		return err
	}
	e.report(s, "verifying", "Verifying changes and issue eligibility: "+j.Issue.Title)
	head, err := work.verify(ctx, j)
	if err != nil {
		return e.failed(s, j, err)
	}
	latest, err := e.source.issue(ctx, j.Issue.Number)
	if err != nil {
		return e.failed(s, j, err)
	}
	if !eligible(e.config, latest) {
		j.Status = "skipped"
		j.Failure = "Issue is no longer eligible; local work retained"
		return e.save(s)
	}
	if err := e.readyToPublish(ctx, j); err != nil {
		if errors.Is(err, errExistingPR) {
			return e.save(s)
		}
		return e.failed(s, j, err)
	}
	e.report(s, "publishing", "Pushing branch and preparing PR: "+j.Issue.Title)
	if err := work.push(ctx, j, head); err != nil {
		return e.failed(s, j, err)
	}
	// Recheck after push in case an earlier creation completed after a timeout.
	pr, err := e.source.pull(ctx, j)
	if err != nil {
		return e.failed(s, j, err)
	}
	if pr == nil {
		if err := e.readyToPublish(ctx, j); err != nil {
			if errors.Is(err, errExistingPR) {
				return e.save(s)
			}
			return e.failed(s, j, err)
		}
		pr, err = e.source.create(ctx, j)
		if err != nil {
			return e.failed(s, j, err)
		}
	}
	if err := validatePull(e.config, j, pr); err != nil {
		return e.failed(s, j, err)
	}
	if pr.Head.SHA != head {
		return e.failed(s, j, errors.New("pull request head differs from the verified local commit"))
	}
	e.recordPull(j, pr)
	return e.save(s)
}
func (e engine) failed(s *State, j *Job, err error) error {
	j.Failure = err.Error()
	j.RetryAt = e.now().Add(time.Duration(e.config.RetryDelay))
	// Leave pending at the budget limit so the next scan reconciles a PR that
	// might have been created remotely before the failure was observed.
	e.log.Error("Issue attempt failed", "issue", j.Issue.Number, "error", err, "attempt", fmt.Sprintf("%d/%d", j.Tries, e.config.Attempts))
	return e.save(s)
}
