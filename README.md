# integration test runner bot

## Infrastructure
It's currently hosted on `company-websites` GKE Kubernetes cluster. The cluster in still in POC phase, so, CD is simplified to not have a lot of time wasted if the POC won't go live.

## Continuous Delivery

### Setup access to GKE
1. create service account with the following roles assigned: `Kubernetes Engine Developer`, `Kubernetes Engine Service Agent` and `Viewer`
2. create json key and make base64 encoded hash with removing new lines: `base64 /path/to/saved-key.json | tr -d \\n`
3. in CI/CD project settings add `GCLOUD_SERVICE_KEY` variable where value is the hash

## Disaster Recovery
In `sre-tools` repo:
```
kubectl apply -Rf kubernetes/test-runner/
```
Apply secret from mystico:
```
pass mender/saas/k8s/gke/secret-test-runner-mender-io.yaml | kubectl apply -f -
```
And deploy correct image manually:
```
kubectl set image deployment/test-runner-mender-io test-runner-mender-io=$SERVICE_IMAGE
```

## Hints
Get previous revision:
```
PREV_REVISION=$(kubectl rollout history deployment test-runner-mender-io | grep ^[0-9] | awk {'print $1'} | tail -n2 | head -n1)
```
Rollback to previous revision:
```
kubectl rollout undo deployment mynginx --to-revision=$PREV_REVISION
```
