package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/go-github/github"
	"github.com/yosida95/golang-jenkins"
	"golang.org/x/oauth2"
	"github.com/gin-gonic/gin"

	log "github.com/sirupsen/logrus"
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
	pr               string
	repo             string
	baseBranch       string
	commitSHA        string
	makeQEMU         bool
	publishArtifacts bool
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
			"mender-artifact",
			"mender-cli",
			"mender-conductor",
			"mender-conductor-enterprise",
			"meta-mender",
			"mender-api-gateway-docker",
			"tenantadm"}

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
		username:              username,
		password:              password,
		baseURL:               url,
		githubSecret:          []byte(githubSecret),
		githubToken:           githubToken,
		watchRepositories:     repositoryWatchList,
		integrationDirectory:  integrationDirectory,
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
			log.Infof("%s:%d triggered %d builds: \n", *pr.Repo.Name, pr.GetNumber(), len(builds))
			mutex.Unlock()

			for idx, build := range builds {
				log.Infof("%d: "+spew.Sdump(build)+"\n", idx+1)
				err = triggerBuild(conf, &build)
				if err != nil {
					log.Errorf("Could not start build: %s", err.Error())
				}
			}
		}
	})
	r.Run("0.0.0.0:8081")
}

func parsePullRequest(conf *config, action string, pr *github.PullRequestEvent) []buildOptions {
	log.Info("Pull request event with action: ", action)
	var builds []buildOptions

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

				switch repo {
				case "meta-mender", "integration", "tenantadm":
					build := buildOptions{
						pr:               strconv.Itoa(pr.GetNumber()),
						repo:             repo,
						baseBranch:       baseBranch,
						commitSHA:        commitSHA,
						makeQEMU:         makeQEMU,
						publishArtifacts: buildContainers,
					}
					builds = append(builds, build)

				default:
					var err error
					integrationsToTest := []string{}

					if integrationsToTest, err = getIntegrationVersionsUsingMicroservice(repo, baseBranch, conf); err != nil {
						log.Fatalf("failed to get related microservices for repo: %s version: %s, failed with: %s\n", repo, baseBranch, err.Error())
					}
					log.Infof("the following integration branches: %s are using %s/%s", integrationsToTest, repo, baseBranch)

					// one pull request can trigger multiple builds
					for _, integrationBranch := range integrationsToTest {
						buildOpts := buildOptions{
							pr:               strconv.Itoa(pr.GetNumber()),
							repo:             repo,
							baseBranch:       integrationBranch,
							commitSHA:        commitSHA,
							makeQEMU:         makeQEMU,
							publishArtifacts: buildContainers,
						}
						builds = append(builds, buildOpts)
					}
				}

			}
		}
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

	var versionedRepositories []string
	if build.repo == "meta-mender" {
		// For meta-mender, pick master versions of all Mender release repos.
		versionedRepositories, err = getListOfVersionedRepositories("origin/master")
	} else {
		versionedRepositories, err = getListOfVersionedRepositories("origin/" + build.baseBranch)
	}
	if err != nil {
		log.Errorf("Could not get list of repositories: %s", err.Error())
		return err
	}

	for _, versionedRepo := range versionedRepositories {
		// iterate over all the repositories (except the one we are testing) and
		// set the correct microservice versions

		// use the default "master" for both mender-qa, and meta-mender (set in Jenkins)
		if versionedRepo != build.repo &&
			versionedRepo != "integration" &&
			build.repo != "meta-mender" {
			if version, err := getServiceRevisionFromIntegration(versionedRepo, build.baseBranch); err != nil {
				log.Errorf("failed to determine %s version: %s", versionedRepo, err.Error())
				return err
			} else {
				log.Infof("%s version %s is being used in %s: ", versionedRepo, version, build.baseBranch)
				buildParameter.Add(repoToJenkinsParameter(versionedRepo), version)
			}
		}
	}

	// set the correct integraton branches if we aren't performing a pull request again integration
	if build.repo != "integration" && build.repo != "meta-mender" {
		buildParameter.Add(repoToJenkinsParameter("integration"), build.baseBranch)
	}

	// set the poky branch equal to the meta-mender base branch, unless it
	// is master, in which case we rely on the default.
	if build.repo == "meta-mender" && build.baseBranch != "master" {
		buildParameter.Add(repoToJenkinsParameter("poky"), build.baseBranch)
	}

	// set the rest of the jenkins build parameters
	buildParameter.Add("BASE_BRANCH", build.baseBranch)
	buildParameter.Add("RUN_INTEGRATION_TESTS", "true")
	buildParameter.Add(repoToJenkinsParameter(build.repo), readHead)

	var qemuParam string
	if build.makeQEMU {
		qemuParam = "true"
	} else {
		qemuParam = ""
	}

	buildParameter.Add("BUILD_QEMUX86_64_UEFI_GRUB", qemuParam)
	buildParameter.Add("TEST_QEMUX86_64_UEFI_GRUB", qemuParam)

	buildParameter.Add("BUILD_QEMUX86_64_BIOS_GRUB", qemuParam)
	buildParameter.Add("TEST_QEMUX86_64_BIOS_GRUB", qemuParam)

	buildParameter.Add("BUILD_QEMUX86_64_BIOS_GRUB_GPT", qemuParam)
	buildParameter.Add("TEST_QEMUX86_64_BIOS_GRUB_GPT", qemuParam)

	buildParameter.Add("BUILD_VEXPRESS_QEMU", qemuParam)
	buildParameter.Add("TEST_VEXPRESS_QEMU", qemuParam)

	buildParameter.Add("BUILD_VEXPRESS_QEMU_FLASH", qemuParam)
	buildParameter.Add("TEST_VEXPRESS_QEMU_FLASH", qemuParam)

	buildParameter.Add("BUILD_VEXPRESS_QEMU_UBOOT_UEFI_GRUB", qemuParam)
	buildParameter.Add("TEST_VEXPRESS_QEMU_UBOOT_UEFI_GRUB", qemuParam)

	buildParameter.Add("BUILD_BEAGLEBONEBLACK", qemuParam)

	if build.publishArtifacts {
		buildParameter.Add("PUBLISH_ARTIFACTS", "true")
	}

	log.Infof("Starting build: %s", spew.Sdump(buildParameter))
	return jenkins.Build(job, buildParameter)
}
