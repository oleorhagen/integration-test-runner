# integration test runner bot

## Main features

### `mender-qa` Pipeline run

Set `WATCH_REPOS_PIPELINE` for the list of repositories for which to run
`mender-qa` Pipeline. See source code for defaults.

This is a Mender specific feature.

### GitHub -> GitLab sync

Set `WATCH_REPOS_SYNC` for the list of repositories for which to do
GitHub->Gitlab git branches sync. Default is a list of the Mender Enterprise
repositories.

### GitLab PR branches

For all repositories in the organization, a pr_XXX branch will be created in
GitLab for every pull/XXX PR from GitHub.

## Infrastructure

It's currently hosted on `company-websites` GKE Kubernetes cluster.

## Continuous Delivery

Commits to the `master` branch trigger a sync with the `sre-tools` repository, committing the new Docker image's SHA256 to the file `kubernetes/mender-test-runner/test-runner-deployment.yaml`. This, in turn, triggers a new application of the Kubernetes manifest files to the cluster.

### Setup access to GKE

1. create service account with the following roles assigned: `Kubernetes Engine Developer`, `Kubernetes Engine Service Agent` and `Viewer`
2. create json key and make base64 encoded hash with removing new lines: `base64 /path/to/saved-key.json | tr -d \\n`
3. in CI/CD project settings add `GCLOUD_SERVICE_KEY` variable where value is the hash

### Disaster Recovery

Apply secret from mystico:

```bash
$ pass mender/saas/k8s/gke/secret-test-runner-mender-io.yaml | kubectl apply -f -
```

From the `sre-tools` repository:

```bash
$ kubectl apply -Rf kubernetes/mender-test-runner/
```
