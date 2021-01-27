package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"github.com/davecgh/go-spew/spew"
	"github.com/gin-gonic/gin"
	"github.com/google/go-github/v28/github"
	"github.com/xanzy/go-gitlab"
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

			// make sure we only parse one pr at a time, since we use git
			mutex.Lock()

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

			builds := parsePullRequest(conf, action, pr)

			// First check if the PR has been merged. If so, stop
			// the pipeline, and do nothing else.
			if err = stopBuildsOfStalePRs(pr, conf); err != nil {
				log.Errorf("Failed to stop a stale build after the PR: %v was merged or closed. Error: %v", pr, err)
			}

			// Then, continue to the integration Pipeline only for mendersoftware members
			if member, _, _ := githubClient.Organizations.IsMember(context, "mendersoftware", pr.Sender.GetLogin()); !member {
				log.Warnf("%s is making a pullrequest, but he/she is not a member of mendersoftware, ignoring", pr.Sender.GetLogin())
				mutex.Unlock()
				return
			}

			log.Infof("%s:%d triggered %d builds: \n", *pr.Repo.Name, pr.GetNumber(), len(builds))

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

func parsePullRequest(conf *config, action string, pr *github.PullRequestEvent) []buildOptions {
	log.Info("Pull request event with action: ", action)
	var builds []buildOptions

	// github pull request events to trigger a CI job for
	if action == "opened" || action == "edited" || action == "reopened" ||
		action == "synchronize" || action == "ready_for_review" {

		return getBuilds(conf, pr)
	}

	return builds
}

func getBuilds(conf *config, pr *github.PullRequestEvent) []buildOptions {

	var builds []buildOptions

	repo := *pr.Repo.Name

	commitSHA := pr.PullRequest.Head.GetSHA()
	//GetLabel returns "mendersoftware:master", we just want the branch
	baseBranch := strings.Split(pr.PullRequest.Base.GetLabel(), ":")[1]

	makeQEMU := false

	for _, watchRepo := range conf.watchRepositoriesTriggerPipeline {
		// make sure the repo that the pull request is performed against is
		// one that we are watching.

		if watchRepo == repo {

			// check if we need to build/test yocto
			for _, qemuRepo := range qemuBuildRepositories {
				if repo == qemuRepo {
					makeQEMU = true
				}
			}

			// we need to have the latest integration/master branch in order to use the release_tool.py
			if err := updateIntegrationRepo(conf); err != nil {
				log.Warnf(err.Error())
			}

			switch repo {
			case "meta-mender", "integration":
				build := buildOptions{
					pr:         strconv.Itoa(pr.GetNumber()),
					repo:       repo,
					baseBranch: baseBranch,
					commitSHA:  commitSHA,
					makeQEMU:   makeQEMU,
				}
				builds = append(builds, build)

			default:
				var err error
				integrationsToTest := []string{}

				if integrationsToTest, err = getIntegrationVersionsUsingMicroservice(repo, baseBranch, conf); err != nil {
					log.Errorf("failed to get related microservices for repo: %s version: %s, failed with: %s\n", repo, baseBranch, err.Error())
					return nil
				}
				log.Infof("the following integration branches: %s are using %s/%s", integrationsToTest, repo, baseBranch)

				// one pull request can trigger multiple builds
				for _, integrationBranch := range integrationsToTest {
					buildOpts := buildOptions{
						pr:         strconv.Itoa(pr.GetNumber()),
						repo:       repo,
						baseBranch: integrationBranch,
						commitSHA:  commitSHA,
						makeQEMU:   makeQEMU,
					}
					builds = append(builds, buildOpts)
				}
			}

		}
	}
	return builds
}

func triggerBuild(conf *config, build *buildOptions, pr *github.PullRequestEvent) error {
	gitlabClient := gitlab.NewClient(nil, conf.gitlabToken)
	err := gitlabClient.SetBaseURL(conf.gitlabBaseURL)
	if err != nil {
		return err
	}

	buildParameters, err := getBuildParameters(conf, build)
	if err != nil {
		return err
	}

	// first stop old pipelines with the same buildParameters
	stopStalePipelines(gitlabClient, buildParameters)

	// trigger the new pipeline
	integrationPipelinePath := "Northern.tech/Mender/mender-qa"
	ref := "master"
	opt := &gitlab.CreatePipelineOptions{
		Ref:       &ref,
		Variables: buildParameters,
	}

	variablesString := ""
	for _, variable := range opt.Variables {
		variablesString += variable.Key + ":" + variable.Value + ", "
	}
	log.Infof("Creating pipeline in project %s:%s with variables: %s", integrationPipelinePath, *opt.Ref, variablesString)

	pipeline, _, err := gitlabClient.Pipelines.CreatePipeline(integrationPipelinePath, opt, nil)
	if err != nil {
		log.Errorf("Could not create pipeline: %s", err.Error())
	}
	log.Infof("Created pipeline: %s", pipeline.WebURL)

	// Add the build variable matrix to the pipeline comment under a
	// drop-down tab
	tmplString := `
Hello :smile_cat: I created a pipeline for you here: [Pipeline-{{.Pipeline.ID}}]({{.Pipeline.WebURL}})

<details>
    <summary>Build Configuration Matrix</summary><p>

| Key   | Value |
| ----- | ----- |
{{range $i, $var := .BuildVars}}{{if $var.Value}}| {{$var.Key}} | {{$var.Value}} |{{printf "\n"}}{{end}}{{end}}

 </p></details>
`
	tmpl, err := template.New("Main").Parse(tmplString)
	if err != nil {
		log.Errorf("Failed to parse the build matrix template. Should never happen! Error: %s\n", err.Error())
	}
	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, struct {
		BuildVars []*gitlab.PipelineVariable
		Pipeline  *gitlab.Pipeline
	}{
		BuildVars: opt.Variables,
		Pipeline:  pipeline,
	}); err != nil {
		log.Errorf("Failed to execute the build matrix template. Error: %s\n", err.Error())
	}

	// Comment with a pipeline-link on the PR
	commentBody := buf.String()
	comment := github.IssueComment{
		Body: &commentBody,
	}
	githubClient := createGitHubClient(conf)
	_, _, err = githubClient.Issues.CreateComment(context.Background(),
		"mendersoftware", *pr.GetRepo().Name, pr.GetNumber(), &comment)
	if err != nil {
		log.Infof("Failed to comment on the pr: %v, Error: %s", pr, err.Error())
	}

	return err
}

func syncRemoteRef(org, repo, ref string) error {

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

	gitcmd = exec.Command("git", "remote", "add", "github", "git@github.com:"+org+"/"+repo)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	remoteURL, err := getRemoteURLGitLab(org, repo)
	if err != nil {
		return fmt.Errorf("getRemoteURLGitLab returned error: %s", err.Error())
	}

	gitcmd = exec.Command("git", "remote", "add", "gitlab", remoteURL)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	if strings.Contains(ref, "tags") {
		tagName := strings.TrimPrefix(ref, "refs/tags/")

		gitcmd = exec.Command("git", "fetch", "--tags", "github")
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}

		gitcmd = exec.Command("git", "push", "-f", "gitlab", tagName)
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}
	} else if strings.Contains(ref, "heads") {
		branchName := strings.TrimPrefix(ref, "refs/heads/")

		gitcmd = exec.Command("git", "fetch", "github")
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}

		gitcmd = exec.Command("git", "checkout", "-b", branchName, "github/"+branchName)
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}

		gitcmd = exec.Command("git", "push", "-f", "gitlab", branchName)
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}
	} else {
		return fmt.Errorf("Unrecognized ref %s", ref)
	}

	log.Infof("Pushed ref to GitLab: %s:%s", repo, ref)
	return nil
}

func stopStalePipelines(client *gitlab.Client, vars []*gitlab.PipelineVariable) {
	integrationPipelinePath := "Northern.tech/Mender/mender-qa"

	sort.SliceStable(vars, func(i, j int) bool {
		return vars[i].Key < vars[j].Key
	})

	username := "mender-test-bot"
	status := gitlab.Pending
	opt := &gitlab.ListProjectPipelinesOptions{
		Username: &username,
		Status:   &status,
	}

	pipelinesPending, _, err := client.Pipelines.ListProjectPipelines(integrationPipelinePath, opt, nil)
	if err != nil {
		log.Errorf("stopStalePipelines: Could not list pending pipelines: %s", err.Error())
	}

	status = gitlab.Running
	opt = &gitlab.ListProjectPipelinesOptions{
		Username: &username,
		Status:   &status,
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

// syncIfOSHasEnterpriseRepo detects whether a commit has been merged to
// the Open Source edition of a repo, and then creates a PR-branch
// in the Enterprise edition, which is then used in order to open
// a PR to the Enterprise repo with the OS changes.
func syncIfOSHasEnterpriseRepo(conf *config, gpr *github.PullRequestEvent) error {

	repo := gpr.GetRepo()
	if repo == nil {
		return fmt.Errorf("syncIfOSHasEnterpriseRepo: Failed to get the repository information")
	}

	// Enterprise repo sentinel
	switch repo.GetName() {
	case "deployments":
	case "inventory":
	case "useradm":
	case "workflows":
	default:
		log.Debugf("syncIfOSHasEnterpriseRepo: Repository without Enterprise fork detected: (%s). Not syncing", repo.GetName())
		return nil
	}

	pr := gpr.GetPullRequest()
	if pr == nil {
		return errors.New("syncIfOSHasEnterpriseRepo: Failed to get the pull request")
	}

	// If the action is "closed" and the "merged" key is "true", the pull request was merged.
	// While webhooks are also triggered when a pull request is synchronized, Events API timelines
	// don't include pull request events with the "synchronize" action.
	if gpr.GetAction() == "closed" && pr.GetMerged() {

		// Only sync on Merges to master, release and feature branches.
		// Verify release branches.
		branch := pr.GetBase()
		if branch == nil {
			return fmt.Errorf("syncIfOSHasEnterpriseRepo: Failed to get the base-branch of the PR: %v", branch)
		}

		syncBranches := regexp.MustCompile(`(master|[0-9]+\.[0-9]+\.x|` + featureBranchPrefix + `.+)`)
		branchRef := branch.GetRef()
		if branchRef == "" {
			return fmt.Errorf("Failed to get the branch-ref from the PR: %v", pr)
		}
		if !syncBranches.MatchString(branchRef) {

			log.Debugf("syncIfOSHasEnterpriseRepo: Detected a merge into another branch than master or a release branch: (%s), no syncing done", branchRef)

		} else {

			log.Infof("syncIfOSHasEnterpriseRepo: Merge to (%s) in an OS repository detected. Syncing the repositories...", branchRef)

			PRNumber := strconv.Itoa(pr.GetNumber())
			PRBranchName := "mergeostoent_" + PRNumber

			merged, err := createPRBranchOnEnterprise(repo.GetName(), branchRef, PRNumber, PRBranchName)
			if err != nil {
				return fmt.Errorf("syncIfOSHasEnterpriseRepo: Failed to create the PR branch on the Enterprise repo due to error: %v", err)
			}

			// Get the link to the original PR, so that it can be linked to
			// in the commit body
			PRURL := pr.GetHTMLURL()

			enterprisePR, err := createPullRequestFromTestBotFork(createPRArgs{
				conf:        conf,
				repo:        repo.GetName() + "-enterprise",
				prBranch:    "mender-test-bot:" + PRBranchName,
				baseBranch:  branchRef,
				message:     fmt.Sprintf("[Bot] %s", pr.GetTitle()),
				messageBody: fmt.Sprintf("Original PR: %s\n\n%s", PRURL, pr.GetBody()),
			})
			if err != nil {
				return fmt.Errorf("syncIfOSHasEnterpriseRepo: Failed to create a PR with error: %v", err)
			}

			log.Infof("syncIfOSHasEnterpriseRepo: Created PR: %d on Enterprise/%s/%s",
				enterprisePR.GetNumber(), repo.GetName(), branchRef)
			log.Debugf("syncIfOSHasEnterpriseRepo: Created PR: %v", pr)
			log.Debug("Trying to @mention the user in the newly created PR")
			userName := pr.GetMergedBy().GetLogin()
			log.Debugf("userName: %s", userName)

			if userName != "" {
				err = commentToNotifyUser(commentArgs{
					pr:             enterprisePR,
					conf:           conf,
					mergeConflicts: !merged,
					repo:           repo.GetName() + "-enterprise",
					userName:       userName,
					prBranchName:   PRBranchName,
					branchName:     branchRef,
				})
				if err != nil {
					log.Errorf("syncIfOSHasEnterpriseRepo: %s", err.Error())
				}
			}

		}

	}

	return nil
}

// createPRBranchOnEnterprise creates a new branch in the Enterprise repository
// starting at the branch in which to sync, with the name 'PRBranchName'
// and merges this with the OS equivalent of 'branchName'.
func createPRBranchOnEnterprise(repo, branchName, PRNumber, PRBranchName string) (merged bool, err error) {

	tmpdir, err := ioutil.TempDir("", repo)
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmpdir)

	gitcmd := exec.Command("git", "init", ".")
	gitcmd.Dir = tmpdir
	out, err := gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	gitcmd = exec.Command("git", "remote", "add", "opensource", "git@github.com:mendersoftware/"+repo+".git")
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	gitcmd = exec.Command("git", "remote", "add", "enterprise", "git@github.com:mendersoftware/"+repo+"-enterprise"+".git")
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	gitcmd = exec.Command("git", "remote", "add", "mender-test-bot", "git@github.com:mender-test-bot/"+repo+"-enterprise"+".git")
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// Set the local name to 'mender-test-bot', and the local
	// email to 'mender@northern.tech'
	gitcmd = exec.Command("git", "config", "--add", "user.name", "mender-test-bot")
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}
	gitcmd = exec.Command("git", "config", "--add", "user.email", "mender@northern.tech")
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// Fetch the branch which we are going to sync
	gitcmd = exec.Command("git", "fetch", "opensource", branchName)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// Fetch the Enterprise branch in which to merge into, and create the PR branch
	gitcmd = exec.Command("git", "fetch", "enterprise", branchName+":"+PRBranchName)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// Checkout the enterprise PR branch
	gitcmd = exec.Command("git", "checkout", PRBranchName)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// Merge the OS branch into the PR branch
	mergeMsg := fmt.Sprintf("Merge OS base branch: (%s) including PR: (%s) into Enterprise: (%[1]s)",
		branchName, PRNumber)
	log.Debug("Trying to " + mergeMsg)
	gitcmd = exec.Command("git", "merge", "-m", mergeMsg, "opensource/"+branchName)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	merged = true
	if err != nil {
		merged = false
		if strings.Contains(string(out), "Automatic merge failed") {
			msg := "Merge conflict detected. Still pushing the Enterprise PR branch, " +
				"and creating the PR, so that the user can manually resolve, " +
				"and re-push to the PR once these are fixed"
			log.Warn(msg)
		} else {
			return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}
	}

	if !merged {
		// In case of a failed merge, reset PRBranchName to opensource/branchName
		// and push this branch to enterprise
		gitcmd = exec.Command("git", "reset", "--hard", "opensource/"+branchName)
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return merged, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}
	}

	// Push the branch to the mender-test-bot's own fork
	gitcmd = exec.Command("git", "push", "--set-upstream", "mender-test-bot", PRBranchName)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return merged, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	if merged {
		log.Infof("Merged branch: opensource/%s/%s into enterprise/%[1]s/%s in the Enterprise repo",
			repo, branchName, PRBranchName)
	} else {
		msg := "Failed to merge opensource/%s/%s into enterprise/%[1]s/%s in the Enterprise repo. " +
			"Therefore pushed opensource/%[1]s/%[2]s to %s so that " +
			"merging can be done by a human locally"
		log.Infof(msg, repo, branchName, PRBranchName, PRBranchName)
	}

	return merged, nil
}

type createPRArgs struct {
	conf        *config
	repo        string
	prBranch    string
	baseBranch  string
	message     string
	messageBody string
}

func createPullRequestFromTestBotFork(args createPRArgs) (*github.PullRequest, error) {

	client := createGitHubClient(args.conf)

	newPR := &github.NewPullRequest{
		Title:               github.String(args.message),
		Head:                github.String(args.prBranch),
		Base:                github.String(args.baseBranch),
		Body:                github.String(args.messageBody),
		MaintainerCanModify: github.Bool(true),
	}

	pr, _, err := client.PullRequests.Create(context.Background(), "mendersoftware", args.repo, newPR)
	if err != nil {
		return nil, fmt.Errorf("Failed to create the PR for: (%s) %v", args.repo, err)
	}

	return pr, nil
}

type commentArgs struct {
	pr             *github.PullRequest
	conf           *config
	mergeConflicts bool
	repo           string
	userName       string
	prBranchName   string
	branchName     string
}

func commentToNotifyUser(args commentArgs) error {

	// Post a comment, and @mention the user
	var commentBody string
	if !args.mergeConflicts {
		commentBody = fmt.Sprintf("@%s I have created a PR for you, ready to merge as soon as tests are passed", args.userName)
	} else {
		tmplString := `
@{{.UserName}} I have created a PR for you.

Unfortunately, a merge conflict was detected. This means that the conflict will have to be resolved manually by you, human. Then pushed to the PR-branch, once all conflicts are resolved.
This can be done by following:

<details>
    <summary><small>this</small> recipe</summary><p>

1. Make sure that the 'mender-test-bot' remote is present in your repository, or else add it with:
    1. {{.BackQuote}}git remote add mender-test-bot git@github.com:mender-test-bot/{{.Repo}}.git{{.BackQuote}}

2. Fetch the remote branches
    1. {{.BackQuote}}git fetch origin {{.BranchName}}:localtmp{{.BackQuote}}
    2. {{.BackQuote}}git fetch mender-test-bot {{.PRBranchName}}{{.BackQuote}}

3. Checkout the localtmp branch
    1. {{.BackQuote}}git checkout localtmp{{.BackQuote}}

4. Merge the branch into the PR branch
    1. {{.BackQuote}}git merge mender-test-bot/{{.PRBranchName}}{{.BackQuote}}

5. Resolve all conflicts

6. Commit the merged changes

7. Push to the PR branch
    1. {{.BackQuote}}git push mender-test-bot localtmp:{{.PRBranchName}}{{.BackQuote}}

 </p></details>
`
		tmpl, err := template.New("Main").Parse(tmplString)
		if err != nil {
			log.Error("The text template should never fail to render!")
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, struct {
			UserName     string
			Repo         string
			PRBranchName string
			BranchName   string
			BackQuote    string
		}{
			UserName:     args.userName,
			Repo:         args.repo,
			PRBranchName: args.prBranchName,
			BranchName:   args.branchName,
			BackQuote:    "`",
		}); err != nil {
			log.Errorf("Failed to execute the merge-conflict PR template string. Error: %s", err.Error())
		}
		commentBody = buf.String()
	}
	comment := github.IssueComment{
		Body: &commentBody,
	}
	client := createGitHubClient(args.conf)

	_, _, err := client.Issues.CreateComment(context.Background(), "mendersoftware", args.repo, args.pr.GetNumber(), &comment)

	return err
}

func getBuildParameters(conf *config, build *buildOptions) ([]*gitlab.PipelineVariable, error) {
	gitlabClient := gitlab.NewClient(nil, conf.gitlabToken)
	err := gitlabClient.SetBaseURL(conf.gitlabBaseURL)
	if err != nil {
		return nil, err
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
		return nil, err
	}

	for _, versionedRepo := range versionedRepositories {
		// iterate over all the repositories (except the one we are testing) and
		// set the correct microservice versions

		// use the default "master" for both mender-qa, and meta-mender (set in CI)
		if versionedRepo != build.repo &&
			versionedRepo != "integration" &&
			build.repo != "meta-mender" {
			version, err := getServiceRevisionFromIntegration(versionedRepo, "origin/"+build.baseBranch, conf)
			if err != nil {
				log.Errorf("failed to determine %s version: %s", versionedRepo, err.Error())
				return nil, err
			}
			log.Infof("%s version %s is being used in %s", versionedRepo, version, build.baseBranch)
			buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: repoToBuildParameter(versionedRepo), Value: version})
		}
	}

	// set the correct integration branches if we aren't performing a pull request against integration
	if build.repo != "integration" && build.repo != "meta-mender" {
		buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: repoToBuildParameter("integration"), Value: build.baseBranch})
	}

	// set the poky branch equal to the meta-mender base branch, unless it
	// is master, in which case we rely on the default.
	if build.repo == "meta-mender" && build.baseBranch != "master" {
		buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: repoToBuildParameter("poky"), Value: build.baseBranch})
		buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: repoToBuildParameter("meta-openembedded"), Value: build.baseBranch})
		buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: repoToBuildParameter("meta-raspberrypi"), Value: build.baseBranch})
	}

	// set the rest of the CI build parameters
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "RUN_INTEGRATION_TESTS", Value: "true"})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: repoToBuildParameter(build.repo), Value: readHead})

	var qemuParam string
	if build.makeQEMU {
		qemuParam = "true"
	} else {
		qemuParam = ""
	}

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "BUILD_QEMUX86_64_UEFI_GRUB", Value: qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "TEST_QEMUX86_64_UEFI_GRUB", Value: qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "BUILD_QEMUX86_64_BIOS_GRUB", Value: qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "TEST_QEMUX86_64_BIOS_GRUB", Value: qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "BUILD_QEMUX86_64_BIOS_GRUB_GPT", Value: qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "TEST_QEMUX86_64_BIOS_GRUB_GPT", Value: qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "BUILD_VEXPRESS_QEMU", Value: qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "TEST_VEXPRESS_QEMU", Value: qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "BUILD_VEXPRESS_QEMU_FLASH", Value: qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "TEST_VEXPRESS_QEMU_FLASH", Value: qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "BUILD_VEXPRESS_QEMU_UBOOT_UEFI_GRUB", Value: qemuParam})
	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "TEST_VEXPRESS_QEMU_UBOOT_UEFI_GRUB", Value: qemuParam})

	buildParameters = append(buildParameters, &gitlab.PipelineVariable{Key: "BUILD_BEAGLEBONEBLACK", Value: qemuParam})
	return buildParameters, nil
}

// stopBuildsOfStalePRs stops any running pipelines on a PR which has been merged.
func stopBuildsOfStalePRs(pr *github.PullRequestEvent, conf *config) error {

	// If the action is "closed" the pull request was merged or just closed,
	// stop builds in both cases.
	if pr.GetAction() != "closed" {
		log.Debugf("stopBuildsOfStalePRs: PR not closed, therefore not stopping it's pipeline")
		return nil
	}

	log.Debug("stopBuildsOfStalePRs: Find any running pipelines and kill mercilessly!")

	for _, build := range getBuilds(conf, pr) {

		gitlabClient := gitlab.NewClient(nil, conf.gitlabToken)
		err := gitlabClient.SetBaseURL(conf.gitlabBaseURL)
		if err != nil {
			log.Debug("stopBuildsOfStalePRs: Failed to set the BaseURL of the gitlabClient")
			return err
		}

		buildParams, err := getBuildParameters(conf, &build)
		if err != nil {
			log.Debug("stopBuildsOfStalePRs: Failed to get the build-parameters for the build")
			return err
		}

		stopStalePipelines(gitlabClient, buildParams)
	}

	return nil

}
