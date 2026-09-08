package issuebot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Progress is an owned snapshot for a live display. Counts cover all saved jobs;
// Issues contains up to 200 jobs ordered by issue number, oldest first, always
// including the active issue. An empty Phase only refreshes saved results.
type Progress struct {
	Phase, Task, Commit  string
	Attempt, MaxAttempts int
	WakeAt               time.Time
	Failure              string
	Issues               []IssueProgress
	Counts               IssueCounts
}

type IssueProgress struct {
	ID, Title, Status, URL, Body string
}

type IssueCounts struct {
	Total, Submitted, ExistingPR, Pending, Blocked, Skipped int
}

type progressKey struct{}

// WithProgress observes progress synchronously. The callback should return
// promptly and must not perform actions on the repository.
func WithProgress(ctx context.Context, observe func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, observe)
}

func (e engine) report(s *State, phase, task string) {
	if e.observe == nil {
		return
	}
	p := Progress{Phase: phase, Task: task, MaxAttempts: e.config.Attempts}
	if j := e.active; j != nil {
		p.Commit, p.Attempt, p.Failure = j.Base, j.Tries, j.Failure
	}
	if phase == "waiting" || phase == "paused" {
		// Other issues may become eligible before a saved job's retry time.
		p.WakeAt = e.now().Add(time.Duration(e.config.Poll))
	}
	numbers := make([]int, 0, len(s.Jobs))
	for n, j := range s.Jobs {
		numbers = append(numbers, n)
		p.Counts.Total++
		switch j.Status {
		case "submitted":
			p.Counts.Submitted++
		case "has_pr":
			p.Counts.ExistingPR++
		case "pending":
			p.Counts.Pending++
		case "blocked":
			p.Counts.Blocked++
		default:
			p.Counts.Skipped++
		}
	}
	sort.Ints(numbers)
	if len(numbers) > 200 {
		numbers = numbers[len(numbers)-200:]
		if e.active != nil && e.active.Issue.Number < numbers[0] {
			numbers[0] = e.active.Issue.Number
		}
	}
	for _, n := range numbers {
		j := s.Jobs[n]
		p.Issues = append(p.Issues, IssueProgress{
			ID: fmt.Sprint(n), Title: fmt.Sprintf("#%d %s", n, j.Issue.Title),
			Status: j.Status, URL: j.URL, Body: issueDetails(j),
		})
	}
	e.observe(p)
}

func issueDetails(j *Job) string {
	body := fmt.Sprintf("Issue\n%s\n%s\n\nBranch\n%s\n\nBase\n%s\n\nAttempts\n%d", j.Issue.URL, j.Issue.Body, j.Branch, j.Base, j.Tries)
	if j.Result != nil {
		body += "\n\nResult\n" + j.Result.Detail + "\n\nTests\n" + strings.Join(j.Result.Tests, "\n")
	}
	if j.Failure != "" {
		body += "\n\nFailure\n" + j.Failure
	}
	if !j.RetryAt.IsZero() {
		body += "\n\nRetry eligible after\n" + j.RetryAt.Local().Format(time.RFC3339)
	}
	if j.Claim != nil {
		body += "\n\nClaim\n" + j.Claim.Status
	}
	return body
}
