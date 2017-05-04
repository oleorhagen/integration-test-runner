package main

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/go-github/github"
	"github.com/yosida95/golang-jenkins"
	"golang.org/x/oauth2"
	"gopkg.in/gin-gonic/gin.v1"

	log "github.com/Sirupsen/logrus"
)

type config struct {
	username          string
	password          string
	baseURL           string
	githubSecret      []byte
	githubToken       string
	watchRepositories []string
}

func getConfig() config {
	var repositoryWatchList []string
	username := os.Getenv("JENKINS_USERNAME")
	password := os.Getenv("JENKINS_PASSWORD")
	url := os.Getenv("JENKINS_BASE_URL")
	githubSecret := os.Getenv("GITHUB_SECRET")
	githubToken := os.Getenv("GITHUB_TOKEN")

	defaultWatchRepositories := "deployments,deviceadm,deviceauth,inventory,useradm,integration,mender,meta-mender"
	watchRepositories := os.Getenv("WATCH_REPOS")

	if len(watchRepositories) == 0 {
		repositoryWatchList = strings.Split(defaultWatchRepositories, ",")
	} else {
		repositoryWatchList = strings.Split(watchRepositories, ",")
	}

	if username == "" {
		panic("set JENKINS_USERNAME")
	}

	if password == "" {
		panic("set JENKINS_PASSWORD")
	}

	if url == "" {
		panic("set JENKINS_BASE_URL")
	}

	if githubSecret == "" {
		panic("set GITHUB_SECRET")
	}

	if githubToken == "" {
		panic("set GITHUB_TOKEN")
	}

	return config{username, password, url, []byte(githubSecret), githubToken, repositoryWatchList}
}

func createGitHubClient(conf config) *github.Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: conf.githubToken},
	)
	tc := oauth2.NewClient(ctx, ts)

	return github.NewClient(tc)
}

func main() {
	conf := getConfig()
	log.Infoln("using config: ", spew.Sdump(conf))

	githubClient := createGitHubClient(conf)
	r := gin.Default()

	r.POST("/incoming", func(context *gin.Context) {
		payload, err := github.ValidatePayload(context.Request, conf.githubSecret)

		if err != nil {
			log.Warnln("payload failed to validate, ignoring.")
			return
		}

		event, _ := github.ParseWebHook(github.WebHookType(context.Request), payload)
		if github.WebHookType(context.Request) == "pull_request" {
			pr := event.(*github.PullRequestEvent)
			action := pr.GetAction()

			member, _, _ := githubClient.Organizations.IsMember(context, "mendersoftware", pr.Sender.GetLogin())
			if !member {
				log.Warnf("%s is making a pullrequest, but he's not a member of mendersoftware, ignoring", pr.Sender.GetLogin())
				return
			}

			log.Info("Pull request event with action: ", action)
			// github pull request events to trigger a jenkins job for
			if action == "opened" || action == "edited" || action == "reopened" || action == "synchronize" {
				repo := *pr.Repo.Name
				commitSHA := pr.PullRequest.Head.GetSHA()

				makeQEMU := false
				for _, watchRepo := range conf.watchRepositories {
					if watchRepo == repo {
						if repo == "mender" || repo == "meta-mender" {
							makeQEMU = true
						}
						go triggerBuild(conf, strconv.Itoa(pr.GetNumber()), repo, commitSHA, makeQEMU)
						return
					}
				}
			}
		}

	})
	r.Run("0.0.0.0:8081")
}

func triggerBuild(c config, pr, repo, commitSHA string, makeQEMU bool) error {
	auth := &gojenkins.Auth{
		Username: c.username,
		ApiToken: c.password,
	}

	jenkins := gojenkins.NewJenkins(auth, c.baseURL)
	job, err := jenkins.GetJob("mender-builder")

	if err != nil {
		return nil
	}

	// The parameter that jenkins uses for repo specific revisions is <REPO_NAME>_REV
	repoRevision := strings.ToUpper(repo) + "_REV"
	repoRevision = strings.Replace(repoRevision, "-", "_", -1)
	readHead := "pull/" + pr + "/head"

	buildParameter := url.Values{}
	buildParameter.Add("PR_TO_TEST", pr)
	buildParameter.Add("REPO_TO_TEST", repo)
	buildParameter.Add("GIT_COMMIT", commitSHA)
	buildParameter.Add(repoRevision, readHead)

	if makeQEMU {
		buildParameter.Add("TEST_QEMU", "true")
		buildParameter.Add("BUILD_QEMU", "true")
	}

	return jenkins.Build(job, buildParameter)
}
