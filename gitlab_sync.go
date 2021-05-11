package main

import (
	"fmt"
	"strings"

	"github.com/mendersoftware/integration-test-runner/git"
	"github.com/sirupsen/logrus"
)

func syncRemoteRef(log *logrus.Entry, org, repo, ref string, conf *config) error {

	state, err := git.Commands(
		git.Command("init", "."),
		git.Command("remote", "add", "github",
			getRemoteURLGitHub(conf.githubProtocol, githubOrganization, repo)),
		git.Command("remote", "add", "gitlab",
			getRemoteURLGitHub(conf.githubProtocol, githubOrganization, repo)),
	)
	defer state.Cleanup()
	if err != nil {
		return err
	}

	if strings.Contains(ref, "tags") {
		tagName := strings.TrimPrefix(ref, "refs/tags/")

		state, err = git.Commands(
			git.Command("fetch", "--tags", "github"),
			git.Command("push", "-f", "gitlab", tagName),
		)
		defer state.Cleanup()
		if err != nil {
			return err
		}
	} else if strings.Contains(ref, "heads") {
		branchName := strings.TrimPrefix(ref, "refs/heads/")

		state, err = git.Commands(
			git.Command("fetch", "github"),
			git.Command("checkout", "-b", branchName, "github/"+branchName),
			git.Command("push", "-f", "gitlab", branchName),
		)
		defer state.Cleanup()
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("Unrecognized ref %s", ref)
	}

	log.Infof("Pushed ref to GitLab: %s:%s", repo, ref)
	return nil
}
