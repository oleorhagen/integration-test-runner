package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

func getServiceRevisionFromIntegration(repo, baseBranch string) (string, error) {
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

	c := exec.Command(tmp+"/extra/release_tool.py", "--version-of", repo)
	log.Infof("running: %s", c.Args)

	if version, err := c.Output(); err != nil {
		return "", fmt.Errorf("failed to get %s version using release tool: %s: \n%s", repo, err.Error(), version)
	} else {
		return strings.TrimSpace(string(version)), nil
	}
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
		return nil, err
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
		return nil, err
	}

	return strings.Split(strings.TrimSpace(string(output)), "\n"), nil
}
