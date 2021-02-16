package main

import (
	"os/exec"
)

func init() {
	// import the github.com SSH public key
	cmd := exec.Command("sh", "-c", "! [ -f $HOME/.ssh/known_hosts ] && "+
		"mkdir -p $HOME/.ssh && ssh-keyscan github.com | "+
		"tee -a $HOME/.ssh/known_hosts && chmod 600 $HOME/.ssh/known_hosts || true")
	err := cmd.Run()
	if err != nil {
		panic(err)
	}
	// import the SSH private key
	cmd = exec.Command("sh", "-c", "[ -n \"$SSH_PRIVATE_KEY\" ] && ! [ -f $HOME/.ssh/id_rsa ] && "+
		"echo \"$SSH_PRIVATE_KEY\" | tr -d '\r' >> $HOME/.ssh/id_rsa && chmod 600 $HOME/.ssh/id_rsa || true")
	err = cmd.Run()
	if err != nil {
		panic(err)
	}
}
