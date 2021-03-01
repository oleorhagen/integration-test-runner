package main

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/davecgh/go-spew/spew"
	"github.com/gin-gonic/gin"
	"github.com/google/go-github/v28/github"
	clientgithub "github.com/mendersoftware/integration-test-runner/client/github"

	"github.com/sirupsen/logrus"
)

var mutex = &sync.Mutex{}

type config struct {
	githubSecret                     []byte
	githubProtocol                   GitProtocol
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
	"deviceconfig",
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
	// websites
	"mender.io",
	"northern-tech-web",
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

const (
	githubOrganization = "mendersoftware"
	githubBotName      = "mender-test-bot"
)

const (
	commandStartPipeline = "start pipeline"
)

func getConfig() (*config, error) {
	var repositoryWatchListPipeline []string
	var repositoryWatchListSync []string
	githubSecret := os.Getenv("GITHUB_SECRET")
	githubToken := os.Getenv("GITHUB_TOKEN")
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	gitlabBaseURL := os.Getenv("GITLAB_BASE_URL")
	integrationDirectory := os.Getenv("INTEGRATION_DIRECTORY")
	logLevel, found := os.LookupEnv("INTEGRATION_TEST_RUNNER_LOG_LEVEL")

	logrus.SetLevel(logrus.InfoLevel)

	if found {
		lvl, err := logrus.ParseLevel(logLevel)
		if err != nil {
			logrus.Infof("Failed to parse the 'INTEGRATION_TEST_RUNNER_LOG_LEVEL' variable, defaulting to 'InfoLevel'")
		} else {
			logrus.Infof("Set 'LogLevel' to %s", lvl)
			logrus.SetLevel(lvl)
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
		githubProtocol:                   GitProtocolSSH,
		githubToken:                      githubToken,
		gitlabToken:                      gitlabToken,
		gitlabBaseURL:                    gitlabBaseURL,
		integrationDirectory:             integrationDirectory,
		watchRepositoriesTriggerPipeline: repositoryWatchListPipeline,
		watchRepositoriesGitLabSync:      repositoryWatchListSync,
	}, nil
}

func init() {
	// Log to stdout and with JSON format; suitable for GKE
	formatter := &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "time",
			logrus.FieldKeyLevel: "level",
			logrus.FieldKeyMsg:   "message",
		},
	}

	logrus.SetOutput(os.Stdout)
	logrus.SetFormatter(formatter)
}

func getCustomLoggerFromContext(ctx *gin.Context) *logrus.Entry {
	deliveryID, ok := ctx.Get("delivery")
	if !ok {
		return nil
	}
	return logrus.WithField("delivery", deliveryID)
}

func processGitHubWebhookRequest(ctx *gin.Context, payload []byte, githubClient clientgithub.Client, conf *config) {
	webhookType := github.WebHookType(ctx.Request)
	webhookEvent, _ := github.ParseWebHook(github.WebHookType(ctx.Request), payload)
	_ = processGitHubWebhook(ctx, webhookType, webhookEvent, githubClient, conf)
}

func processGitHubWebhook(ctx *gin.Context, webhookType string, webhookEvent interface{}, githubClient clientgithub.Client, conf *config) error {
	switch webhookType {
	case "pull_request":
		pr := webhookEvent.(*github.PullRequestEvent)
		return processGitHubPullRequest(ctx, pr, githubClient, conf)
	case "push":
		push := webhookEvent.(*github.PushEvent)
		return processGitHubPush(ctx, push, githubClient, conf)
	case "issue_comment":
		comment := webhookEvent.(*github.IssueCommentEvent)
		return processGitHubComment(ctx, comment, githubClient, conf)
	}
	return nil
}

func main() {
	conf, err := getConfig()

	if err != nil {
		logrus.Fatalf("failed to load config: %s", err.Error())
	}

	logrus.Infoln("using settings: ", spew.Sdump(conf))

	githubClient := clientgithub.NewGitHubClient(conf.githubToken)
	r := gin.Default()

	// webhook for GitHub
	r.POST("/", func(context *gin.Context) {
		payload, err := github.ValidatePayload(context.Request, conf.githubSecret)
		if err != nil {
			logrus.Warnln("payload failed to validate, ignoring.")
			return
		}
		context.Set("delivery", github.DeliveryID(context.Request))
		go processGitHubWebhookRequest(context, payload, githubClient, conf)
	})

	// 200 replay for the loadbalancer
	r.GET("/", func(_ *gin.Context) {})

	_ = r.Run("0.0.0.0:8080")
}
