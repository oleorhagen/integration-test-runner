package git

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"

	"github.com/pkg/errors"
)

type gitCmd struct {
	Dir string
	cmd *exec.Cmd
}

func (g *gitCmd) With(s *State) *gitCmd {
	g.Dir = s.Dir
	return g
}

type State struct {
	Dir string
}

func (s *State) Cleanup() {
	if s.Dir != "" {
		os.RemoveAll(s.Dir)
	}
}

func cleanupTempDir(dir string) func() {
	return func() { os.RemoveAll(dir) }
}

func Commands(cmds ...*gitCmd) (*State, error) {
	tdir, err := ioutil.TempDir("", "gitcmd")
	if err != nil {
		return &State{}, err
	}
	s := &State{Dir: tdir}
	for _, cmd := range cmds {
		cmd.Dir = tdir
		out, err := cmd.Run()
		if err != nil {
			return s, errors.Wrapf(err,
				"git command: %s returned error:\n%s",
				cmd.cmd.Args, out)
		}
	}
	return s, nil
}

func Command(args ...string) *gitCmd {
	return &gitCmd{
		cmd: exec.Command("git", args...),
	}
}

func (g *gitCmd) Run() (string, error) {
	if g.Dir != "" {
		g.cmd.Dir = g.Dir
	}
	out, err := g.cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v returned error: %s: %s", g.cmd.Args, out, err.Error())
	}
	return string(out), nil
}
