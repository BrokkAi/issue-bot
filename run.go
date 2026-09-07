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
	config Config
	source issueSource
	log    *slog.Logger
	agent  func(Config) Agent
	now    func() time.Time
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
	for {
		worked, err := e.step(ctx, state)
		if err != nil {
			return err
		}
		if once {
			return nil
		}
		if worked {
			continue
		}
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
func (e engine) save(s *State) error { return writeState(e.config, s) }
func (e engine) step(ctx context.Context, s *State) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	issues, err := e.source.issues(ctx)
	if err != nil {
		return false, err
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	// Reconcile interrupted/pending work even when an issue was closed or unlabelled.
	numbers := make([]int, 0, len(s.Jobs))
	for n, j := range s.Jobs {
		if j.Status == "pending" && (e.config.Issue == 0 || e.config.Issue == n) {
			numbers = append(numbers, n)
		}
	}
	sort.Ints(numbers)
	for _, n := range numbers {
		j := s.Jobs[n]
		pr, err := e.source.pull(ctx, j)
		if err != nil {
			return false, err
		}
		if pr != nil {
			e.recordPull(j, pr)
			if err := e.save(s); err != nil {
				return false, err
			}
		}
	}
	for _, i := range issues {
		if !eligible(e.config, i) {
			continue
		}
		j := s.Jobs[i.Number]
		if j != nil && (j.Status != "pending" || j.RetryAt.After(e.now())) {
			continue
		}
		if j == nil {
			j = &Job{Issue: i, Branch: branchName(e.config, i.Number), Status: "pending"}
			s.Jobs[i.Number] = j
			if err := e.save(s); err != nil {
				return false, err
			}
		}
		pr, err := e.source.pull(ctx, j)
		if err != nil {
			return false, err
		}
		if pr != nil {
			e.recordPull(j, pr)
			return true, e.save(s)
		}
		if j.Tries >= e.config.Attempts {
			j.Status = "blocked"
			return true, e.save(s)
		}
		latest, err := e.source.issue(ctx, i.Number)
		if err != nil {
			return false, err
		}
		if !eligible(e.config, latest) {
			j.Status = "skipped"
			j.Failure = "Issue closed, locked or no longer matches filters"
			return true, e.save(s)
		}
		j.Issue = latest
		return true, e.attempt(ctx, s, j)
	}
	return false, nil
}
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
	e.log.Info("Working on issue", "issue", j.Issue.Number, "title", j.Issue.Title, "attempt", j.Tries, "directory", work.config.Directory)
	result, err := e.agent(work.config).Execute(ctx, issuePrompt(e.config, j))
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
	if err := work.push(ctx, j); err != nil {
		return e.failed(s, j, err)
	}
	// Recheck after push in case an earlier creation completed after a timeout.
	pr, err := e.source.pull(ctx, j)
	if err != nil {
		return e.failed(s, j, err)
	}
	if pr == nil {
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
