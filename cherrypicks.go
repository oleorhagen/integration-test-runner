package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/google/go-github/v28/github"
	"github.com/sirupsen/logrus"

	clientgithub "github.com/mendersoftware/integration-test-runner/client/github"
	"github.com/mendersoftware/integration-test-runner/git"
)

var errorCherryPickConflict = errors.New("Cherry pick had conflicts")

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
	repoURL := getRemoteURLGitHub(conf.githubProtocol, githubOrganization, repo)
	prNumber := strconv.Itoa(pr.GetNumber())
	prBranchName := "pr_" + prNumber
	state, err := git.Commands(
		git.Command("init", "."),
		git.Command("remote", "add", "github", repoURL),
		git.Command("fetch", "github", "master:local"),
		git.Command("fetch", "github", "pull/"+prNumber+"/head:"+prBranchName),
	)
	defer state.Cleanup()
	if err != nil {
		return err
	}

	// count the number commits with Changelog entries
	baseSHA := pr.GetPullRequest().GetBase().GetSHA()
	countCmd := exec.Command(
		"sh", "-c",
		"git log "+baseSHA+"...pr_"+prNumber+" | grep -i -e \"^    Changelog:\" | grep -v -i -e \"^    Changelog: *none\" | wc -l")
	countCmd.Dir = state.Dir
	out, err := countCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v returned error: %s: %s", countCmd.Args, out, err.Error())
	}

	changelogs, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if changelogs == 0 {
		log.Infof("Found no changelog entries, ignoring cherry-pick suggestions")
		return nil
	}

	// fetch all the branches
	_, err = git.Command("fetch", "github").With(state).Run()
	if err != nil {
		return err
	}

	// get list of release versions
	versions, err := getLatestIntegrationRelease(3, conf)
	if err != nil {
		return err
	}
	releaseBranches := []string{}
	for _, version := range versions {
		releaseBranch, err := getServiceRevisionFromIntegration(repo, "origin/"+version, conf)
		if err != nil {
			return err
		} else if releaseBranch != "" {
			releaseBranches = append(releaseBranches, releaseBranch+" (release "+version+")")
		}
	}

	// no suggestions, stop here
	if len(releaseBranches) == 0 {
		return nil
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
	if err := githubClient.CreateComment(context.Background(), githubOrganization,
		pr.GetRepo().GetName(), pr.GetNumber(), &comment); err != nil {
		log.Infof("Failed to comment on the pr: %v, Error: %s", pr, err.Error())
		return err
	}
	return nil
}

func branchIsABranchInRepository(conf *config, log *logrus.Entry, repository, queryBranch string) (bool, error) {
	tdir, err := ioutil.TempDir("", "gitcmd")
	if err != nil {
		log.Errorf("Failed to create tempdir with error: %s", err)
		return false, err
	}
	defer os.Remove(tdir)
	branches, err := git.Command(
		"ls-remote",
		"--heads",
		getRemoteURLGitHub(conf.githubProtocol, "mendersoftware", repository),
	).With(&git.State{Dir: tdir}).Run()
	if err != nil {
		return false, err
	}
	reg := regexp.MustCompile("\\w+\trefs/heads/(\\S+)")
	for _, line := range strings.Split(branches, "\n") {
		res := reg.FindStringSubmatch(line)
		if len(res) != 2 {
			continue
		}
		if res[1] == queryBranch {
			return true, nil
		}
	}
	return false, nil

}

func cherryPickToBranch(log *logrus.Entry, comment *github.IssueCommentEvent, pr *github.PullRequest, conf *config, targetBranch string,
	client clientgithub.Client,
) (*github.PullRequest, error) {

	ok, err := branchIsABranchInRepository(conf, log, comment.GetRepo().GetName(), targetBranch)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s is not a branch in %s", targetBranch, comment.GetRepo().GetName())
	}

	prBranchName := fmt.Sprintf("cherry-%s-%s",
		targetBranch, pr.GetHead().GetRef())
	state, err := git.Commands(
		git.Command("init", "."),
		git.Command("remote", "add", "mendersoftware",
			getRemoteURLGitHub(conf.githubProtocol, "mendersoftware", comment.GetRepo().GetName())),
		git.Command("fetch", "mendersoftware"),
		git.Command("checkout", "mendersoftware/"+targetBranch),
		git.Command("checkout", "-b", prBranchName),
	)
	defer state.Cleanup()
	if err != nil {
		return nil, err
	}

	if _, err = git.Command("cherry-pick", "-x",
		pr.GetHead().GetSHA(), "^"+pr.GetBase().GetSHA()).
		With(state).Run(); err != nil {
		if strings.Contains(err.Error(), "conflict") {
			return nil, errorCherryPickConflict
		}
		return nil, err
	}
	if os.Getenv("MENDERGITDRY") == "" {
		if _, err = git.Command("push",
			os.Getenv("MENDERGITDRY"),
			"mendersoftware",
			prBranchName+":"+prBranchName).
			With(state).Run(); err != nil {
			return nil, err
		}
	}

	newPR := &github.NewPullRequest{
		Title: github.String(fmt.Sprintf("[Cherry %s]: %s",
			targetBranch, comment.GetIssue().GetTitle())),
		Head: github.String(prBranchName),
		Base: github.String(targetBranch),
		Body: github.String(
			fmt.Sprintf("Cherry pick of PR: %d\nFor you %s :)",
				pr.GetID(), comment.Sender.GetName())),
		MaintainerCanModify: github.Bool(true),
	}
	newPRRes, err := client.CreatePullRequest(
		context.Background(),
		githubOrganization,
		comment.GetRepo().GetName(),
		newPR)
	if err != nil {
		return nil, fmt.Errorf("Failed to create the PR for: (%s) %v",
			comment.GetRepo().GetName(), err)
	}
	return newPRRes, nil
}

func cherryPickPR(
	log *logrus.Entry,
	comment *github.IssueCommentEvent,
	pr *github.PullRequest,
	conf *config,
	body string,
	githubClient clientgithub.Client,
) error {
	targetBranches := parseCherryTargetBranches(body)
	if len(targetBranches) == 0 {
		return fmt.Errorf("No target branches found in the comment body: %s", body)
	}
	// Translate the Mender release target branches, to the given version of the repository
	releaseBranches := []string{}
	for _, branch := range targetBranches {
		releaseBranch, err := getServiceRevisionFromIntegration(
			comment.GetRepo().GetName(),
			"origin/"+branch,
			conf,
		)
		if err != nil {
			return err
		} else if releaseBranch != "" {
			log.Infof(
				"Cherry picking to version %s of %s in Mender release %s",
				releaseBranch,
				comment.GetRepo().GetName(),
				branch)
			releaseBranches = append(releaseBranches, releaseBranch)
		} else {
			return fmt.Errorf(
				"Did not find a release branch for: %s in %s",
				branch,
				comment.GetRepo().GetName())
		}
	}
	conflicts := make(map[string]bool)
	errors := make(map[string]string)
	success := make(map[string]string)
	for _, targetBranch := range releaseBranches {
		if _, err := cherryPickToBranch(log, comment, pr, conf, targetBranch, githubClient); err != nil {
			if err == errorCherryPickConflict {
				conflicts[targetBranch] = true
				continue
			}
			log.Errorf("Failed to cherry pick: %s to %s, err: %s",
				comment.GetIssue().GetTitle(), targetBranch, err)
			errors[targetBranch] = err.Error()
		} else {
			success[targetBranch] = fmt.Sprintf("#%d", pr.GetNumber())
		}
	}
	// Comment with cherry links on the PR
	commentText := `Hi :smileycat:
I did my very best, and this is the result of the cherry pick operation:
`
	for _, targetBranch := range targetBranches {
		if !conflicts[targetBranch] && errors[targetBranch] != "" {
			commentText = commentText +
				fmt.Sprintf("\t* %s :red_check_mark: Error: %s\n", targetBranch, errors[targetBranch])
		} else if success[targetBranch] != "" {
			commentText = commentText +
				fmt.Sprintf("\t* %s :white_check_mark: %s\n", targetBranch, success[targetBranch])
		} else {
			commentText = commentText +
				fmt.Sprintf("\t* %s Had merge conflicts, you will have to fix this yourself :crying_cat_face:\n", targetBranch)
		}
	}

	commentBody := github.IssueComment{
		Body: &commentText,
	}
	if err := githubClient.CreateComment(
		context.Background(),
		githubOrganization,
		comment.GetRepo().GetName(),
		pr.GetNumber(),
		&commentBody,
	); err != nil {
		log.Infof("Failed to comment on the pr: %v, Error: %s", pr, err.Error())
		return err
	}
	return nil
}

func parseCherryTargetBranches(body string) []string {
	matches := []string{}
	for _, line := range strings.Split(body, "\n") {
		if m := regexp.MustCompile(" *\\* *([0-9]+\\.[0-9]+\\.x)").FindStringSubmatch(line); len(m) > 1 {
			matches = append(matches, m[1])
		}
	}
	return matches
}
