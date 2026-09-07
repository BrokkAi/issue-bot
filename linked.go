package issuebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// LinkedPull can belong to anyone, on any branch or fork. A closed PR also
// counts: the operator asked for issues that do not already have a PR.
type LinkedPull struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

const linkedQuery = `query($owner:String!,$repo:String!,$number:Int!,$cursor:String) {
 repository(owner:$owner,name:$repo) {
  issue(number:$number) {
   closedByPullRequestsReferences(first:1,includeClosedPrs:true) { nodes { number url state } }
   timelineItems(first:100,after:$cursor,itemTypes:[CROSS_REFERENCED_EVENT,CONNECTED_EVENT]) {
    nodes {
     ... on CrossReferencedEvent { source { ... on PullRequest { number url state } } }
     ... on ConnectedEvent { subject { ... on PullRequest { number url state } } }
    }
    pageInfo { hasNextPage endCursor }
   }
  }
 }
}`

func (g githubClient) linkedPull(ctx context.Context, n int) (*LinkedPull, error) {
	parts := strings.SplitN(g.config.GitHubRepo(), "/", 2)
	cursor := ""
	for page := 0; page < 1000; page++ {
		var reply struct {
			Errors []struct{ Message string } `json:"errors"`
			Data   struct {
				Repository *struct {
					Issue *struct {
						ClosedBy struct{ Nodes []LinkedPull } `json:"closedByPullRequestsReferences"`
						Timeline struct {
							Nodes []struct {
								Source  *LinkedPull
								Subject *LinkedPull
							}
							PageInfo struct {
								HasNextPage bool
								EndCursor   string
							}
						} `json:"timelineItems"`
					}
				}
			}
		}
		fields := []string{"-f", "query=" + linkedQuery, "-f", "owner=" + parts[0], "-f", "repo=" + parts[1], "-F", fmt.Sprintf("number=%d", n)}
		if cursor != "" {
			fields = append(fields, "-f", "cursor="+cursor)
		}
		if err := g.api(ctx, "graphql", &reply, fields...); err != nil {
			return nil, err
		}
		if len(reply.Errors) > 0 {
			b, _ := json.Marshal(reply.Errors)
			return nil, fmt.Errorf("linked PR query: %s", b)
		}
		if reply.Data.Repository == nil || reply.Data.Repository.Issue == nil {
			return nil, errors.New("linked PR query returned no issue")
		}
		i := reply.Data.Repository.Issue
		for _, p := range i.ClosedBy.Nodes {
			if p.Number > 0 && p.URL != "" {
				return &p, nil
			}
		}
		for _, node := range i.Timeline.Nodes {
			for _, p := range []*LinkedPull{node.Source, node.Subject} {
				if p != nil && p.Number > 0 && p.URL != "" {
					return p, nil
				}
			}
		}
		if !i.Timeline.PageInfo.HasNextPage {
			return nil, nil
		}
		next := i.Timeline.PageInfo.EndCursor
		if next == "" || next == cursor {
			return nil, errors.New("linked PR query returned an invalid cursor")
		}
		cursor = next
	}
	return nil, errors.New("issue timeline exceeded pagination limit")
}
