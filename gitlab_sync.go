package main

import (
	"fmt"
	"strings"

	"github.com/mendersoftware/integration-test-runner/git"
	"github.com/sirupsen/logrus"
)

func syncRemoteRef(log *logrus.Entry, org, repo, ref string, conf *config) error {

	remoteURLGitLab, err := getRemoteURLGitLab(org, repo)
	if err != nil {
		return fmt.Errorf("getRemoteURLGitLab returned error: %s", err.Error())
	}

	state, err := git.Commands(
		git.Command("init", "."),
		git.Command("remote", "add", "github",
			getRemoteURLGitHub(conf.githubProtocol, githubOrganization, repo)),
		git.Command("remote", "add", "gitlab", remoteURLGitLab),
	)
	defer state.Cleanup()
	if err != nil {
		return err
	}

	if strings.Contains(ref, "tags") {
		tagName := strings.TrimPrefix(ref, "refs/tags/")

		err := git.Command("fetch", "--tags", "github").With(state).Run()
		if err != nil {
			return err
		}
		err = git.Command("push", "-f", "gitlab", tagName).With(state).Run()
		if err != nil {
			return err
		}
	} else if strings.Contains(ref, "heads") {
		branchName := strings.TrimPrefix(ref, "refs/heads/")

		err := git.Command("fetch", "github").With(state).Run()
		if err != nil {
			return err
		}
		err = git.Command("checkout", "-b", branchName, "github/"+branchName).With(state).Run()
		if err != nil {
			return err
		}
		err = git.Command("push", "-f", "gitlab", branchName).With(state).Run()
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("Unrecognized ref %s", ref)
	}

	log.Infof("Pushed ref to GitLab: %s:%s", repo, ref)
	return nil
}
