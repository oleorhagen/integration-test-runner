package main

import (
	"bytes"
	"context"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/google/go-github/v28/github"
	"github.com/xanzy/go-gitlab"

	log "github.com/sirupsen/logrus"
)

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
