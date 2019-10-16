package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/davecgh/go-spew/spew"
	"github.com/gin-gonic/gin"
	"github.com/google/go-github/github"
	"github.com/xanzy/go-gitlab"
	"golang.org/x/oauth2"

	log "github.com/sirupsen/logrus"
	"regexp"
)

var mutex = &sync.Mutex{}

type config struct {
	githubSecret               []byte
	githubToken                string
	gitlabToken                string
	gitlabBaseURL              string
	watchRepositories          []string
	integrationBranchDependant []string
	integrationDirectory       string
	gallieniiPort              string
}

type buildOptions struct {
	pr               string
	repo             string
	baseBranch       string
	commitSHA        string
	makeQEMU         bool
}

const (
	GIT_OPERATION_TIMEOUT = 30
)

func getConfig() (*config, error) {
	var repositoryWatchList []string
	githubSecret := os.Getenv("GITHUB_SECRET")
	githubToken := os.Getenv("GITHUB_TOKEN")
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	gitlabBaseURL := os.Getenv("GITLAB_BASE_URL")
	integrationDirectory := os.Getenv("INTEGRATION_DIRECTORY")
	gallieniiPort := os.Getenv("GALLIENII_PORT")

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
			"tenantadm",
			"deployments-enterprise",
			"useradm-enterprise",
		}

	watchRepositories := os.Getenv("WATCH_REPOS")

	if len(watchRepositories) == 0 {
		repositoryWatchList = defaultWatchRepositories
	} else {
		repositoryWatchList = strings.Split(watchRepositories, ",")
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
	case gallieniiPort == "":
		return &config{}, fmt.Errorf("Set GALLIENII_PORT")
	}

	return &config{
		githubSecret:         []byte(githubSecret),
		githubToken:          githubToken,
		gitlabToken:          gitlabToken,
		gitlabBaseURL:        gitlabBaseURL,
		watchRepositories:    repositoryWatchList,
		integrationDirectory: integrationDirectory,
		gallieniiPort:        gallieniiPort,
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
			action := pr.GetAction()

			// To run component's Pipeline create a branch in GitLab, regardless of the PR
			// coming from a mendersoftware member or not (equivalent to the old Travis tests)
			err := createPullRequestBranch(*pr.Repo.Name, strconv.Itoa(pr.GetNumber()), action)
			if err != nil {
				log.Errorf("Could not create PR branch: %s", err.Error())
			}

			// Then, continue to the integration Pipeline only for mendersoftware members
			if member, _, _ := githubClient.Organizations.IsMember(context, "mendersoftware", pr.Sender.GetLogin()); !member {
				log.Warnf("%s is making a pullrequest, but he/she is not a member of mendersoftware, ignoring", pr.Sender.GetLogin())
				return
			}

			// make sure we only parse one pr at a time, since we use git
			mutex.Lock()
			builds := parsePullRequest(conf, action, pr)
			log.Infof("%s:%d triggered %d builds: \n", *pr.Repo.Name, pr.GetNumber(), len(builds))

			// Keep the OS and Enterprise repos in sync
			if err = syncEnterpriseRepos(conf, pr); err != nil {
				log.Errorf("Failed to sync the OS and Enterprise repos: %s", err.Error())
			}
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
	r.Run("0.0.0.0:8083")
}

func parsePullRequest(conf *config, action string, pr *github.PullRequestEvent) []buildOptions {
	log.Info("Pull request event with action: ", action)
	var builds []buildOptions

	// Do not run the integration tests if 'NO-RUN-TESTS' is found on a separate line in the commit body
	if pr.GetPullRequest().Body != nil && strings.Contains(*pr.GetPullRequest().Body, "NO-RUN-TESTS") {
		return nil
	}

	repo := *pr.Repo.Name
	commitSHA := pr.PullRequest.Head.GetSHA()

	// github pull request events to trigger a jenkins job for
	if action == "opened" || action == "edited" || action == "reopened" || action == "synchronize" {

		//GetLabel returns "mendersoftware:master", we just want the branch
		baseBranch := strings.Split(pr.PullRequest.Base.GetLabel(), ":")[1]

		makeQEMU := false

		for _, watchRepo := range conf.watchRepositories {
			// make sure the repo that the pull request is performed against is
			// one that we are watching.

			if watchRepo == repo {
				if repo == "mender" || repo == "meta-mender" || repo == "mender-artifact" {
					makeQEMU = true
				}

				// we need to have the latest integration/master branch in order to use the release_tool.py
				if err := updateIntegrationRepo(conf); err != nil {
					log.Warnf(err.Error())
				}

				switch repo {
				case "meta-mender", "integration":
					build := buildOptions{
						pr:               strconv.Itoa(pr.GetNumber()),
						repo:             repo,
						baseBranch:       baseBranch,
						commitSHA:        commitSHA,
						makeQEMU:         makeQEMU,
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
	gitlabClient := gitlab.NewClient(nil, conf.gitlabToken)
	err := gitlabClient.SetBaseURL(conf.gitlabBaseURL)
	if err != nil {
		return err
	}

	readHead := "pull/" + build.pr + "/head"
	var buildParameters []*gitlab.PipelineVariable

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
				buildParameters = append(buildParameters, &gitlab.PipelineVariable{repoToBuildParameter(versionedRepo), version})
			}
		}
	}

	// set the correct integraton branches if we aren't performing a pull request again integration
	if build.repo != "integration" && build.repo != "meta-mender" {
		buildParameters = append(buildParameters, &gitlab.PipelineVariable{repoToBuildParameter("integration"), build.baseBranch})
	}

	// set the poky branch equal to the meta-mender base branch, unless it
	// is master, in which case we rely on the default.
	if build.repo == "meta-mender" && build.baseBranch != "master" {
		buildParameters = append(buildParameters, &gitlab.PipelineVariable{repoToBuildParameter("poky"), build.baseBranch})
	}

	// set the rest of the jenkins build parameters
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"BASE_BRANCH", build.baseBranch})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"RUN_INTEGRATION_TESTS", "true"})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{repoToBuildParameter(build.repo), readHead})

	var qemuParam string
	if build.makeQEMU {
		qemuParam = "true"
	} else {
		qemuParam = ""
	}

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"BUILD_QEMUX86_64_UEFI_GRUB", qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"TEST_QEMUX86_64_UEFI_GRUB", qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"BUILD_QEMUX86_64_BIOS_GRUB", qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"TEST_QEMUX86_64_BIOS_GRUB", qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"BUILD_QEMUX86_64_BIOS_GRUB_GPT", qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"TEST_QEMUX86_64_BIOS_GRUB_GPT", qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"BUILD_VEXPRESS_QEMU", qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"TEST_VEXPRESS_QEMU", qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"BUILD_VEXPRESS_QEMU_FLASH", qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"TEST_VEXPRESS_QEMU_FLASH", qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"BUILD_VEXPRESS_QEMU_UBOOT_UEFI_GRUB", qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"TEST_VEXPRESS_QEMU_UBOOT_UEFI_GRUB", qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{"BUILD_BEAGLEBONEBLACK", qemuParam})

	// first stop old pipelines with ame buildParameters
	stopStalePipelines(gitlabClient, buildParameters)

	// trigger the new pipeline
	integrationPipelinePath := "Northern.tech/Mender/mender-qa"
	ref := "master"
	opt := &gitlab.CreatePipelineOptions{
		Ref: &ref,
		Variables: buildParameters,
	}

	variablesString := ""
	for _, variable := range opt.Variables {
		variablesString += variable.Key + ":" + variable.Value + ", "
	}
	log.Infof("Creating pipeline in project %s:%s with variables: %s", integrationPipelinePath, *opt.Ref, variablesString)

	pipeline, _, err := gitlabClient.Pipelines.CreatePipeline(integrationPipelinePath, opt, nil)
	if err != nil {
		log.Errorf("Cound not create pipeline", err.Error())
	}
	log.Infof("Created pipeline: %s", pipeline.WebURL)

	return err
}

func createPullRequestBranch(repo, pr, action string) error {

	if action != "opened" && action != "edited" && action != "reopened" && action != "synchronize" {
		log.Infof("Action %s, ignoring", action)
		return nil
	}

	tmpdir, err := ioutil.TempDir("", repo)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	gitcmd := exec.Command("git", "init", ".")
	gitcmd.Dir = tmpdir
	out, err := gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	gitcmd = exec.Command("git", "remote", "add", "github", "git@github.com:mendersoftware/" + repo)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	gitcmd = exec.Command("git", "remote", "add", "gitlab", "git@gitlab.com:Northern.tech/Mender/" + repo)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	prBranchName := "pr_" + pr
	gitcmd = exec.Command("git", "fetch", "github", "pull/" + pr + "/head:" + prBranchName )
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	gitcmd = exec.Command("git", "push", "-f", "--set-upstream", "gitlab", prBranchName )
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	log.Infof("Created branch: %s:%s", repo, prBranchName)
	log.Info("Pipeline is expected to start automatically")
	return nil
}

func stopStalePipelines (client *gitlab.Client, vars []*gitlab.PipelineVariable) {
	integrationPipelinePath := "Northern.tech/Mender/mender-qa"

	sort.SliceStable(vars, func(i, j int) bool {
		return vars[i].Key < vars[j].Key
	})

	username := "mender-test-bot"
	status := gitlab.Pending
	opt := &gitlab.ListProjectPipelinesOptions{
		Username: &username,
		Status: &status,
	}

	pipelinesPending, _, err := client.Pipelines.ListProjectPipelines(integrationPipelinePath, opt, nil)
	if err != nil {
		log.Errorf("stopStalePipelines: Could not list pending pipelines: %s", err.Error())
	}

	status = gitlab.Running
	opt = &gitlab.ListProjectPipelinesOptions{
		Username: &username,
		Status: &status,
	}

	pipelinesRunning, _, err := client.Pipelines.ListProjectPipelines(integrationPipelinePath, opt, nil)
	if err != nil {
		log.Errorf("stopStalePipelines: Could not list running pipelines: %s", err.Error())
	}

	for _, pipeline := range append(pipelinesPending, pipelinesRunning...) {

		variables, _, err := client.Pipelines.GetPipelineVariables(integrationPipelinePath, pipeline.ID, nil)
		if err != nil {
			log.Errorf("stopStalePipelines: Could not get variables for pipeline: %s", err.Error())
			continue
		}

		sort.SliceStable(variables, func(i, j int) bool {
			return variables[i].Key < variables[j].Key
		})

		if reflect.DeepEqual(vars, variables) {
			log.Infof("Cancelling stale pipeline %d, url: %s", pipeline.ID, pipeline.WebURL)

			_, _, err := client.Pipelines.CancelPipelineBuild(integrationPipelinePath, pipeline.ID, nil)
			if err != nil {
				log.Errorf("stopStalePipelines: Could not cancel pipeline: %s", err.Error())
			}

		}

	}
}

// syncEnterpriseRepos detects whether a commit has been merged to
// the Open Source edition of a repo, and then sends a GET request to
// the Gallienii server, asking it to synchronize the repos
func syncEnterpriseRepos(conf *config, gpr *github.PullRequestEvent) error {

	repo := gpr.GetRepo()
	if repo == nil {
		return fmt.Errorf("syncEnterpriseRepos: Failed to get the repository information")
	}

	// Enterprise repo sentinel
	switch repo.GetName() {
	case "deployments":
	case "useradm":
	default:
		log.Debugf("syncEnterpriseRepos: Non-Enterprise repository detected: (%s). Not syncing", repo.GetName())
		return nil
	}

	pr := gpr.GetPullRequest()
	if pr == nil {
		return fmt.Errorf("syncEnterpriseRepos: Failed to get the pull request")
	}

	// If the action is "closed" and the "merged" key is "true", the pull request was merged.
	// While webhooks are also triggered when a pull request is synchronized, Events API timelines
	// don't include pull request events with the "synchronize" action.
	if gpr.GetAction() == "closed" && pr.GetMerged() {

		// Only sync on Merges to master or release branches and
		// verify release branches through [0-9].[0-9].x regex
		branch := pr.GetBase()
		if branch == nil {
			return fmt.Errorf("syncEnterpriseRepos: Failed to get the base-branch of the PR: %v", branch)
		}
		syncBranches := regexp.MustCompile("(master|[0-9].[0-9].x)")
		branchRef := branch.GetRef()
		if ! syncBranches.MatchString(branchRef) {
			log.Debugf("syncEnterpriseRepos: Detected a merge into another branch than master or a relase branch: (%s), do not sync", branchRef)
			return nil
		}

		log.Infof("syncEnterpriseRepos: Merge to (%s) from an OS repository detected. Running 'gallienii'...", branchRef)
		address := fmt.Sprintf("http://localhost:%s", conf.gallieniiPort)
		resp, err := http.Get(address)
		if err != nil {
			err = fmt.Errorf("syncEnterpriseRepos: HTTP GET request to the Gallianii server failed wit error: %s", err.Error())
			return err
		}
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			errstr := fmt.Sprintf("syncEnterpriseRepos: Failed to read the HTTP response body from Gallienii. Err: %s",
				err.Error())
			return errors.New(errstr)
		}
		log.Debugf("Gallienii returned: %s", body)
	}

	return nil
}
