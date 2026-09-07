package issuebot

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type issueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}
type commentSource interface {
	comments(context.Context, int) ([]issueComment, error)
	postComment(context.Context, int, string) (issueComment, error)
	editComment(context.Context, int64, string) (issueComment, error)
}

func (g githubClient) comments(ctx context.Context, n int) ([]issueComment, error) {
	var all []issueComment
	for page := 1; page <= 1000; page++ {
		var items []issueComment
		if err := g.api(ctx, g.path(fmt.Sprintf("/issues/%d/comments?per_page=100&page=%d", n, page)), &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("comment history exceeded pagination limit")
}
func (g githubClient) postComment(ctx context.Context, n int, body string) (issueComment, error) {
	var c issueComment
	err := g.api(ctx, g.path(fmt.Sprintf("/issues/%d/comments", n)), &c, "--method", "POST", "-f", "body="+body)
	return c, err
}
func (g githubClient) editComment(ctx context.Context, id int64, body string) (issueComment, error) {
	var c issueComment
	err := g.api(ctx, g.path(fmt.Sprintf("/issues/comments/%d", id)), &c, "--method", "PATCH", "-f", "body="+body)
	return c, err
}
