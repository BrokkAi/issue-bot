package issuebot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const claimPrefix = "<!-- issue-bot-claim:v1 "

var errClaimLost = errors.New("issue claim expired or belongs to another instance")

// Claim is also saved before creating the comment, so an ambiguous POST can be
// recovered by token without issuing another starting comment.
type Claim struct {
	Token     string    `json:"token"`
	CommentID int64     `json:"comment_id,omitempty"`
	Repo      string    `json:"repo"`
	Issue     int       `json:"issue"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	Detail    string    `json:"detail,omitempty"`
	URL       string    `json:"url,omitempty"`
}

func newClaim(cfg Config, n int, now time.Time) *Claim {
	token := make([]byte, 16)
	_, _ = rand.Read(token) // crypto/rand.Read never returns an error in supported Go.
	return &Claim{Token: hex.EncodeToString(token), Repo: cfg.GitHubRepo(), Issue: n, Status: "working", ExpiresAt: now.Add(time.Duration(cfg.ClaimTimeout))}
}
func (c Claim) body() string {
	var text string
	switch c.Status {
	case "working":
		text = fmt.Sprintf("I'm starting work on this issue. This instance will keep its claim renewed while working. Other issue-bot instances should leave it alone until %s (UTC) unless this comment is marked finished earlier.", c.ExpiresAt.UTC().Format(time.RFC3339))
	case "done":
		text = "Done — pull request: " + c.URL
	default:
		text = "I've stopped working on this issue and released my claim. " + c.Detail
	}
	wire := c
	wire.CommentID = 0
	data, _ := json.Marshal(wire)
	return text + "\n\n" + claimPrefix + string(data) + " -->"
}
func parseClaim(comment issueComment, repo string, n int) *Claim {
	pos := strings.LastIndex(comment.Body, claimPrefix)
	if pos < 0 {
		return nil
	}
	tail := strings.TrimSpace(comment.Body[pos+len(claimPrefix):])
	if !strings.HasSuffix(tail, "-->") {
		return nil
	}
	var c Claim
	if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimSuffix(tail, "-->"))), &c) != nil {
		return nil
	}
	if !strings.EqualFold(c.Repo, repo) || c.Issue != n || len(c.Token) != 32 {
		return nil
	}
	if _, err := hex.DecodeString(c.Token); err != nil {
		return nil
	}
	c.CommentID = comment.ID
	return &c
}
func claimWinner(comments []issueComment, repo string, n int, now time.Time) *Claim {
	var winner *Claim
	for _, comment := range comments {
		c := parseClaim(comment, repo, n)
		if c == nil || c.Status != "working" || !c.ExpiresAt.After(now) || c.CommentID < 1 {
			continue
		}
		if winner == nil || c.CommentID < winner.CommentID {
			winner = c
		}
	}
	return winner
}
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type claimManager struct {
	source commentSource
	config Config
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
}

// acquire does read/post/read election; the lowest active comment ID wins.
// Comments are an advisory lease, not an atomic compare-and-swap service.
func (m claimManager) acquire(ctx context.Context, c *Claim) (bool, error) {
	comments, err := m.source.comments(ctx, c.Issue)
	if err != nil {
		return false, err
	}
	winner := claimWinner(comments, c.Repo, c.Issue, m.now())
	if winner != nil && winner.Token != c.Token {
		return false, nil
	}
	// Recover our own previous POST even when its response was lost.
	for _, comment := range comments {
		if old := parseClaim(comment, c.Repo, c.Issue); old != nil && old.Token == c.Token {
			c.CommentID = old.CommentID
			break
		}
	}
	c.Status = "working"
	c.ExpiresAt = m.now().Add(time.Duration(m.config.ClaimTimeout))
	if c.CommentID == 0 {
		comment, err := m.source.postComment(ctx, c.Issue, c.body())
		if err != nil {
			return false, err
		}
		c.CommentID = comment.ID
	} else {
		if _, err := m.source.editComment(ctx, c.CommentID, c.body()); err != nil {
			return false, err
		}
	}
	wait := m.wait
	if wait == nil {
		wait = pause
	}
	if err := wait(ctx, 2*time.Second); err != nil {
		return false, err
	}
	comments, err = m.source.comments(ctx, c.Issue)
	if err != nil {
		return false, err
	}
	winner = claimWinner(comments, c.Repo, c.Issue, m.now())
	if winner == nil || winner.Token != c.Token || winner.CommentID != c.CommentID {
		return false, nil
	}
	*c = *winner
	return true, nil
}
func (m claimManager) sync(ctx context.Context, c *Claim) error {
	// Resolve by token on every retry, including a lost initial POST response.
	comments, err := m.source.comments(ctx, c.Issue)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if old := parseClaim(comment, c.Repo, c.Issue); old != nil && old.Token == c.Token {
			c.CommentID = old.CommentID
			if comment.Body == c.body() {
				return nil
			}
			_, err = m.source.editComment(ctx, c.CommentID, c.body())
			return err
		}
	}
	if c.Status != "done" {
		return nil
	} // No visible claim was created; nothing to release.
	comment, err := m.source.postComment(ctx, c.Issue, c.body())
	if err == nil {
		c.CommentID = comment.ID
	}
	return err
}

type lease struct {
	mu      sync.Mutex
	manager claimManager
	claim   Claim
	cancel  context.CancelCauseFunc
	stop    context.CancelFunc
	done    chan struct{}
}

func (m claimManager) start(parent context.Context, c Claim) (context.Context, *lease) {
	ctx, cancel := context.WithCancelCause(parent)
	renewal, stop := context.WithCancel(ctx)
	l := &lease{manager: m, claim: c, cancel: cancel, stop: stop, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		interval := time.Duration(m.config.ClaimTimeout) / 3
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-renewal.Done():
				return
			case <-ticker.C:
				attempt, end := context.WithTimeout(renewal, interval)
				err := l.refresh(attempt, true)
				end()
				if err != nil {
					cancel(fmt.Errorf("claim renewal: %w", err))
					return
				}
			}
		}
	}()
	return ctx, l
}
func (l *lease) refresh(ctx context.Context, renew bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	comments, err := l.manager.source.comments(ctx, l.claim.Issue)
	if err != nil {
		return err
	}
	winner := claimWinner(comments, l.claim.Repo, l.claim.Issue, l.manager.now())
	if winner == nil || winner.Token != l.claim.Token || winner.CommentID != l.claim.CommentID {
		return errClaimLost
	}
	if renew {
		next := l.claim
		next.ExpiresAt = l.manager.now().Add(time.Duration(l.manager.config.ClaimTimeout))
		if _, err := l.manager.source.editComment(ctx, next.CommentID, next.body()); err != nil {
			return err
		}
		l.claim = next
	}
	return nil
}
func (l *lease) close() Claim {
	l.stop()
	<-l.done
	l.cancel(context.Canceled)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.claim
}
