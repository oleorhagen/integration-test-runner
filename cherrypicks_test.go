package main

import (
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"testing"

	"github.com/google/go-github/v28/github"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	mock_github "github.com/mendersoftware/integration-test-runner/client/github/mocks"
)

func TestSuggestCherryPicks(t *testing.T) {
	testCases := map[string]struct {
		pr      *github.PullRequestEvent
		err     error
		comment *github.IssueComment
	}{
		"no cherry picks, not closed": {
			pr: &github.PullRequestEvent{
				Action: github.String("opened"),
				PullRequest: &github.PullRequest{
					Merged: github.Bool(false),
				},
			},
		},
		"no cherry picks, closed but not merged": {
			pr: &github.PullRequestEvent{
				Action: github.String("closed"),
				PullRequest: &github.PullRequest{
					Merged: github.Bool(false),
				},
			},
		},
		"no cherry picks, ref not master": {
			pr: &github.PullRequestEvent{
				Action: github.String("closed"),
				PullRequest: &github.PullRequest{
					Base: &github.PullRequestBranch{
						Ref: github.String("branch"),
					},
					Merged: github.Bool(true),
				},
				Repo: &github.Repository{
					Name: github.String("workflows"),
				},
			},
		},
		"no cherry picks, no changelogs": {
			pr: &github.PullRequestEvent{
				Action: github.String("closed"),
				Number: github.Int(113),
				PullRequest: &github.PullRequest{
					Base: &github.PullRequestBranch{
						Ref: github.String("master"),
						SHA: github.String("c5f65511d5437ae51da9c2e1c9017587d51044c8"),
					},
					Merged: github.Bool(true),
				},
				Repo: &github.Repository{
					Name: github.String("workflows"),
				},
			},
		},
		"cherry picks, changelogs": {
			pr: &github.PullRequestEvent{
				Action: github.String("closed"),
				Number: github.Int(88),
				PullRequest: &github.PullRequest{
					Base: &github.PullRequestBranch{
						Ref: github.String("master"),
						SHA: github.String("2294fae512f81d781b65b67844182ffb97240e83"),
					},
					Merged: github.Bool(true),
				},
				Repo: &github.Repository{
					Name: github.String("workflows"),
				},
			},
			comment: &github.IssueComment{
				Body: github.String(`
Hello :smile_cat: This PR contains changelog entries. Please, verify the need of backporting it to the following release branches:
1.4.x (release 2.7.x)
1.3.x (release 2.6.x)
1.1.x (release 2.4.x)
`),
			},
		},
		"cherry picks, changelogs, less than three release branches": {
			pr: &github.PullRequestEvent{
				Action: github.String("closed"),
				Number: github.Int(18),
				PullRequest: &github.PullRequest{
					Base: &github.PullRequestBranch{
						Ref: github.String("master"),
						SHA: github.String("11cc44037981d16e087b11ab7d6afdffae73e74e"),
					},
					Merged: github.Bool(true),
				},
				Repo: &github.Repository{
					Name: github.String("mender-connect"),
				},
			},
			comment: &github.IssueComment{
				Body: github.String(`
Hello :smile_cat: This PR contains changelog entries. Please, verify the need of backporting it to the following release branches:
1.1.x (release 2.7.x)
1.0.x (release 2.6.x)
`),
			},
		},
		"cherry picks, changelogs, syntax with no space": {
			pr: &github.PullRequestEvent{
				Action: github.String("closed"),
				Number: github.Int(29),
				PullRequest: &github.PullRequest{
					Base: &github.PullRequestBranch{
						Ref: github.String("master"),
						SHA: github.String("c138b0256ec874bcd16d4cae4b598b8615b2d415"),
					},
					Merged: github.Bool(true),
				},
				Repo: &github.Repository{
					Name: github.String("mender-connect"),
				},
			},
			comment: &github.IssueComment{
				Body: github.String(`
Hello :smile_cat: This PR contains changelog entries. Please, verify the need of backporting it to the following release branches:
1.1.x (release 2.7.x)
1.0.x (release 2.6.x)
`),
			},
		},
	}

	tmpdir, err := ioutil.TempDir("", "*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpdir)

	gitSetup := exec.Command("git", "clone", "https://github.com/mendersoftware/integration.git", tmpdir)
	gitSetup.Dir = tmpdir
	_, err = gitSetup.CombinedOutput()
	if err != nil {
		panic(err)
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			mclient := &mock_github.Client{}
			defer mclient.AssertExpectations(t)

			if tc.comment != nil {
				mclient.On("CreateComment",
					mock.MatchedBy(func(ctx context.Context) bool {
						return true
					}),
					githubOrganization,
					*tc.pr.Repo.Name,
					*tc.pr.Number,
					tc.comment,
				).Return(nil)
			}

			conf := &config{
				githubProtocol: GitProtocolHTTP,
			}
			conf.integrationDirectory = tmpdir

			log := logrus.NewEntry(logrus.StandardLogger())
			err := suggestCherryPicks(log, tc.pr, mclient, conf)
			if tc.err != nil {
				assert.Error(t, err)
				assert.EqualError(t, err, tc.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
