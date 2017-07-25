package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/go-github/github"
	"github.com/yosida95/golang-jenkins"
	"golang.org/x/oauth2"
	"gopkg.in/gin-gonic/gin.v1"

	log "github.com/Sirupsen/logrus"
)

var mutex = &sync.Mutex{}

type config struct {
	username                   string
	password                   string
	baseURL                    string
	githubSecret               []byte
	githubToken                string
	watchRepositories          []string
	integrationBranchDependant []string
	integrationDirectory       string
}

type buildOptions struct {
	pr             string
	repo           string
	baseBranch     string
	commitSHA      string
	makeQEMU       bool
	pushContainers bool
}

const (
	GIT_OPERATION_TIMEOUT = 30
)

func getConfig() (*config, error) {
	var repositoryWatchList []string
	username := os.Getenv("JENKINS_USERNAME")
	password := os.Getenv("JENKINS_PASSWORD")
	url := os.Getenv("JENKINS_BASE_URL")
	githubSecret := os.Getenv("GITHUB_SECRET")
	githubToken := os.Getenv("GITHUB_TOKEN")
	integrationDirectory := os.Getenv("INTEGRATION_DIRECTORY")

	// repos that require to be tested with specific integration branches
	integrationBranchDependant :=
		[]string{
			"deployments",
			"deviceadm",
			"deviceauth",
			"useradm",
			"integration",
			"inventory",
			"mender-api-gateway-docker",
			"mender",
		}

	// if no env. variable is set, this is the default repo watch list
	defaultWatchRepositories :=
		[]string{
			"deployments",
			"deviceadm",
			"deviceauth",
			"inventory",
			"useradm",
			"integration",
			"mender",
			"mender-qa",
			"mender-artifact",
			"meta-mender",
			"mender-api-gateway-docker"}

	watchRepositories := os.Getenv("WATCH_REPOS")

	if len(watchRepositories) == 0 {
		repositoryWatchList = defaultWatchRepositories
	} else {
		repositoryWatchList = strings.Split(watchRepositories, ",")
	}

	switch {
	case username == "":
		return &config{}, fmt.Errorf("set JENKINS_USERNAME")
	case password == "":
		return &config{}, fmt.Errorf("set JENKINS_PASSWORD")
	case url == "":
		return &config{}, fmt.Errorf("set JENKINS_BASE_URL")
	case githubSecret == "":
		return &config{}, fmt.Errorf("set GITHUB_SECRET")
	case githubToken == "":
		return &config{}, fmt.Errorf("set GITHUB_TOKEN")
	case integrationDirectory == "":
		return &config{}, fmt.Errorf("set INTEGRATION_DIRECTORY")
	}

	return &config{
		username:                   username,
		password:                   password,
		baseURL:                    url,
		githubSecret:               []byte(githubSecret),
		githubToken:                githubToken,
		watchRepositories:          repositoryWatchList,
		integrationBranchDependant: integrationBranchDependant,
		integrationDirectory:       integrationDirectory,
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
	conf, err := getConfig()

	if err != nil {
		log.Fatalf("failed to load config: %s", err.Error())
	}

	log.Infoln("using settings: ", spew.Sdump(conf))

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

			if member, _, _ := githubClient.Organizations.IsMember(context, "mendersoftware", pr.Sender.GetLogin()); !member {
				log.Warnf("%s is making a pullrequest, but he's not a member of mendersoftware, ignoring", pr.Sender.GetLogin())
				return
			}

			action := pr.GetAction()

			// make sure we only parse one pr at a time, since we use git
			mutex.Lock()
			builds := parsePullRequest(conf, action, pr)
			mutex.Unlock()

			for _, build := range builds {
				go triggerBuild(conf, &build)
			}

		} else if github.WebHookType(context.Request) == "pull_request_review" {
			prReview := event.(*github.PullRequestReviewEvent)
			member, _, _ := githubClient.Organizations.IsMember(context, "mendersoftware", prReview.PullRequest.User.GetLogin())

			if !member {
				log.Warnf("%s is making a pullrequest review event, but he's not a member of mendersoftware, ignoring", prReview.PullRequest.User.GetLogin())
				return
			}
		}

	})
	r.Run("0.0.0.0:8081")
}

func parsePullRequest(conf *config, action string, pr *github.PullRequestEvent) []buildOptions {
	log.Info("Pull request event with action: ", action)
	builds := make([]buildOptions, 0)

	repo := *pr.Repo.Name
	commitSHA := pr.PullRequest.Head.GetSHA()

	// github pull request events to trigger a jenkins job for
	if action == "opened" || action == "edited" || action == "reopened" || action == "synchronize" {

		//GetLabel returns "mendersoftware:master", we just want the branch
		baseBranch := strings.Split(pr.PullRequest.Base.GetLabel(), ":")[1]

		makeQEMU := false
		buildContainers := false

		for _, watchRepo := range conf.watchRepositories {
			// make sure the repo that the pull request is performed against is
			// one that we are watching.

			if watchRepo == repo {
				if repo == "mender" || repo == "meta-mender" || repo == "mender-artifact" {
					makeQEMU = true
				}

				if action == "merge" || action == "closed" {
					if repo == "mender" || repo == "meta-mender" {
						buildContainers = true
					} else {
						// if this is a merge, and it's not for mender or meta-mender, we aren't interested.
						return nil
					}
				}

				// we need to have the latest integration/master branch in order to use the release_tool.py
				if err := updateIntegrationRepo(conf); err != nil {
					log.Warnf(err.Error())
				}

				// by default, the integration branch will be master..
				integrationsToTest := []string{"master"}

				// unless what we are testing is dependant on our integration branch.
				if isDependantOnIntegration(repo, conf) {
					if repo == "integration" {
						integrationsToTest = []string{baseBranch}
					} else {
						var err error
						if integrationsToTest, err = getIntegrationVersionsUsingMicroservice(repo, baseBranch, conf); err != nil {
							log.Warnf("failed to get related microservices for repo: %s version: %s, failed with: %s\n", repo, baseBranch, err.Error())
						}
					}
				}

				// one pull request can trigger multiple builds
				for _, integrationBranch := range integrationsToTest {
					buildOpts := buildOptions{
						pr:             strconv.Itoa(pr.GetNumber()),
						repo:           repo,
						baseBranch:     integrationBranch,
						commitSHA:      commitSHA,
						makeQEMU:       makeQEMU,
						pushContainers: buildContainers,
					}
					builds = append(builds, buildOpts)
				}
			}
		}
	}

	log.Infof("%s:%d triggered %d builds: \n", repo, pr.GetNumber(), len(builds))
	for idx, build := range builds {
		log.Infof("%d: "+spew.Sdump(build)+"\n", idx+1)
	}

	return builds
}

func triggerBuild(conf *config, build *buildOptions) error {
	auth := &gojenkins.Auth{
		Username: conf.username,
		ApiToken: conf.password,
	}

	jenkins := gojenkins.NewJenkins(auth, conf.baseURL)
	job, err := jenkins.GetJob("mender-builder")

	if err != nil {
		return nil
	}

	readHead := "pull/" + build.pr + "/head"
	buildParameter := url.Values{}

	for _, watchRepo := range conf.watchRepositories {
		// don't set build parameter for the repo we are building, since we set that later
		// and use the default "master" for both mender-qa, and meta-mender (set in Jenkins)
		if watchRepo != build.repo && watchRepo != "mender-qa" && watchRepo != "meta-mender" {
			if watchRepo == "mender-artifact" {
				// mender-artifact also needs special treatment
				if maVersion, err := getMenderArtifactRevisionFromIntegration(build.baseBranch); err != nil {
					log.Errorln("failed to determine mender-artifact version: " + err.Error())
					return err
				} else {
					log.Info("mender-artifact version is: ", maVersion)
					buildParameter.Add(repoToJenkinsParameter(watchRepo), maVersion)
				}
			} else {
				buildParameter.Add(repoToJenkinsParameter(watchRepo), build.baseBranch)
			}
		}
	}

	// we dont watch for "gui" pr, since we don't test it here, so we must include it manually
	buildParameter.Add("GUI_REV", build.baseBranch)

	// set the rest of the jenkins build parameters
	buildParameter.Add("PR_TO_TEST", build.pr)
	buildParameter.Add("REPO_TO_TEST", build.repo)
	buildParameter.Add("BASE_BRANCH", build.baseBranch)
	buildParameter.Add("GIT_COMMIT", build.commitSHA)
	buildParameter.Add("RUN_INTEGRATION_TESTS", "true")
	buildParameter.Add(repoToJenkinsParameter(build.repo), readHead)

	if build.makeQEMU {
		buildParameter.Add("BUILD_QEMU", "true")
		buildParameter.Add("BUILD_BBB", "true")

		buildParameter.Add("TEST_QEMU", "true")
	}

	if build.pushContainers {
		buildParameter.Add("PUSH_CONTAINERS", "true")
	}

	return jenkins.Build(job, buildParameter)
}

// The parameter that jenkins uses for repo specific revisions is <REPO_NAME>_REV
func repoToJenkinsParameter(repo string) string {
	repoRevision := strings.ToUpper(repo) + "_REV"
	return strings.Replace(repoRevision, "-", "_", -1)
}

// Use python script in order to determine which integration branches to test with
func getIntegrationVersionsUsingMicroservice(repo, version string, conf *config) ([]string, error) {
	c := exec.Command("release_tool.py", "--integration-versions-including", repo, "--version", version)
	c.Dir = conf.integrationDirectory + "/extra/"
	integrations, err := c.Output()

	if err != nil {
		return nil, err
	}

	branches := strings.Split(strings.TrimSpace(string(integrations)), "\n")

	// remove the remote (ex: "`origin`/1.0.x")
	for idx, branch := range branches {
		if strings.Contains(branch, "/") {
			branches[idx] = strings.Split(branch, "/")[1]
		}
	}

	return branches, nil
}

func updateIntegrationRepo(conf *config) error {
	gitcmd := exec.Command("git", "pull", "--rebase", "origin")
	gitcmd.Dir = conf.integrationDirectory

	// timeout and kill process after GIT_OPERATION_TIMEOUT seconds
	t := time.AfterFunc(GIT_OPERATION_TIMEOUT*time.Second, func() {
		gitcmd.Process.Kill()
	})
	defer t.Stop()

	if err := gitcmd.Run(); err != nil {
		return fmt.Errorf("failed to 'git pull' integration folder: %s\n", err.Error())
	}
	return nil
}

func isDependantOnIntegration(repo string, conf *config) bool {
	for _, e := range conf.integrationBranchDependant {
		if repo == e {
			return true
		}
	}
	return false
}

func getMenderArtifactRevisionFromIntegration(baseBranch string) (string, error) {
	tmp, _ := ioutil.TempDir("/var/tmp", "mender-integration")
	defer os.RemoveAll(tmp)

	gitcmd := exec.Command("git", "clone", "-b", baseBranch, "https://github.com/mendersoftware/integration.git", tmp)

	// timeout and kill process after GIT_OPERATION_TIMEOUT seconds
	t := time.AfterFunc(GIT_OPERATION_TIMEOUT*time.Second, func() {
		gitcmd.Process.Kill()
	})
	defer t.Stop()

	if err := gitcmd.Run(); err != nil {
		return "", fmt.Errorf("failed to 'git pull' integration folder: %s\n", err.Error())
	}

	c := exec.Command(tmp+"/extra/release_tool.py", "--version-of", "mender-artifact")

	if maVersion, err := c.Output(); err != nil {
		return "", fmt.Errorf("failed to get mender-artifact version using relese tool: %s\n", err.Error())
	} else {
		return strings.TrimSpace(string(maVersion)), nil
	}
}
