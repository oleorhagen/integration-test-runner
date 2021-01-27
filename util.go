package main

import (
	"fmt"
)

func getRemoteURLGitLab(org, repo string) (string, error) {
	// By default, the GitLab project is Northern.tech/<group>/<repo>
	group, ok := gitHubOrganizationToGitLabGroup[org]
	if !ok {
		return "", fmt.Errorf("Unrecognized organization %s", org)
	}
	remoteURL := "git@gitlab.com:Northern.tech/" + group + "/" + repo

	// Override for some specific repos have custom GitLab group/project
	if v, ok := gitHubRepoToGitLabProjectCustom[repo]; ok {
		remoteURL = "git@gitlab.com:" + v
	}
	return remoteURL, nil
}
