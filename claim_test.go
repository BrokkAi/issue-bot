package issuebot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExistingPRSkippedBeforeClaimAndNextIssueWorked(t *testing.T) {
	f := newFixture(t)
	f.source.links[1] = &LinkedPull{Number: 55, URL: "https://github.com/other/fork/pull/55", State: "CLOSED"}
	if worked, err := f.e.step(context.Background(), f.s); err != nil || !worked {
		t.Fatalf("%v %v", worked, err)
	}
	if f.calls != 1 || f.source.creates != 1 || f.s.Jobs[1].Status != "has_pr" || f.s.Jobs[2].Status != "submitted" {
		t.Fatal("did not skip existing PR and continue")
	}
	if len(f.source.notes[1]) != 0 {
		t.Fatal("claimed an issue that already had a PR")
	}
	if notes := f.source.notes[2]; len(notes) != 1 || !strings.Contains(notes[0].Body, "Done") || !strings.Contains(notes[0].Body, f.s.Jobs[2].URL) {
		t.Fatalf("missing completion comment: %v", notes)
	}
}
func TestForeignClaimSkipsIssueUntilExpiry(t *testing.T) {
	f := newFixture(t)
	now := f.e.now()
	foreign := newClaim(f.e.config, 1, now)
	if _, err := f.source.postComment(context.Background(), 1, foreign.body()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
	if f.s.Jobs[2].Status != "submitted" || f.s.Jobs[1].Tries != 0 {
		t.Fatal("active claim did not protect issue")
	}
	f.e.now = func() time.Time { return now.Add(time.Duration(f.e.config.ClaimTimeout) + time.Second) }
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
	if f.s.Jobs[1].Status != "submitted" || f.calls != 2 {
		t.Fatal("expired claim prevented takeover")
	}
}
func TestClaimCommentPrecedesAnyAgentWork(t *testing.T) {
	f := newFixture(t)
	normal := f.e.agent
	f.e.agent = func(c Config) Agent {
		if winner := claimWinner(f.source.notes[1], c.GitHubRepo(), 1, f.e.now()); winner == nil || winner.Token != f.s.Jobs[1].Claim.Token {
			t.Fatal("agent started without owning a visible claim")
		}
		return normal(c)
	}
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
}
func TestCompletionCommentRetryNeverRunsAgentAgain(t *testing.T) {
	f := newFixture(t)
	f.source.items = f.source.items[:1]
	f.source.failEdit = true
	if _, err := f.e.step(context.Background(), f.s); err == nil {
		t.Fatal("comment update should fail")
	}
	saved, err := ReadState(f.e.config)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Jobs[1].Status != "submitted" || !saved.Jobs[1].ClaimPending {
		t.Fatal("completion progress was lost")
	}
	f.source.failEdit = false
	f.source.items = nil
	if _, err := f.e.step(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 || f.source.creates != 1 || saved.Jobs[1].ClaimPending {
		t.Fatal("comment retry duplicated work")
	}
	if !strings.Contains(f.source.notes[1][0].Body, saved.Jobs[1].URL) {
		t.Fatal("completion lost PR link")
	}
}
func TestPRAddedDuringWorkPreventsPublication(t *testing.T) {
	f := newFixture(t)
	normal := f.e.agent
	f.e.agent = func(c Config) Agent {
		return scriptedAgent(func(ctx context.Context, p string) (Result, error) {
			result, err := normal(c).Execute(ctx, p)
			f.source.links[1] = &LinkedPull{Number: 56, URL: "https://github.com/o/r/pull/56", State: "OPEN"}
			return result, err
		})
	}
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
	if f.source.creates != 0 || f.s.Jobs[1].Status != "has_pr" {
		t.Fatal("duplicate PR created")
	}
	if refs := localGit(t, f.e.config.Directory, "ls-remote", "--heads", "origin", "refs/heads/issue-bot/*"); refs != "" {
		t.Fatal("pushed after discovering PR")
	}
}
func TestLostClaimPreventsPush(t *testing.T) {
	f := newFixture(t)
	normal := f.e.agent
	f.e.agent = func(c Config) Agent {
		return scriptedAgent(func(ctx context.Context, p string) (Result, error) {
			r, err := normal(c).Execute(ctx, p)
			claim := *f.s.Jobs[1].Claim
			claim.Status = "released"
			_, _ = f.source.editComment(ctx, claim.CommentID, claim.body())
			return r, err
		})
	}
	if _, err := f.e.step(context.Background(), f.s); err != nil {
		t.Fatal(err)
	}
	if f.source.creates != 0 || !strings.Contains(f.s.Jobs[1].Failure, "claim") {
		t.Fatal("published without claim")
	}
}
func TestClaimPOSTResponseLossUsesSameComment(t *testing.T) {
	f := newFixture(t)
	m := f.e.manager()
	claim := newClaim(f.e.config, 1, f.e.now())
	f.source.failPost = true
	if _, err := m.acquire(context.Background(), claim); err == nil {
		t.Fatal("expected ambiguous POST")
	}
	f.source.failPost = false
	if acquired, err := m.acquire(context.Background(), claim); err != nil || !acquired {
		t.Fatalf("recovery %v %v", acquired, err)
	}
	if len(f.source.notes[1]) != 1 {
		t.Fatal("duplicated starting comment")
	}
}

// Both instances see no claim, then both POST before either verifies ownership.
// GitHub's ascending IDs break the tie independently of machine identity.
type concurrentComments struct {
	*fakeSource
	reads   atomic.Int32
	initial sync.WaitGroup
	posted  sync.WaitGroup
}

func (s *concurrentComments) comments(ctx context.Context, n int) ([]issueComment, error) {
	sequence := s.reads.Add(1)
	result, err := s.fakeSource.comments(ctx, n)
	if sequence <= 2 {
		s.initial.Done()
		s.initial.Wait()
	}
	return result, err
}
func (s *concurrentComments) postComment(ctx context.Context, n int, body string) (issueComment, error) {
	c, err := s.fakeSource.postComment(ctx, n, body)
	s.posted.Done()
	s.posted.Wait()
	return c, err
}
func TestSimultaneousClaimsElectOnlyOneWinner(t *testing.T) {
	f := newFixture(t)
	source := &concurrentComments{fakeSource: f.source}
	source.initial.Add(2)
	source.posted.Add(2)
	m := f.e.manager()
	m.source = source
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			c := newClaim(f.e.config, 1, f.e.now())
			ok, err := m.acquire(context.Background(), c)
			results <- ok
			errs <- err
		}()
	}
	wins := 0
	for range 2 {
		select {
		case ok := <-results:
			if ok {
				wins++
			}
		case <-time.After(3 * time.Second):
			t.Fatal("claim election deadlocked")
		}
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("elected %d claimants", wins)
	}
}
func TestRenewalFailureCancelsWork(t *testing.T) {
	f := newFixture(t)
	m := f.e.manager()
	m.now = time.Now
	m.config.ClaimTimeout = Duration(90 * time.Millisecond)
	claim := newClaim(m.config, 1, m.now())
	comment, _ := f.source.postComment(context.Background(), 1, claim.body())
	claim.CommentID = comment.ID
	f.source.failEdit = true
	ctx, l := m.start(context.Background(), *claim)
	defer l.close()
	select {
	case <-ctx.Done():
		if !strings.Contains(context.Cause(ctx).Error(), "claim renewal") {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("work survived a failed renewal")
	}
}
func TestRenewalExtendsClaimAndExpiredLeaseFails(t *testing.T) {
	f := newFixture(t)
	m := f.e.manager()
	claim := newClaim(m.config, 1, m.now())
	comment, _ := f.source.postComment(context.Background(), 1, claim.body())
	claim.CommentID = comment.ID
	before := claim.ExpiresAt
	now := m.now().Add(time.Minute)
	m.now = func() time.Time { return now }
	l := &lease{manager: m, claim: *claim}
	if err := l.refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !l.claim.ExpiresAt.After(before) {
		t.Fatal("heartbeat failed to extend expiry")
	}
	now = l.claim.ExpiresAt.Add(time.Second)
	if err := l.refresh(context.Background(), false); !errors.Is(err, errClaimLost) {
		t.Fatalf("expired claim accepted: %v", err)
	}
}

func TestUndeliverableCompletionDoesNotStarveOtherIssues(t *testing.T) {
	f := newFixture(t)
	f.source.failEdit = true
	_, _ = f.e.step(context.Background(), f.s)
	if f.source.creates != 1 {
		t.Fatal("first PR not created")
	}
	_, _ = f.e.step(context.Background(), f.s)
	if f.source.creates != 2 || f.calls != 2 {
		t.Fatal("a failed completion comment blocked the issue queue")
	}
	if !f.s.Jobs[1].ClaimPending || !f.s.Jobs[2].ClaimPending {
		t.Fatal("lost comment retries")
	}
}
