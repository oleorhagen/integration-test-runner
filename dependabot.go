package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"

	"github.com/google/go-github/v28/github"
	"github.com/sirupsen/logrus"
)

func validDependabotPR(pr *github.PullRequestEvent) bool {
	return (pr.GetAction() == "opened" || pr.GetAction() == "reopened") &&
		pr.GetSender().GetLogin() == "dependabot[bot]"
}

func maybeVendorDependabotPR(log *logrus.Entry, pr *github.PullRequestEvent, conf *config) (bool, error) {
	isDependabotPR := false
	if !validDependabotPR(pr) {
		log.Debugf("Not a valid dependabot PR (%s:%s), ignoring...\n", pr.GetSender().GetLogin(), pr.GetAction())
		return isDependabotPR, nil
	}

	isDependabotPR = false
	branchName := pr.GetPullRequest().GetHead().GetRef()
	if branchName == "" {
		return isDependabotPR, fmt.Errorf("No Head set in the PR: %v", pr)
	}

	repo := pr.GetRepo().GetName()
	if repo == "" {
		return isDependabotPR, fmt.Errorf("No repository name found in PR: %v", pr)
	}

	tmpdir, err := ioutil.TempDir("", repo)
	if err != nil {
		return isDependabotPR, err
	}
	defer os.RemoveAll(tmpdir)

	repoURL := getRemoteURLGitHub(conf.githubProtocol, githubOrganization, repo)
	cmd := exec.Command("git", "clone", "--single-branch", "--branch", branchName, repoURL)
	cmd.Dir = tmpdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return isDependabotPR, fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmdDir := tmpdir + "/" + repo

	cmd = exec.Command("go", "mod", "tidy", "-v")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return isDependabotPR, fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("go", "mod", "vendor", "-v")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return isDependabotPR, fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("go", "mod", "verify")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return isDependabotPR, fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return isDependabotPR, fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("git", "commit", "--amend", "--no-edit")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return isDependabotPR, fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}

	cmd = exec.Command("git", "push", "--force")
	cmd.Dir = cmdDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return isDependabotPR, fmt.Errorf("%v returned error: %s: %s", cmd.Args, out, err.Error())
	}
	log.Info("Successfully vendored Dependabot PR")

	return isDependabotPR, nil

}
