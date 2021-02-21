package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetRemoteURLGitHub(t *testing.T) {
	url := getRemoteURLGitHub(GitProtocolSSH, "mendersoftware", "workflows")
	assert.Equal(t, "git@github.com:/mendersoftware/workflows.git", url)

	url = getRemoteURLGitHub(GitProtocolHTTP, "mendersoftware", "workflows")
	assert.Equal(t, "https://github.com/mendersoftware/workflows", url)
}

func TestGetRemoteURLGitLab(t *testing.T) {
	url, err := getRemoteURLGitLab("mendersoftware", "workflows")
	assert.NoError(t, err)
	assert.Equal(t, "git@gitlab.com:Northern.tech/Mender/workflows", url)

	url, err = getRemoteURLGitLab("mendersoftware", "saas")
	assert.NoError(t, err)
	assert.Equal(t, "git@gitlab.com:Northern.tech/MenderSaaS/saas", url)

	url, err = getRemoteURLGitLab("unknown", "saas")
	assert.Error(t, err)
	assert.Equal(t, "", url)
}
