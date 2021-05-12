package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/google/go-github/v28/github"
	"github.com/mendersoftware/integration-test-runner/git"
	"github.com/sirupsen/logrus"

	clientgithub "github.com/mendersoftware/integration-test-runner/client/github"
)

// syncIfOSHasEnterpriseRepo detects whether a commit has been merged to
// the Open Source edition of a repo, and then creates a PR-branch
// in the Enterprise edition, which is then used in order to open
// a PR to the Enterprise repo with the OS changes.
func syncIfOSHasEnterpriseRepo(log *logrus.Entry, conf *config, gpr *github.PullRequestEvent) error {

	repo := gpr.GetRepo()
	if repo == nil {
		return fmt.Errorf("syncIfOSHasEnterpriseRepo: Failed to get the repository information")
	}

	// Enterprise repo sentinel
	switch repo.GetName() {
	case "deployments":
	case "inventory":
	case "useradm":
	case "workflows":
	default:
		log.Debugf("syncIfOSHasEnterpriseRepo: Repository without Enterprise fork detected: (%s). Not syncing", repo.GetName())
		return nil
	}

	pr := gpr.GetPullRequest()
	if pr == nil {
		return errors.New("syncIfOSHasEnterpriseRepo: Failed to get the pull request")
	}

	// If the action is "closed" and the "merged" key is "true", the pull request was merged.
	// While webhooks are also triggered when a pull request is synchronized, Events API timelines
	// don't include pull request events with the "synchronize" action.
	if gpr.GetAction() == "closed" && pr.GetMerged() {

		// Only sync on Merges to master, release and feature branches.
		// Verify release branches.
		branch := pr.GetBase()
		if branch == nil {
			return fmt.Errorf("syncIfOSHasEnterpriseRepo: Failed to get the base-branch of the PR: %v", branch)
		}

		syncBranches := regexp.MustCompile(`(master|[0-9]+\.[0-9]+\.x|` + featureBranchPrefix + `.+)`)
		branchRef := branch.GetRef()
		if branchRef == "" {
			return fmt.Errorf("Failed to get the branch-ref from the PR: %v", pr)
		}
		if !syncBranches.MatchString(branchRef) {

			log.Debugf("syncIfOSHasEnterpriseRepo: Detected a merge into another branch than master or a release branch: (%s), no syncing done", branchRef)

		} else {

			log.Infof("syncIfOSHasEnterpriseRepo: Merge to (%s) in an OS repository detected. Syncing the repositories...", branchRef)

			PRNumber := strconv.Itoa(pr.GetNumber())
			PRBranchName := "mergeostoent_" + PRNumber

			merged, err := createPRBranchOnEnterprise(log, repo.GetName(), branchRef, PRNumber, PRBranchName, conf)
			if err != nil {
				return fmt.Errorf("syncIfOSHasEnterpriseRepo: Failed to create the PR branch on the Enterprise repo due to error: %v", err)
			}

			// Get the link to the original PR, so that it can be linked to
			// in the commit body
			PRURL := pr.GetHTMLURL()

			enterprisePR, err := createPullRequestFromTestBotFork(createPRArgs{
				conf:        conf,
				repo:        repo.GetName() + "-enterprise",
				prBranch:    githubBotName + ":" + PRBranchName,
				baseBranch:  branchRef,
				message:     fmt.Sprintf("[Bot] %s", pr.GetTitle()),
				messageBody: fmt.Sprintf("Original PR: %s\n\n%s", PRURL, pr.GetBody()),
			})
			if err != nil {
				return fmt.Errorf("syncIfOSHasEnterpriseRepo: Failed to create a PR with error: %v", err)
			}

			log.Infof("syncIfOSHasEnterpriseRepo: Created PR: %d on Enterprise/%s/%s",
				enterprisePR.GetNumber(), repo.GetName(), branchRef)
			log.Debugf("syncIfOSHasEnterpriseRepo: Created PR: %v", pr)
			log.Debug("Trying to @mention the user in the newly created PR")
			userName := pr.GetMergedBy().GetLogin()
			log.Debugf("userName: %s", userName)

			if userName != "" {
				err = commentToNotifyUser(log, commentArgs{
					pr:             enterprisePR,
					conf:           conf,
					mergeConflicts: !merged,
					repo:           repo.GetName() + "-enterprise",
					userName:       userName,
					prBranchName:   PRBranchName,
					branchName:     branchRef,
				})
				if err != nil {
					log.Errorf("syncIfOSHasEnterpriseRepo: %s", err.Error())
				}
			}

		}

	}

	return nil
}

// createPRBranchOnEnterprise creates a new branch in the Enterprise repository
// starting at the branch in which to sync, with the name 'PRBranchName'
// and merges this with the OS equivalent of 'branchName'.
func createPRBranchOnEnterprise(log *logrus.Entry, repo, branchName, PRNumber, PRBranchName string, conf *config) (merged bool, err error) {

	state, err := git.Commands(
		git.Command("init", "."),
		git.Command("remote", "add", "opensource",
			getRemoteURLGitHub(conf.githubProtocol, githubOrganization, repo)),
		git.Command("remote", "add", "enterprise",
			getRemoteURLGitHub(conf.githubProtocol, githubOrganization, repo+"-enterprise")),
		git.Command("remote", "add", githubBotName,
			getRemoteURLGitHub(conf.githubProtocol, githubBotName, repo+"-enterprise")),
		git.Command("config", "--add", "user.name", githubBotName),
		git.Command("config", "--add", "user.email", "mender@northern.tech"),
		git.Command("fetch", "opensource", branchName),
		git.Command("fetch", "enterprise", branchName+":"+PRBranchName),
		git.Command("checkout", PRBranchName),
	)
	defer state.Cleanup()
	if err != nil {
		return false, err
	}

	// Merge the OS branch into the PR branch
	mergeMsg := fmt.Sprintf("Merge OS base branch: (%s) including PR: (%s) into Enterprise: (%[1]s)",
		branchName, PRNumber)
	log.Debug("Trying to " + mergeMsg)
	gitcmd := exec.Command("git", "merge", "-m", mergeMsg, "opensource/"+branchName)
	gitcmd.Dir = state.Dir
	out, err := gitcmd.CombinedOutput()
	merged = true
	if err != nil {
		merged = false
		if strings.Contains(string(out), "Automatic merge failed") {
			msg := "Merge conflict detected. Still pushing the Enterprise PR branch, " +
				"and creating the PR, so that the user can manually resolve, " +
				"and re-push to the PR once these are fixed"
			log.Warn(msg)
		} else {
			return false, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}
	}

	if !merged {
		// In case of a failed merge, reset PRBranchName to opensource/branchName
		// and push this branch to enterprise
		gitcmd = exec.Command("git", "reset", "--hard", "opensource/"+branchName)
		gitcmd.Dir = state.Dir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return merged, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}
	}

	// Push the branch to the bot's own fork
	gitcmd = exec.Command("git", "push", "--set-upstream", githubBotName, PRBranchName)
	gitcmd.Dir = state.Dir
	out, err = gitcmd.CombinedOutput()
	if err != nil {
		return merged, fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
	}

	if merged {
		log.Infof("Merged branch: opensource/%s/%s into enterprise/%[1]s/%s in the Enterprise repo",
			repo, branchName, PRBranchName)
	} else {
		msg := "Failed to merge opensource/%s/%s into enterprise/%[1]s/%s in the Enterprise repo. " +
			"Therefore pushed opensource/%[1]s/%[2]s to %s so that " +
			"merging can be done by a human locally"
		log.Infof(msg, repo, branchName, PRBranchName, PRBranchName)
	}

	return merged, nil
}

type createPRArgs struct {
	conf        *config
	repo        string
	prBranch    string
	baseBranch  string
	message     string
	messageBody string
}

func createPullRequestFromTestBotFork(args createPRArgs) (*github.PullRequest, error) {

	newPR := &github.NewPullRequest{
		Title:               github.String(args.message),
		Head:                github.String(args.prBranch),
		Base:                github.String(args.baseBranch),
		Body:                github.String(args.messageBody),
		MaintainerCanModify: github.Bool(true),
	}

	client := clientgithub.NewGitHubClient(args.conf.githubToken)
	pr, err := client.CreatePullRequest(context.Background(), githubOrganization, args.repo, newPR)
	if err != nil {
		return nil, fmt.Errorf("Failed to create the PR for: (%s) %v", args.repo, err)
	}

	return pr, nil
}

type commentArgs struct {
	pr             *github.PullRequest
	conf           *config
	mergeConflicts bool
	repo           string
	userName       string
	prBranchName   string
	branchName     string
}

func commentToNotifyUser(log *logrus.Entry, args commentArgs) error {

	// Post a comment, and @mention the user
	var commentBody string
	if !args.mergeConflicts {
		commentBody = fmt.Sprintf("@%s I have created a PR for you, ready to merge as soon as tests are passed", args.userName)
	} else {
		tmplString := `
@{{.UserName}} I have created a PR for you.

Unfortunately, a merge conflict was detected. This means that the conflict will have to be resolved manually by you, human. Then pushed to the PR-branch, once all conflicts are resolved.
This can be done by following:

<details>
    <summary><small>this</small> recipe</summary><p>

1. Make sure that the '{{.GitHubBotName}}' remote is present in your repository, or else add it with:
    1. {{.BackQuote}}git remote add {{.GitHubBotName}} git@github.com:{{.GitHubBotName}}/{{.Repo}}.git{{.BackQuote}}

2. Fetch the remote branches
    1. {{.BackQuote}}git fetch origin {{.BranchName}}:localtmp{{.BackQuote}}
    2. {{.BackQuote}}git fetch {{.GitHubBotName}} {{.PRBranchName}}{{.BackQuote}}

3. Checkout the localtmp branch
    1. {{.BackQuote}}git checkout localtmp{{.BackQuote}}

4. Merge the branch into the PR branch
    1. {{.BackQuote}}git merge {{.GitHubBotName}}/{{.PRBranchName}}{{.BackQuote}}

5. Resolve all conflicts

6. Commit the merged changes

7. Push to the PR branch
    1. {{.BackQuote}}git push {{.GitHubBotName}} localtmp:{{.PRBranchName}}{{.BackQuote}}

 </p></details>
`
		tmpl, err := template.New("Main").Parse(tmplString)
		if err != nil {
			log.Error("The text template should never fail to render!")
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, struct {
			UserName      string
			Repo          string
			PRBranchName  string
			BranchName    string
			BackQuote     string
			GitHubBotName string
		}{
			UserName:      args.userName,
			Repo:          args.repo,
			PRBranchName:  args.prBranchName,
			BranchName:    args.branchName,
			BackQuote:     "`",
			GitHubBotName: githubBotName,
		}); err != nil {
			log.Errorf("Failed to execute the merge-conflict PR template string. Error: %s", err.Error())
		}
		commentBody = buf.String()
	}
	comment := github.IssueComment{
		Body: &commentBody,
	}

	client := clientgithub.NewGitHubClient(args.conf.githubToken)
	return client.CreateComment(context.Background(), githubOrganization, args.repo, args.pr.GetNumber(), &comment)
}
