package issuebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/BrokkAi/issue-bot/internal/osrun"
)

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Locked bool   `json:"locked"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
}
type PullRequest struct {
	Number   int        `json:"number"`
	URL      string     `json:"html_url"`
	State    string     `json:"state"`
	Body     string     `json:"body"`
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}
type issueSource interface {
	commentSource
	linkedPull(context.Context, int) (*LinkedPull, error)
	issues(context.Context) ([]Issue, error)
	issue(context.Context, int) (Issue, error)
	pull(context.Context, *Job) (*PullRequest, error)
	create(context.Context, *Job) (*PullRequest, error)
}
type githubClient struct{ config Config }

func (g githubClient) api(ctx context.Context, endpoint string, result any, fields ...string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	args := []string{"gh", "api", "--hostname", g.config.GitHub.Host, endpoint}
	args = append(args, fields...)
	out, err := osrun.Run(ctx, "", nil, args...)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), result)
}
func (g githubClient) path(suffix string) string { return "repos/" + g.config.GitHubRepo() + suffix }
func (g githubClient) issues(ctx context.Context) ([]Issue, error) {
	if g.config.Issue > 0 {
		i, err := g.issue(ctx, g.config.Issue)
		if err != nil {
			return nil, err
		}
		return []Issue{i}, nil
	}
	var all []Issue
	for page := 1; page <= 1000; page++ {
		q := url.Values{"state": {"open"}, "sort": {"created"}, "direction": {"asc"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if len(g.config.Labels) > 0 {
			q.Set("labels", strings.Join(g.config.Labels, ","))
		}
		var items []Issue
		if err := g.api(ctx, g.path("/issues?")+q.Encode(), &items); err != nil {
			return nil, err
		}
		for _, i := range items {
			if len(i.PullRequest) == 0 {
				all = append(all, i)
			}
		}
		if len(items) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("issue history exceeded pagination limit")
}
func (g githubClient) issue(ctx context.Context, n int) (Issue, error) {
	var i Issue
	err := g.api(ctx, g.path(fmt.Sprintf("/issues/%d", n)), &i)
	return i, err
}
func marker(cfg Config, j *Job) string {
	return fmt.Sprintf("<!-- issue-bot: %s#%d -->", cfg.GitHubRepo(), j.Issue.Number)
}
func (g githubClient) pull(ctx context.Context, j *Job) (*PullRequest, error) {
	owner := strings.Split(g.config.GitHubRepo(), "/")[0]
	q := url.Values{"state": {"all"}, "head": {owner + ":" + j.Branch}, "per_page": {"100"}}
	var prs []PullRequest
	if err := g.api(ctx, g.path("/pulls?")+q.Encode(), &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	if len(prs) != 1 {
		return nil, errors.New("multiple pull requests on the issue branch; inspect before retrying")
	}
	pr := &prs[0]
	if err := validatePull(g.config, j, pr); err != nil {
		return nil, err
	}
	return pr, nil
}
func validatePull(cfg Config, j *Job, p *PullRequest) error {
	if p.Number < 1 || p.URL == "" || p.Head.Ref != j.Branch || !strings.EqualFold(p.Head.Repo.FullName, cfg.GitHubRepo()) || p.Base.Ref != cfg.Branch || !strings.EqualFold(p.Base.Repo.FullName, cfg.GitHubRepo()) || !strings.Contains(p.Body, marker(cfg, j)) {
		return errors.New("pull request does not match this issue, repository and branch")
	}
	return nil
}
func (g githubClient) create(ctx context.Context, j *Job) (*PullRequest, error) {
	r := j.Result
	body := r.Detail + "\n\nValidation:\n"
	for _, check := range r.Tests {
		body += "- " + check + "\n"
	}
	body += fmt.Sprintf("\nFixes #%d\n\n%s\n", j.Issue.Number, marker(g.config, j))
	var pr PullRequest
	err := g.api(ctx, g.path("/pulls"), &pr, "--method", "POST", "-f", "title="+r.Title, "-f", "head="+j.Branch, "-f", "base="+g.config.Branch, "-f", "body="+body, "-F", fmt.Sprintf("draft=%t", g.config.Draft))
	if err != nil {
		return nil, err
	}
	return &pr, validatePull(g.config, j, &pr)
}
func eligible(cfg Config, i Issue) bool {
	if i.Number < 1 || i.State != "open" || i.Locked || len(i.PullRequest) > 0 {
		return false
	}
	labels := map[string]bool{}
	for _, l := range i.Labels {
		labels[strings.ToLower(l.Name)] = true
	}
	for _, l := range cfg.ExcludeLabels {
		if labels[strings.ToLower(l)] {
			return false
		}
	}
	for _, l := range cfg.Labels {
		if !labels[strings.ToLower(l)] {
			return false
		}
	}
	return true
}
