package main

import (
	"testing"

	"gotest.tools/assert"
)

func TestGetRemoteURLGitHub(t *testing.T) {
	url := getRemoteURLGitHub(GitProtocolSSH, "mendersoftware", "workflows")
	assert.Equal(t, "git@github.com:/mendersoftware/workflows.git", url)

	url = getRemoteURLGitHub(GitProtocolHTTP, "mendersoftware", "workflows")
	assert.Equal(t, "https://github.com/mendersoftware/workflows", url)
}
