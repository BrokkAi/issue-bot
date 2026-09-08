package issuebot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestProgressTracksVerifiedPRAndRecovery(t *testing.T) {
	for _, lostResponse := range []bool{false, true} {
		t.Run(fmt.Sprint(lostResponse), func(t *testing.T) {
			f := newFixture(t)
			f.source.items = f.source.items[:1]
			f.source.failCreate = lostResponse
			var snapshots []Progress
			f.e.observe = func(p Progress) {
				snapshots = append(snapshots, p)
				if p.Phase == "publishing" {
					s, err := ReadState(f.e.config)
					if err != nil || s.Jobs[1].Result == nil || s.Jobs[1].Tries != 1 {
						t.Fatalf("publication shown before durable evidence: %v", err)
					}
				}
			}
			if _, err := f.e.step(context.Background(), f.s); err != nil {
				t.Fatal(err)
			}
			phases := map[string]bool{}
			for _, p := range snapshots {
				phases[p.Phase] = true
			}
			for _, phase := range []string{"checking", "fetching", "claiming", "preparing", "attempt", "verifying", "publishing"} {
				if !phases[phase] {
					t.Errorf("missing %s", phase)
				}
			}
			last := snapshots[len(snapshots)-1]
			if lostResponse {
				if last.Counts.Submitted != 0 || last.Phase != "paused" {
					t.Fatalf("unconfirmed PR shown as submitted: %+v", last)
				}
				s, err := ReadState(f.e.config)
				if err != nil {
					t.Fatal(err)
				}
				f.source.items = nil
				if _, err := f.e.step(context.Background(), s); err != nil {
					t.Fatal(err)
				}
				last = snapshots[len(snapshots)-1]
			} else if last.Phase != "complete" {
				t.Fatalf("final phase: %s", last.Phase)
			}
			if last.Counts.Submitted != 1 || last.Issues[0].URL == "" || f.calls != 1 {
				t.Fatalf("incorrect result or repeated agent work: %+v", last)
			}
			for _, p := range snapshots {
				if p.Phase == "attempt" && (p.Issues[0].Status != "pending" || strings.Contains(p.Issues[0].Body, "Fixed the bug")) {
					t.Fatal("later mutations changed an earlier snapshot")
				}
			}
		})
	}
}

func TestProgressBoundsHistoryAndKeepsActiveIssue(t *testing.T) {
	now := time.Now()
	var got Progress
	e := engine{config: DefaultConfig(), now: func() time.Time { return now }, observe: func(p Progress) { got = p }}
	s := &State{Jobs: map[int]*Job{}}
	for n := 1; n <= 250; n++ {
		s.Jobs[n] = &Job{Issue: Issue{Number: n}, Status: "submitted"}
	}
	e.active = s.Jobs[1]
	e.active.Status = "blocked"
	e.report(s, "waiting", "Next check")
	if len(got.Issues) != 200 || got.Issues[0].ID != "1" || got.Counts.Total != 250 || got.Counts.Submitted != 249 || got.Counts.Blocked != 1 {
		t.Fatalf("bounded display changed totals or hid active job: %+v", got.Counts)
	}
	if !got.WakeAt.Equal(now.Add(time.Duration(e.config.Poll))) {
		t.Fatal("countdown does not match polling")
	}
}
