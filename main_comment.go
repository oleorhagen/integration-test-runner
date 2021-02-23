package main

import (
	"strconv"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/gin-gonic/gin"
	"github.com/google/go-github/v28/github"

	clientgithub "github.com/mendersoftware/integration-test-runner/client/github"
)

func processGitHubComment(ctx *gin.Context, comment *github.IssueCommentEvent, githubClient clientgithub.Client, conf *config) error {
	log := getCustomLoggerFromContext(ctx)

	// process created actions only, ignore the others
	action := comment.GetAction()
	if action != "created" {
		log.Infof("Ignoring action %s on comment", action)
		return nil
	}

	// accept commands only from organization members
	if !githubClient.IsOrganizationMember(ctx, githubOrganization, comment.Sender.GetLogin()) {
		log.Warnf("%s commented, but he/she is not a member of our organization, ignoring", comment.Sender.GetLogin())
		return nil
	}

	// filter comments mentioning the bot
	body := comment.Comment.GetBody()
	if !strings.Contains(body, "@"+githubBotName) {
		log.Info("ignoring comment not mentioning me")
		return nil
	}

	// retrieve the pull request
	prLink := comment.Issue.GetPullRequestLinks().GetURL()
	if prLink == "" {
		log.Warnf("ignoring comment not on a pull request")
		return nil
	}

	prLinkParts := strings.Split(prLink, "/")
	prNumber, err := strconv.Atoi(prLinkParts[len(prLinkParts)-1])
	if err != nil {
		log.Errorf("Unable to retrieve the pull request: %s", err.Error())
		return err
	}

	pr, err := githubClient.GetPullRequest(ctx, githubOrganization, comment.GetRepo().GetName(), prNumber)
	if err != nil {
		log.Errorf("Unable to retrieve the pull request: %s", err.Error())
		return err
	}

	// extract the command and check it is valid
	switch {
	case strings.Contains(body, commandStartPipeline):
		// make sure we only parse one pr at a time, since we use release_tool
		mutex.Lock()

		// get the list of builds
		prRequest := &github.PullRequestEvent{
			Repo:        comment.GetRepo(),
			Number:      github.Int(pr.GetNumber()),
			PullRequest: pr,
		}
		builds := parsePullRequest(log, conf, "opened", prRequest)
		log.Infof("%s:%d will trigger %d builds", comment.GetRepo().GetName(), pr.GetNumber(), len(builds))

		// release the mutex
		mutex.Unlock()

		// start the builds
		for idx, build := range builds {
			log.Infof("%d: "+spew.Sdump(build)+"\n", idx+1)
			if build.repo == "meta-mender" && build.baseBranch == "master-next" {
				log.Info("Skipping build targeting meta-mender:master-next")
				continue
			}
			if err := triggerBuild(log, conf, &build, prRequest); err != nil {
				log.Errorf("Could not start build: %s", err.Error())
			}
		}
	default:
		log.Warnf("no command found: %s", body)
		return nil
	}

	return nil
}
