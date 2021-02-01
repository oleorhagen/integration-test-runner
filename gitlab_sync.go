package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"
)

func syncRemoteRef(log *logrus.Entry, org, repo, ref string) error {

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

	gitcmd = exec.Command("git", "remote", "add", "github", "git@github.com:"+org+"/"+repo)
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

	if strings.Contains(ref, "tags") {
		tagName := strings.TrimPrefix(ref, "refs/tags/")

		gitcmd = exec.Command("git", "fetch", "--tags", "github")
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}

		gitcmd = exec.Command("git", "push", "-f", "gitlab", tagName)
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}
	} else if strings.Contains(ref, "heads") {
		branchName := strings.TrimPrefix(ref, "refs/heads/")

		gitcmd = exec.Command("git", "fetch", "github")
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}

		gitcmd = exec.Command("git", "checkout", "-b", branchName, "github/"+branchName)
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}

		gitcmd = exec.Command("git", "push", "-f", "gitlab", branchName)
		gitcmd.Dir = tmpdir
		out, err = gitcmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v returned error: %s: %s", gitcmd.Args, out, err.Error())
		}
	} else {
		return fmt.Errorf("Unrecognized ref %s", ref)
	}

	log.Infof("Pushed ref to GitLab: %s:%s", repo, ref)
	return nil
}
