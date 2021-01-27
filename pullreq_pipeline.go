package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"

	log "github.com/sirupsen/logrus"
)

func createPullRequestBranch(org, repo, pr, action string) error {

	if action != "opened" && action != "edited" && action != "reopened" &&
		action != "synchronize" && action != "ready_for_review" {
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

	gitcmd = exec.Command("git", "remote", "add", "github", "git@github.com:mendersoftware/"+repo)
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

	prBranchName := "pr_" + pr
	gitcmd = exec.Command("git", "fetch", "github", "pull/"+pr+"/head:"+prBranchName)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	gitcmd = exec.Command("git", "push", "-f", "--set-upstream", "gitlab", prBranchName)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	log.Infof("Created branch: %s:%s", repo, prBranchName)
	log.Info("Pipeline is expected to start automatically")
	return nil
}
