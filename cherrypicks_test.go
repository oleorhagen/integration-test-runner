package main

import (
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
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

	for name, test := range testCases {
		t.Run(name, func(t *testing.T) {
			mclient := &mock_github.Client{}
			defer mclient.AssertExpectations(t)

			if test.comment != nil {
				mclient.On("CreateComment",
					mock.MatchedBy(func(ctx context.Context) bool {
						return true
					}),
					githubOrganization,
					*test.pr.Repo.Name,
					*test.pr.Number,
					test.comment,
				).Return(nil)
			}

			conf := &config{
				githubProtocol: GitProtocolHTTP,
			}
			conf.integrationDirectory = tmpdir

			log := logrus.NewEntry(logrus.StandardLogger())
			err := suggestCherryPicks(log, test.pr, mclient, conf)
			if test.err != nil {
				assert.Error(t, err)
				assert.EqualError(t, err, test.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCherryTargetBranches(t *testing.T) {
	tests := map[string]struct {
		input    string
		expected []string
	}{
		"Success": {
			input: `
cherry pick to:
    * 2.6.x
    * 2.5.x
    * 2.4.x
`,
			expected: []string{"2.6.x", "2.5.x", "2.4.x"},
		},
		"Failure": {
			input: `cherry pick to:
 * 2.4.1
* 2.5.3`,
			expected: []string{},
		},
	}

	for name, test := range tests {
		t.Log(name)
		output := parseCherryTargetBranches(test.input)
		assert.Equal(t, test.expected, output)
	}
}

func TestCherryPickToReleaseBranches(t *testing.T) {

	os.Setenv("MENDERGITDRY", "--dry-run")

	tests := map[string]struct {
		pr       *github.PullRequest
		err      error
		comment  *github.IssueCommentEvent
		body     string
		expected []string
	}{
		"cherry picks, changelogs": {
			pr: &github.PullRequest{
				Number: github.Int(749),
				Base: &github.PullRequestBranch{
					Ref: github.String("master"),
					SHA: github.String("04670761d39da501361501e2a4e96581b0645225"),
				},
				Head: &github.PullRequestBranch{
					Ref: github.String("pr-branch"),
					SHA: github.String("33375381a411a07429cac9fb6f800814e21dc2b8"),
				},
				Merged: github.Bool(true),
			},
			comment: &github.IssueCommentEvent{
				Issue: &github.Issue{
					Title: github.String("MEN-4703"),
				},
				Repo: &github.Repository{
					Name: github.String("mender"),
				},
				Comment: &github.IssueComment{
					Body: github.String(`
cherry-pick to:
* 2.6.x
* 2.5.x
* 2.4.x
`),
				},
			},
			body: `
cherry-pick to:
* 2.6.x
* 2.5.x
* 2.4.x
`,
			expected: []string{`I did my very best, and this is the result of the cherry pick operation:`,
				`* 2.6.x Had merge conflicts, you will have to fix this yourself :crying_cat_face:`,
				`* 2.5.x :white_check_mark: #749`,
				`* 2.4.x Had merge conflicts, you will have to fix this yourself :crying_cat_face:`,
			},
		},
	}

	tmpdir, err := ioutil.TempDir("", "TestCherryPickToRelease")
	if err != nil {
		t.FailNow()
	}
	defer os.RemoveAll(tmpdir)

	gitSetup := exec.Command("git", "clone", "https://github.com/mendersoftware/integration.git", tmpdir)
	gitSetup.Dir = tmpdir
	_, err = gitSetup.CombinedOutput()
	if err != nil {
		t.FailNow()
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			mclient := &mock_github.Client{}
			defer mclient.AssertExpectations(t)

			mclient.On("CreateComment",
				mock.MatchedBy(func(ctx context.Context) bool {
					return true
				}),
				githubOrganization,
				*test.comment.Repo.Name,
				*test.pr.Number,
				mock.MatchedBy(func(i *github.IssueComment) bool {
					for _, expected := range test.expected {
						if !strings.Contains(*i.Body, expected) {
							return false
						}
					}
					return true
				}),
			).Return(nil)

			mclient.On("CreatePullRequest",
				mock.MatchedBy(func(ctx context.Context) bool {
					return true
				}),
				githubOrganization,
				test.comment.GetRepo().GetName(),
				mock.MatchedBy(func(_ *github.NewPullRequest) bool { return true }),
			).Return(nil, nil)

			conf := &config{
				githubProtocol: GitProtocolHTTP,
			}
			conf.integrationDirectory = tmpdir

			log := logrus.NewEntry(logrus.StandardLogger())

			err = cherryPickPR(log, test.comment, test.pr, conf, test.body, mclient)

			if test.err != nil {
				assert.Error(t, err)
				assert.EqualError(t, err, test.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
