package ghapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Review-thread state exists only in GraphQL: isResolved, isOutdated, and the
// mutation that resolves a thread have no REST equivalent
// (project_plan.md §1). These are the two hand-rolled requests that use it,
// rather than a dependency on a struct-tagged query package.

// maxThreadPages bounds how much thread history one call reads. A pull request
// with more threads than this is one where a later round should narrow with a
// path rather than read everything.
const maxThreadPages = 3

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(first: 100) {
            pageInfo { hasNextPage }
            nodes {
              databaseId
              author { login }
              body
              createdAt
              path
              line
            }
          }
        }
      }
    }
  }
}`

const resolveReviewThreadMutation = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`

// ThreadComment is one comment in a review thread.
type ThreadComment struct {
	ID        int64
	Author    string
	Body      string
	Path      string
	Line      int
	CreatedAt time.Time
}

// ReviewThread is one thread of review comments. Its earliest comment opened
// it, which is what decides whose thread it is.
type ReviewThread struct {
	ID         string
	Path       string
	Line       int
	IsResolved bool
	IsOutdated bool
	// HasMoreComments is true when the thread holds more replies than one page
	// of the query reads.
	HasMoreComments bool
	Comments        []ThreadComment
}

// Root returns the comment that opened the thread, which is the only one a
// resolution may be allowed to touch. Ownership is decided by the timestamp
// rather than by position in the list: the caller that lists threads for a
// resolution must not depend on the order the api happens to return comments
// in, because the check is what keeps a person's thread open.
func (t ReviewThread) Root() (ThreadComment, bool) {
	if len(t.Comments) == 0 {
		return ThreadComment{}, false
	}
	root := t.Comments[0]
	for _, comment := range t.Comments[1:] {
		if !comment.CreatedAt.IsZero() && (root.CreatedAt.IsZero() || comment.CreatedAt.Before(root.CreatedAt)) {
			root = comment
		}
	}
	return root, true
}

// Newest returns the timestamp of the thread's most recent comment, which is
// what orders threads newest first.
func (t ReviewThread) Newest() time.Time {
	newest := time.Time{}
	for _, comment := range t.Comments {
		if comment.CreatedAt.After(newest) {
			newest = comment.CreatedAt
		}
	}
	return newest
}

// ReviewThreads returns the pull request's review threads with their resolution
// state and their replies.
func (c *Client) ReviewThreads(ctx context.Context) ([]ReviewThread, error) {
	var (
		threads []ReviewThread
		cursor  string
	)
	for page := 0; page < maxThreadPages; page++ {
		var response struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads reviewThreadNodes `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		variables := map[string]any{"owner": c.owner, "name": c.repo, "number": c.number}
		if cursor != "" {
			variables["cursor"] = cursor
		}
		if err := c.graphql(ctx, reviewThreadsQuery, variables, &response); err != nil {
			return nil, err
		}

		nodes := response.Repository.PullRequest.ReviewThreads
		for _, node := range nodes.Nodes {
			threads = append(threads, node.thread())
		}
		if !nodes.PageInfo.HasNextPage {
			break
		}
		cursor = nodes.PageInfo.EndCursor
	}
	return threads, nil
}

// ErrThreadResolveUnsupported reports that GitHub refused the mutation that
// closes a review thread to this token.
//
// A GitHub App installation is answered FORBIDDEN "Resource not accessible by
// integration" for resolveReviewThread unless the App holds Contents: Read and
// write. `pull_requests: write` is not enough and does not stand in for it: the
// same mutation on the same thread is refused to an installation whose Contents
// permission is read, and resolved by an installation of an App whose Contents
// permission is write. The mutation is not closed to Apps as such, and the
// permission is not the repo's contents in any sense the operator can see from
// the pull request, which is why the message names it.
var ErrThreadResolveUnsupported = errors.New("github refused the mutation that closes a review thread")

// ResolveReviewThread marks a thread resolved. GitHub keeps the thread and its
// replies; what changes is that later rounds read it as a decision rather than
// an open question.
func (c *Client) ResolveReviewThread(ctx context.Context, threadID string) error {
	if threadID == "" {
		return fmt.Errorf("a thread id is required")
	}

	var response struct {
		ResolveReviewThread struct {
			Thread struct {
				ID         string `json:"id"`
				IsResolved bool   `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	err := c.graphql(ctx, resolveReviewThreadMutation, map[string]any{"threadId": threadID}, &response)
	if err != nil {
		var answered *graphqlError
		if errors.As(err, &answered) && answered.refusedToTheIntegration() {
			return fmt.Errorf("%w: %w", ErrThreadResolveUnsupported, err)
		}
		return err
	}
	if !response.ResolveReviewThread.Thread.IsResolved {
		return fmt.Errorf("github did not resolve thread %s", threadID)
	}
	return nil
}

// graphqlError is one error GitHub's GraphQL api answered with, kept as a type
// so a caller can tell a refusal apart from a request that did not work.
type graphqlError struct {
	message string
	kind    string
}

func (e *graphqlError) Error() string {
	return fmt.Sprintf("github graphql: %s (type %s)", e.message, e.kind)
}

// refusedToTheIntegration reports the answer GitHub gives an installation for
// an operation its integration may not perform.
func (e *graphqlError) refusedToTheIntegration() bool {
	return e.kind == "FORBIDDEN" && strings.Contains(e.message, "not accessible by integration")
}

// graphql posts one query and decodes its data. GraphQL answers a failed
// operation with HTTP 200 and an errors array, so the array is checked before
// the data is decoded.
func (c *Client) graphql(ctx context.Context, query string, variables map[string]any, data any) error {
	request, err := c.gh.NewRequest("POST", "graphql", map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("building the graphql request: %w", err)
	}

	var body struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if _, err := c.gh.Do(ctx, request, &body); err != nil {
		return fmt.Errorf("calling the github graphql api: %w", err)
	}
	if len(body.Errors) > 0 {
		return &graphqlError{message: body.Errors[0].Message, kind: body.Errors[0].Type}
	}
	if data == nil {
		return nil
	}
	if err := json.Unmarshal(body.Data, data); err != nil {
		return fmt.Errorf("decoding the graphql response: %w", err)
	}
	return nil
}

// reviewThreadNodes is the shape of one page of reviewThreads.
type reviewThreadNodes struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []threadNode `json:"nodes"`
}

type threadNode struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       *int   `json:"line"`
	Comments   struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []threadCommentNode `json:"nodes"`
	} `json:"comments"`
}

type threadCommentNode struct {
	DatabaseID int64  `json:"databaseId"`
	Body       string `json:"body"`
	Path       string `json:"path"`
	Line       *int   `json:"line"`
	CreatedAt  string `json:"createdAt"`
	Author     struct {
		Login string `json:"login"`
	} `json:"author"`
}

func (n threadNode) thread() ReviewThread {
	thread := ReviewThread{
		ID:              n.ID,
		Path:            n.Path,
		IsResolved:      n.IsResolved,
		IsOutdated:      n.IsOutdated,
		HasMoreComments: n.Comments.PageInfo.HasNextPage,
	}
	if n.Line != nil {
		thread.Line = *n.Line
	}
	for _, comment := range n.Comments.Nodes {
		thread.Comments = append(thread.Comments, ThreadComment{
			ID:        comment.DatabaseID,
			Author:    comment.Author.Login,
			Body:      comment.Body,
			Path:      comment.Path,
			Line:      deref(comment.Line),
			CreatedAt: parseTime(comment.CreatedAt),
		})
	}
	return thread
}

func deref(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// parseTime reads the RFC3339 timestamps the GraphQL api returns. A value that
// does not parse leaves the zero time rather than failing a whole history read
// over one comment.
func parseTime(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
