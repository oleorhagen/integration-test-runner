package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-github/v28/github"
	log "github.com/sirupsen/logrus"
)

func getServiceRevisionFromIntegration(repo, baseBranch string, conf *config) (string, error) {
	c := exec.Command("release_tool.py", "--version-of", repo, "--in-integration-version", baseBranch)
	c.Dir = conf.integrationDirectory + "/extra/"
	version, err := c.Output()
	if err != nil {
		err = fmt.Errorf("getServiceRevisionFromIntegration: Error: %v (%s)", err, version)
	}
	return strings.TrimSpace(string(version)), err
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
		return fmt.Errorf("failed to 'git pull' integration folder: %s", err.Error())
	}
	return nil
}

// The parameter that the build system uses for repo specific revisions is <REPO_NAME>_REV
func repoToBuildParameter(repo string) string {
	repoRevision := strings.ToUpper(repo) + "_REV"
	return strings.Replace(repoRevision, "-", "_", -1)
}

// Use python script in order to determine which integration branches to test with
func getIntegrationVersionsUsingMicroservice(repo, version string, conf *config) ([]string, error) {
	c := exec.Command("release_tool.py", "--integration-versions-including", repo, "--version", version)
	c.Dir = conf.integrationDirectory + "/extra/"
	integrations, err := c.Output()

	if err != nil {
		return nil, fmt.Errorf("getIntegrationVersionsUsingMicroservice: Error: %v (%s)", err, integrations)
	}

	branches := strings.Split(strings.TrimSpace(string(integrations)), "\n")

	// remove the remote (ex: "`origin`/1.0.x")
	for idx, branch := range branches {
		if strings.Contains(branch, "/") {
			branches[idx] = strings.Split(branch, "/")[1]
		}
	}

	log.Infof("%s/%s is being used in the following integration: %s", repo, version, branches)
	return branches, nil
}

func getListOfVersionedRepositories(inVersion string) ([]string, error) {
	c := exec.Command("release_tool.py", "--list", "--in-integration-version", inVersion)
	output, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("getListOfVersionedRepositories: Error: %v (%s)", err, output)
	}

	return strings.Split(strings.TrimSpace(string(output)), "\n"), nil
}

func validDependabotPR(pr *github.PullRequestEvent) bool {
	return (pr.GetAction() == "opened" || pr.GetAction() == "reopened") &&
		pr.GetSender().GetLogin() == "dependabot[bot]"
}

func maybeVendorDependabotPR(pr *github.PullRequestEvent) error {
	if !validDependabotPR(pr) {
		log.Debugf("Not a valid dependabot PR (%s:%s), ignoring...\n", pr.GetSender().GetLogin(), pr.GetAction())
		return nil
	}

	branchName := pr.GetPullRequest().GetHead().GetRef()
	if branchName == "" {
		return fmt.Errorf("No Head set in the PR: %v", pr)
	}

	repo := pr.GetRepo().GetName()
	if repo == "" {
		return fmt.Errorf("No repository name found in PR: %v", pr)
	}

	tmpdir, err := ioutil.TempDir("", repo)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	cmd := exec.Command("git", "clone", "--single-branch", "--branch", branchName, "git@github.com:mendersoftware/"+repo+".git")
	cmd.Dir = tmpdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmdDir := tmpdir + "/" + repo

	cmd = exec.Command("go", "mod", "tidy", "-v")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("go", "mod", "vendor", "-v")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("go", "mod", "verify")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("git", "commit", "--amend", "--no-edit")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("git", "push", "--force")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}
	log.Info("Successfully vendored Dependabot PR")

	return nil

}
