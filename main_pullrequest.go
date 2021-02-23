package main

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/go-github/v28/github"

	clientgithub "github.com/mendersoftware/integration-test-runner/client/github"
)

func processGitHubPullRequest(ctx *gin.Context, pr *github.PullRequestEvent, githubClient clientgithub.Client, conf *config) error {
	log := getCustomLoggerFromContext(ctx)

	// Do not run if the PR is a draft
	if pr.GetPullRequest().GetDraft() {
		log.Infof("The PR: %s/%d is a draft. Do not run tests", pr.GetRepo().GetName(), pr.GetNumber())
		return nil
	}

	if isDependabotPR, err := maybeVendorDependabotPR(log, pr, conf); isDependabotPR || err != nil {
		if err != nil {
			log.Errorf("maybeVendorDependabotPR: %v", err)
		}
		return err
	}

	action := pr.GetAction()

	// To run component's Pipeline create a branch in GitLab, regardless of the PR
	// coming from an organization member or not (equivalent to the old Travis tests)
	if err := createPullRequestBranch(log, *pr.Organization.Login, *pr.Repo.Name, strconv.Itoa(pr.GetNumber()), action, conf); err != nil {
		log.Errorf("Could not create PR branch: %s", err.Error())
	}

	// Delete merged pr branches in GitLab
	if err := deleteStaleGitlabPRBranch(log, pr, conf); err != nil {
		log.Errorf("Failed to delete the stale PR branch after the PR: %v was merged or closed. Error: %v", pr, err)
	}

	// make sure we only parse one pr at a time, since we use release_tool
	mutex.Lock()

	// If the pr was merged, suggest cherry-picks
	if err := suggestCherryPicks(log, pr, githubClient, conf); err != nil {
		log.Errorf("Failed to suggest cherry picks for the pr %v. Error: %v", pr, err)
	}

	// release the mutex
	mutex.Unlock()

	// Continue to the integration Pipeline only for organization members
	if member := githubClient.IsOrganizationMember(ctx, githubOrganization, pr.Sender.GetLogin()); !member {
		log.Warnf("%s is making a pullrequest, but he/she is not a member of our organization, ignoring", pr.Sender.GetLogin())
		return nil
	}

	// make sure we only parse one pr at a time, since we use release_tool
	mutex.Lock()

	// First check if the PR has been merged. If so, stop
	// the pipeline, and do nothing else.
	if err := stopBuildsOfStalePRs(log, pr, conf); err != nil {
		log.Errorf("Failed to stop a stale build after the PR: %v was merged or closed. Error: %v", pr, err)
	}

	// Keep the OS and Enterprise repos in sync
	if err := syncIfOSHasEnterpriseRepo(log, conf, pr); err != nil {
		log.Errorf("Failed to sync the OS and Enterprise repos: %s", err.Error())
	}

	// get the list of builds
	builds := parsePullRequest(log, conf, action, pr)
	log.Infof("%s:%d would trigger %d builds", pr.GetRepo().GetName(), pr.GetNumber(), len(builds))

	// release the mutex
	mutex.Unlock()

	// do not start the builds, inform the user about the `start pipeline` command instead
	if len(builds) > 0 {
		msg := "@" + pr.GetSender().GetLogin() + ", Let me know if you want to start the integration pipeline by mentioning me and the command \"" + commandStartPipeline + "\"."
		if err := githubClient.CreateComment(ctx, pr.GetOrganization().GetLogin(), pr.GetRepo().GetName(), pr.GetNumber(), &github.IssueComment{
			Body: github.String(msg),
		}); err != nil {
			log.Infof("Failed to comment on the pr: %v, Error: %s", pr, err.Error())
		}
	}

	return nil
}
