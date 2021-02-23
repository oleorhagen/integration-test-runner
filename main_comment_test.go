package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/go-github/v28/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	mock_github "github.com/mendersoftware/integration-test-runner/client/github/mocks"
)

func TestProcessGitHubWebhook(t *testing.T) {
	testCases := map[string]struct {
		webhookType  string
		webhookEvent interface{}

		repo     string
		prNumber int

		isOrganizationMember *bool
		pullRequest          *github.PullRequest
		pullRequestErr       error

		err error
	}{
		"comment updated, ignore": {
			webhookType: "issue_comment",
			webhookEvent: &github.IssueCommentEvent{
				Action: github.String("updated"),
			},
		},
		"comment from non-mendersoftware user, ignore": {
			webhookType: "issue_comment",
			webhookEvent: &github.IssueCommentEvent{
				Action: github.String("created"),
				Sender: &github.User{
					Login: github.String("not-member"),
				},
			},

			isOrganizationMember: github.Bool(false),
		},
		"comment from organization user, missing mention": {
			webhookType: "issue_comment",
			webhookEvent: &github.IssueCommentEvent{
				Action: github.String("created"),
				Comment: &github.IssueComment{
					Body: github.String("@friend start pipeline"),
				},
				Sender: &github.User{
					Login: github.String("not-member"),
				},
			},

			isOrganizationMember: github.Bool(false),
		},
		"comment from organization user, command not recognized": {
			webhookType: "issue_comment",
			webhookEvent: &github.IssueCommentEvent{
				Action: github.String("created"),
				Comment: &github.IssueComment{
					Body: github.String("@" + githubBotName + " dummy"),
				},
				Sender: &github.User{
					Login: github.String("not-member"),
				},
			},

			isOrganizationMember: github.Bool(false),
		},
		"comment from organization user, no pull request associated": {
			webhookType: "issue_comment",
			webhookEvent: &github.IssueCommentEvent{
				Action: github.String("created"),
				Comment: &github.IssueComment{
					Body: github.String("@" + githubBotName + " start pipeline"),
				},
				Sender: &github.User{
					Login: github.String("member"),
				},
			},

			isOrganizationMember: github.Bool(true),
		},
		"comment from organization user, wrong pull request link": {
			webhookType: "issue_comment",
			webhookEvent: &github.IssueCommentEvent{
				Action: github.String("created"),
				Comment: &github.IssueComment{
					Body: github.String("@" + githubBotName + " start pipeline"),
				},
				Issue: &github.Issue{
					PullRequestLinks: &github.PullRequestLinks{
						URL: github.String("https://api.github.com/repos/mendersoftware/integration-test-runner/pulls/a"),
					},
				},
				Sender: &github.User{
					Login: github.String("member"),
				},
			},

			isOrganizationMember: github.Bool(true),

			err: errors.New("strconv.Atoi: parsing \"a\": invalid syntax"),
		},
		"comment from organization user, error retrieving pull request": {
			webhookType: "issue_comment",
			webhookEvent: &github.IssueCommentEvent{
				Action: github.String("created"),
				Comment: &github.IssueComment{
					Body: github.String("@" + githubBotName + " start pipeline"),
				},
				Issue: &github.Issue{
					PullRequestLinks: &github.PullRequestLinks{
						URL: github.String("https://api.github.com/repos/mendersoftware/integration-test-runner/pulls/78"),
					},
				},
				Repo: &github.Repository{
					Name: github.String("integration-test-runner"),
				},
				Sender: &github.User{
					Login: github.String("member"),
				},
			},

			isOrganizationMember: github.Bool(true),

			repo:     "integration-test-runner",
			prNumber: 78,

			pullRequestErr: errors.New("generic error"),
			err:            errors.New("generic error"),
		},
		"comment from organization user, start the builds": {
			webhookType: "issue_comment",
			webhookEvent: &github.IssueCommentEvent{
				Action: github.String("created"),
				Comment: &github.IssueComment{
					Body: github.String("@" + githubBotName + " start pipeline"),
				},
				Issue: &github.Issue{
					PullRequestLinks: &github.PullRequestLinks{
						URL: github.String("https://api.github.com/repos/mendersoftware/integration-test-runner/pulls/78"),
					},
				},
				Repo: &github.Repository{
					Name: github.String("integration-test-runner"),
				},
				Sender: &github.User{
					Login: github.String("member"),
				},
			},

			isOrganizationMember: github.Bool(true),

			repo:     "integration-test-runner",
			prNumber: 78,

			pullRequest: &github.PullRequest{
				Base: &github.PullRequestBranch{
					Label: github.String("user:branch"),
				},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			mclient := &mock_github.Client{}
			defer mclient.AssertExpectations(t)

			if tc.isOrganizationMember != nil {
				mclient.On("IsOrganizationMember",
					mock.MatchedBy(func(ctx context.Context) bool {
						return true
					}),
					githubOrganization,
					tc.webhookEvent.(*github.IssueCommentEvent).GetSender().GetLogin(),
				).Return(*tc.isOrganizationMember)
			}

			if tc.pullRequest != nil || tc.pullRequestErr != nil {
				mclient.On("GetPullRequest",
					mock.MatchedBy(func(ctx context.Context) bool {
						return true
					}),
					githubOrganization,
					tc.repo,
					tc.prNumber,
				).Return(tc.pullRequest, tc.pullRequestErr)
			}

			conf := &config{
				githubProtocol: GitProtocolHTTP,
			}

			ctx := &gin.Context{}
			ctx.Set("delivery", "dummy")

			err := processGitHubWebhook(ctx, tc.webhookType, tc.webhookEvent, mclient, conf)
			if tc.err != nil {
				assert.EqualError(t, err, tc.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
