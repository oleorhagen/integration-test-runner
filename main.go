package main

import (
	"fmt"
	"os"
	"strconv"
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
	log := getCustomLoggerFromContext(ctx)

	if webhookType == "pull_request" {
		pr := webhookEvent.(*github.PullRequestEvent)

		// Do not run if the PR is a draft
		if pr.GetPullRequest().GetDraft() {
			log.Infof("The PR: %s/%d is a draft. Do not run tests", pr.GetRepo().GetName(), pr.GetNumber())
			return nil
		}

		if isDependabotPR, err := maybeVendorDependabotPR(log, pr, conf); isDependabotPR {
			if err != nil {
				log.Errorf("maybeVendorDependabotPR: %v", err)
			}
			return err
		}

		action := pr.GetAction()

		// To run component's Pipeline create a branch in GitLab, regardless of the PR
		// coming from an organization member or not (equivalent to the old Travis tests)
		if err := createPullRequestBranch(log, *pr.Organization.Login, *pr.Repo.Name, strconv.Itoa(pr.GetNumber()), action, conf); err != nil {
			log.Errorf("Could not create PR branch: %s", err.Error())
		}

		// Delete merged pr branches in GitLab
		if err := deleteStaleGitlabPRBranch(log, pr, conf); err != nil {
			log.Errorf("Failed to delete the stale PR branch after the PR: %v was merged or closed. Error: %v", pr, err)
		}

		// make sure we only parse one pr at a time, since we use release_tool
		mutex.Lock()

		// If the pr was merged, suggest cherry-picks
		if err := suggestCherryPicks(log, pr, githubClient, conf); err != nil {
			log.Errorf("Failed to suggest cherry picks for the pr %v. Error: %v", pr, err)
		}

		// release the mutex
		mutex.Unlock()

		// Continue to the integration Pipeline only for organization members
		if member := githubClient.IsOrganizationMember(ctx, githubOrganization, pr.Sender.GetLogin()); !member {
			log.Warnf("%s is making a pullrequest, but he/she is not a member of our organization, ignoring", pr.Sender.GetLogin())
			return nil
		}

		// make sure we only parse one pr at a time, since we use release_tool
		mutex.Lock()

		// First check if the PR has been merged. If so, stop
		// the pipeline, and do nothing else.
		if err := stopBuildsOfStalePRs(log, pr, conf); err != nil {
			log.Errorf("Failed to stop a stale build after the PR: %v was merged or closed. Error: %v", pr, err)
		}

		// Keep the OS and Enterprise repos in sync
		if err := syncIfOSHasEnterpriseRepo(log, conf, pr); err != nil {
			log.Errorf("Failed to sync the OS and Enterprise repos: %s", err.Error())
		}

		// get the list of builds
		builds := parsePullRequest(log, conf, action, pr)
		log.Infof("%s:%d would trigger %d builds", pr.GetRepo().GetName(), pr.GetNumber(), len(builds))

		// release the mutex
		mutex.Unlock()

		// do not start the builds, inform the user about the `start pipeline` command instead
		if len(builds) > 0 {
			msg := "@" + pr.GetSender().GetLogin() + ", Let me know if you want to start the integration pipeline by mentioning me and the command \"" + commandStartPipeline + "\"."
			if err := githubClient.CreateComment(ctx, pr.GetOrganization().GetName(), pr.GetRepo().GetName(), pr.GetNumber(), &github.IssueComment{
				Body: github.String(msg),
			}); err != nil {
				log.Infof("Failed to comment on the pr: %v, Error: %s", pr, err.Error())
			}
		}
	} else if webhookType == "push" {
		push := webhookEvent.(*github.PushEvent)
		repoName := push.GetRepo().GetName()
		repoOrg := push.GetRepo().GetOrganization()
		refName := push.GetRef()
		log.Debugf("Got push event :: repo %s :: ref %s", repoName, refName)
		for _, repo := range conf.watchRepositoriesGitLabSync {
			if repoName == repo {
				err := syncRemoteRef(log, repoOrg, repoName, refName, conf)
				if err != nil {
					log.Errorf("Could not sync branch: %s", err.Error())
				}
				break
			}
		}
	} else if webhookType == "issue_comment" {
		comment := webhookEvent.(*github.IssueCommentEvent)

		// process created actions only, ignore the others
		action := comment.GetAction()
		if action != "created" {
			log.Infof("Ignoring action %s on comment", action)
			return nil
		}

		// accept commands only from organization members
		if member := githubClient.IsOrganizationMember(ctx, githubOrganization, comment.Sender.GetLogin()); !member {
			log.Warnf("%s commented, but he/she is not a member of our organization, ignoring", comment.Sender.GetLogin())
			return nil
		}

		// filter comments mentioning the bot
		body := comment.Comment.GetBody()
		if !strings.Contains(body, "@"+githubBotName) {
			log.Infof("ignoring comment not mentioning me")
			return nil
		}

		// extract the command and check it is valid
		command := ""
		if strings.Contains(body, commandStartPipeline) {
			command = commandStartPipeline
		}
		if command == "" {
			log.Warnf("no command found: %s", body)
			return nil
		}

		switch command {
		case commandStartPipeline:
			// retrieve the pull request
			prLink := comment.Issue.GetPullRequestLinks().GetURL()
			if prLink == "" {
				log.Warnf("ignoring comment not on a pull request")
				return nil
			}

			prLinkParts := strings.Split(prLink, "/")
			prNumber, err := strconv.Atoi(prLinkParts[len(prLinkParts)-1])
			if err != nil {
				log.Errorf("Unable to retrieve the pull request: %s", err.Error())
				return err
			}

			pr, err := githubClient.GetPullRequest(ctx, githubOrganization, comment.GetRepo().GetName(), prNumber)
			if err != nil {
				log.Errorf("Unable to retrieve the pull request: %s", err.Error())
				return err
			}

			// make sure we only parse one pr at a time, since we use release_tool
			mutex.Lock()

			// get the list of builds
			prRequest := &github.PullRequestEvent{Repo: comment.GetRepo(), PullRequest: pr}
			builds := parsePullRequest(log, conf, "opened", prRequest)
			log.Infof("%s:%d would trigger %d builds", comment.GetRepo().GetName(), pr.GetNumber(), len(builds))

			// release the mutex
			mutex.Unlock()

			// start the builds
			for idx, build := range builds {
				log.Infof("%d: "+spew.Sdump(build)+"\n", idx+1)
				if build.repo == "meta-mender" && build.baseBranch == "master-next" {
					log.Info("Skipping build targeting meta-mender:master-next")
					continue
				}
				if err := triggerBuild(log, conf, &build, prRequest); err != nil {
					log.Errorf("Could not start build: %s", err.Error())
				}
			}
		}
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
