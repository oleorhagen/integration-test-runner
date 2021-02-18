package main

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/template"

	"github.com/google/go-github/v28/github"
	"github.com/sirupsen/logrus"

	clientgithub "github.com/mendersoftware/integration-test-runner/client/github"
)

func getLatestIntegrationRelease(number int, conf *config) ([]string, error) {
	cmd := fmt.Sprintf("git for-each-ref --sort=-creatordate --format='%%(refname:short)' 'refs/tags' "+
		"| sed -E '/(^[0-9]+\\.[0-9]+)\\.[0-9]+$/!d;s//\\1.x/' | uniq | head -n %d | sort -V -r", number)
	c := exec.Command("sh", "-c", cmd)
	c.Dir = conf.integrationDirectory + "/extra/"
	version, err := c.Output()
	if err != nil {
		err = fmt.Errorf("getLatestIntegrationRelease: Error: %v (%s)", err, version)
	}
	versionStr := strings.TrimSpace(string(version))
	return strings.SplitN(versionStr, "\n", -1), err
}

// suggestCherryPicks suggests cherry-picks to release branches if the PR has been merged to master
func suggestCherryPicks(log *logrus.Entry, pr *github.PullRequestEvent, githubClient clientgithub.Client, conf *config) error {
	// ignore PRs if they are not closed and merged
	action := pr.GetAction()
	merged := pr.GetPullRequest().GetMerged()
	if action != "closed" || !merged {
		log.Infof("Ignoring cherry-pick suggestions for action: %s, merged: %v", action, merged)
		return nil
	}

	// ignore PRs if they don't target the master branch
	baseRef := pr.GetPullRequest().GetBase().GetRef()
	if baseRef != "master" {
		log.Infof("Ignoring cherry-pick suggestions for base ref: %s", baseRef)
		return nil
	}

	// initialize the git work area
	repo := pr.GetRepo().GetName()
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

	repoURL := getRemoteURLGitHub(conf.githubProtocol, "mendersoftware", repo)
	gitcmd = exec.Command("git", "remote", "add", "github", repoURL)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// fetch the master branch
	gitcmd = exec.Command("git", "fetch", "github", "master:local")
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// fetch the PR branch
	prNumber := strconv.Itoa(pr.GetNumber())
	prBranchName := "pr_" + prNumber
	gitcmd = exec.Command("git", "fetch", "github", "pull/"+prNumber+"/head:"+prBranchName)
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// count the number commits with Changelog entries
	baseSHA := pr.GetPullRequest().GetBase().GetSHA()
	countCmd := exec.Command("sh", "-c", "git log "+baseSHA+"...pr_"+prNumber+" | grep -i -e \"^    Changelog: \" | grep -v -i -e \"^    Changelog: *none\" | wc -l")
	countCmd.Dir = tmpdir
	out, err = countCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	changelogs, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if changelogs == 0 {
		log.Infof("Found no changelog entries, ignoring cherry-pick suggestions")
		return nil
	}

	// fetch all the branches
	gitcmd = exec.Command("git", "fetch", "github")
	gitcmd.Dir = tmpdir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	// get list of release versions
	versions, err := getLatestIntegrationRelease(3, conf)
	if err != nil {
		return err
	}
	releaseBranches := make([]string, 3)
	for i, version := range versions {
		releaseBranch, err := getServiceRevisionFromIntegration(repo, "origin/"+version, conf)
		if err != nil {
			return err
		}
		releaseBranches[i] = releaseBranch + " (release " + version + ")"
	}

	// suggest cherry-picking with a comment
	tmplString := `
Hello :smile_cat: This PR contains changelog entries. Please, verify the need of backporting it to the following release branches:
{{.ReleaseBranches}}
`
	tmpl, err := template.New("Main").Parse(tmplString)
	if err != nil {
		log.Errorf("Failed to parse the build matrix template. Should never happen! Error: %s\n", err.Error())
	}
	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, struct {
		ReleaseBranches string
	}{
		ReleaseBranches: strings.Join(releaseBranches, "\n"),
	}); err != nil {
		log.Errorf("Failed to execute the build matrix template. Error: %s\n", err.Error())
	}

	// Comment with a pipeline-link on the PR
	commentBody := buf.String()
	comment := github.IssueComment{
		Body: &commentBody,
	}
	if err := githubClient.CreateComment(context.Background(), "mendersoftware",
		pr.GetRepo().GetName(), pr.GetNumber(), &comment); err != nil {
		log.Infof("Failed to comment on the pr: %v, Error: %s", pr, err.Error())
		return err
	}
	return nil
}
