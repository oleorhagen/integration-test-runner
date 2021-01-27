package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/davecgh/go-spew/spew"
	"github.com/gin-gonic/gin"
	"github.com/google/go-github/v28/github"
	"golang.org/x/oauth2"

	log "github.com/sirupsen/logrus"
)

var mutex = &sync.Mutex{}

type config struct {
	githubSecret                     []byte
	githubToken                      string
	gitlabToken                      string
	gitlabBaseURL                    string
	integrationDirectory             string
	watchRepositoriesTriggerPipeline []string // List of repositories for which to trigger mender-qa pipeline
	watchRepositoriesGitLabSync      []string // List of repositories for which to trigger GitHub->Gitlab branches sync
}

type buildOptions struct {
	pr         string
	repo       string
	baseBranch string
	commitSHA  string
	makeQEMU   bool
}

// List of repos for which the integration pipeline shall be run
// It can be overridden with env. variable WATCH_REPOS_PIPELINE
var defaultWatchRepositoriesPipeline = []string{
	"create-artifact-worker",
	"deployments",
	"deployments-enterprise",
	"deviceadm",
	"deviceauth",
	"inventory",
	"inventory-enterprise",
	"integration",
	"mender",
	"mender-artifact",
	"mender-conductor",
	"mender-conductor-enterprise",
	"meta-mender",
	"mender-api-gateway-docker",
	"tenantadm",
	"useradm",
	"useradm-enterprise",
	"workflows",
	"workflows-enterprise",
	"auditlogs",
	"mtls-ambassador",
	"mender-connect",
	"deviceconnect",
}

// Mapping https://github.com/<org> -> https://gitlab.com/Northern.tech/<group>
var gitHubOrganizationToGitLabGroup = map[string]string{
	"mendersoftware": "Mender",
	"cfengine":       "CFEngine",
}

// Mapping of special repos that have a custom group/project
var gitHubRepoToGitLabProjectCustom = map[string]string{
	"saas": "Northern.tech/MenderSaaS/saas",
}

// List of repos for which the GitHub->Gitlab sync shall be performed.
// It can be overridden with env. variable WATCH_REPOS_SYNC
var defaultWatchRepositoriesSync = []string{
	// backend
	"deployments-enterprise",
	"inventory-enterprise",
	"tenantadm",
	"useradm-enterprise",
	"workflows-enterprise",
	"mender-conductor-enterprise",
	"mender-helm",
	"auditlogs",
	"mtls-ambassador",
	// client
	"mender-binary-delta",
	// docs
	"mender-docs-site",
	"mender-api-docs",
	// saas
	"saas",
	"saas-tools",
	"sre-tools",
	// mender-qa is in fact an open source repo but the project
	// in GitLab is kept private; hence it requires manual sync
	"mender-qa",
}

var qemuBuildRepositories = []string{
	"meta-mender",
	"mender",
	"mender-artifact",
	"mender-connect",
}

const (
	GIT_OPERATION_TIMEOUT = 30
)

const (
	featureBranchPrefix = "feature-"
)

func initLogger() {
	// Log to stdout and with JSON format; suitable for GKE
	formatter := &log.JSONFormatter{
		FieldMap: log.FieldMap{
			log.FieldKeyTime:  "time",
			log.FieldKeyLevel: "level",
			log.FieldKeyMsg:   "message",
		},
	}

	log.SetOutput(os.Stdout)
	log.SetFormatter(formatter)
}

func getConfig() (*config, error) {
	var repositoryWatchListPipeline []string
	var repositoryWatchListSync []string
	githubSecret := os.Getenv("GITHUB_SECRET")
	githubToken := os.Getenv("GITHUB_TOKEN")
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	gitlabBaseURL := os.Getenv("GITLAB_BASE_URL")
	integrationDirectory := os.Getenv("INTEGRATION_DIRECTORY")
	logLevel, found := os.LookupEnv("INTEGRATION_TEST_RUNNER_LOG_LEVEL")

	log.SetLevel(log.InfoLevel)

	if found {
		lvl, err := log.ParseLevel(logLevel)
		if err != nil {
			log.Infof("Failed to parse the 'INTEGRATION_TEST_RUNNER_LOG_LEVEL' variable, defaulting to 'InfoLevel'")
		} else {
			log.Infof("Set 'LogLevel' to %s", lvl)
			log.SetLevel(lvl)
		}
	}

	watchRepositoriesTriggerPipeline, ok := os.LookupEnv("WATCH_REPOS_PIPELINE")

	if ok {
		repositoryWatchListPipeline = strings.Split(watchRepositoriesTriggerPipeline, ",")
	} else {
		repositoryWatchListPipeline = defaultWatchRepositoriesPipeline
	}

	watchRepositoriesGitLabSync, ok := os.LookupEnv("WATCH_REPOS_SYNC")

	if ok {
		repositoryWatchListSync = strings.Split(watchRepositoriesGitLabSync, ",")
	} else {
		repositoryWatchListSync = defaultWatchRepositoriesSync
	}

	switch {
	case githubSecret == "":
		return &config{}, fmt.Errorf("set GITHUB_SECRET")
	case githubToken == "":
		return &config{}, fmt.Errorf("set GITHUB_TOKEN")
	case gitlabToken == "":
		return &config{}, fmt.Errorf("set GITLAB_TOKEN")
	case gitlabBaseURL == "":
		return &config{}, fmt.Errorf("set GITLAB_BASE_URL")
	case integrationDirectory == "":
		return &config{}, fmt.Errorf("set INTEGRATION_DIRECTORY")
	}

	return &config{
		githubSecret:                     []byte(githubSecret),
		githubToken:                      githubToken,
		gitlabToken:                      gitlabToken,
		gitlabBaseURL:                    gitlabBaseURL,
		integrationDirectory:             integrationDirectory,
		watchRepositoriesTriggerPipeline: repositoryWatchListPipeline,
		watchRepositoriesGitLabSync:      repositoryWatchListSync,
	}, nil
}

func createGitHubClient(conf *config) *github.Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: conf.githubToken},
	)

	tc := oauth2.NewClient(ctx, ts)
	return github.NewClient(tc)
}

func main() {

	initLogger()

	conf, err := getConfig()

	if err != nil {
		log.Fatalf("failed to load config: %s", err.Error())
	}

	log.Infoln("using settings: ", spew.Sdump(conf))

	githubClient := createGitHubClient(conf)
	r := gin.Default()

	// webhook for GitHub
	r.POST("/", func(context *gin.Context) {
		payload, err := github.ValidatePayload(context.Request, conf.githubSecret)

		if err != nil {
			log.Warnln("payload failed to validate, ignoring.")
			return
		}

		event, _ := github.ParseWebHook(github.WebHookType(context.Request), payload)
		if github.WebHookType(context.Request) == "pull_request" {
			pr := event.(*github.PullRequestEvent)

			// Do not run if the PR is a draft
			if pr.GetPullRequest().GetDraft() {
				log.Infof("The PR: %s/%d is a draft. Do not run tests", pr.GetRepo().GetName(), pr.GetNumber())
				return
			}

			isDependabotPR, err := maybeVendorDependabotPR(pr)
			if isDependabotPR {
				if err != nil {
					log.Errorf("maybeVendorDependabotPR: %v", err)
				}
				return
			}

			action := pr.GetAction()

			// To run component's Pipeline create a branch in GitLab, regardless of the PR
			// coming from a mendersoftware member or not (equivalent to the old Travis tests)
			err = createPullRequestBranch(*pr.Organization.Login, *pr.Repo.Name, strconv.Itoa(pr.GetNumber()), action)
			if err != nil {
				log.Errorf("Could not create PR branch: %s", err.Error())
			}

			// Continue to the integration Pipeline only for mendersoftware members
			if member, _, _ := githubClient.Organizations.IsMember(context, "mendersoftware", pr.Sender.GetLogin()); !member {
				log.Warnf("%s is making a pullrequest, but he/she is not a member of mendersoftware, ignoring", pr.Sender.GetLogin())
				return
			}

			// make sure we only parse one pr at a time, since we use release_tool
			mutex.Lock()

			builds := parsePullRequest(conf, action, pr)
			log.Infof("%s:%d triggered %d builds: \n", *pr.Repo.Name, pr.GetNumber(), len(builds))

			// First check if the PR has been merged. If so, stop
			// the pipeline, and do nothing else.
			if err = stopBuildsOfStalePRs(pr, conf); err != nil {
				log.Errorf("Failed to stop a stale build after the PR: %v was merged or closed. Error: %v", pr, err)
			}

			// Keep the OS and Enterprise repos in sync
			if err = syncIfOSHasEnterpriseRepo(conf, pr); err != nil {
				log.Errorf("Failed to sync the OS and Enterprise repos: %s", err.Error())
			}
			mutex.Unlock()

			for idx, build := range builds {
				log.Infof("%d: "+spew.Sdump(build)+"\n", idx+1)
				if build.repo == "meta-mender" && build.baseBranch == "master-next" {
					log.Info("Skipping build targeting meta-mender:master-next")
					continue
				}
				err = triggerBuild(conf, &build, pr)
				if err != nil {
					log.Errorf("Could not start build: %s", err.Error())
				}
			}
		} else if github.WebHookType(context.Request) == "push" {
			push := event.(*github.PushEvent)
			repoName := push.GetRepo().GetName()
			repoOrg := push.GetRepo().GetOrganization()
			refName := push.GetRef()
			log.Debugf("Got push event :: repo %s :: ref %s", repoName, refName)
			for _, repo := range conf.watchRepositoriesGitLabSync {
				if repoName == repo {
					err = syncRemoteRef(repoOrg, repoName, refName)
					if err != nil {
						log.Errorf("Could not sync branch: %s", err.Error())
					}
					break
				}
			}
		}
	})

	// 200 replay for the loadbalancer
	r.GET("/", func(_ *gin.Context) {})

	r.Run("0.0.0.0:8080")
}
