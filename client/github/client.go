package github

import (
	"context"

	"github.com/google/go-github/v28/github"
	"golang.org/x/oauth2"
)

// GitHubClient represents a GitHub client
type Client interface {
	CreateComment(ctx context.Context, org string, repo string, number int, comment *github.IssueComment) error
	IsOrganizationMember(ctx context.Context, org string, user string) bool
	CreatePullRequest(ctx context.Context, org string, repo string, pr *github.NewPullRequest) (*github.PullRequest, error)
	GetPullRequest(ctx context.Context, org string, repo string, pr int) (*github.PullRequest, error)
}

type gitHubClient struct {
	client *github.Client
}

// NewGitHubClient returns a new GitHubClient for the given conf
func NewGitHubClient(accessToken string) Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: accessToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	return &gitHubClient{
		client: client,
	}
}

func (c *gitHubClient) CreateComment(ctx context.Context, org string, repo string, number int, comment *github.IssueComment) error {
	_, _, err := c.client.Issues.CreateComment(ctx, org, repo, number, comment)
	return err
}

func (c *gitHubClient) IsOrganizationMember(ctx context.Context, org string, user string) bool {
	res, _, _ := c.client.Organizations.IsMember(ctx, org, user)
	return res
}

func (c *gitHubClient) CreatePullRequest(ctx context.Context, org string, repo string, pr *github.NewPullRequest) (*github.PullRequest, error) {
	newPR, _, err := c.client.PullRequests.Create(ctx, org, repo, pr)
	return newPR, err
}

func (c *gitHubClient) GetPullRequest(ctx context.Context, org string, repo string, pr int) (*github.PullRequest, error) {
	newPR, _, err := c.client.PullRequests.Get(ctx, org, repo, pr)
	return newPR, err
}
