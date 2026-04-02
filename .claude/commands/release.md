---
description: Release gateway and/or relay — bumps versions, updates docs if needed, commits, tags and pushes.
allowed-tools: Read, Edit, Write, Glob, Grep, Bash(git *), Bash(go build *), Bash(go vet *)
---

## Context

- Git status: !`git status --short`
- Recent commits since last tag: !`git log --oneline $(git describe --tags --abbrev=0 2>/dev/null || echo "")..HEAD 2>/dev/null | head -20`
- Current tags: !`git tag --sort=-version:refname | head -10`
- Current Chart.yaml: !`cat helm/gateway/Chart.yaml`
- Current values image.tag: !`grep 'tag:' helm/gateway/values.yaml | head -1`
- Current CHANGELOG (last 30 lines): !`tail -30 CHANGELOG.md`

## Pre-task — documentation check

Before doing anything else, review the recent commits listed above and answer:

1. Do `README.md` or `helm/gateway/README.md` need to be updated to reflect changes in the recent commits?
   - New features or behaviour changes not yet documented?
   - Version references out of date?
   - New config fields not in the parameter tables?

If yes, update the docs now (Read the files first, then Edit). Be surgical — only update what is actually missing or wrong.

## Task

Once docs are up to date, perform the release steps below. Ask the user which components to release if not specified in the invocation arguments (`$ARGUMENTS`): `gateway`, `relay`, or `all`.

### Step 1 — Determine new versions

Based on `$ARGUMENTS` and the recent commits, determine:
- New gateway version (e.g. `v0.4.12`) — increment from the latest `gateway/vX.Y.Z` tag
- New relay version (e.g. `v0.4.8`) — increment from the latest `relay/vX.Y.Z` tag
- New Helm chart version (e.g. `0.5.5`) — increment patch if only appVersion changed, minor if chart templates changed

Present the proposed versions to the user and wait for confirmation before proceeding.

### Step 2 — Update files

For each component being released, update:

**If releasing gateway:**
- `helm/gateway/values.yaml` → `image.tag: "vX.Y.Z"`
- `helm/gateway/Chart.yaml` → `appVersion: "vX.Y.Z"` and bump `version`
- `CHANGELOG.md` → add entry under `## Gateway` with today's date

**If releasing relay:**
- `CHANGELOG.md` → add entry under `## Relay` with today's date

**If Helm chart version changed:**
- `CHANGELOG.md` → add entry under `## Helm chart (kevent-gateway)`

### Step 3 — Commit

Stage and commit all modified files:

```
git add helm/gateway/Chart.yaml helm/gateway/values.yaml CHANGELOG.md README.md helm/gateway/README.md
git commit -m "release: gateway vX.Y.Z / relay vX.Y.Z / chart 0.X.Y"
```

(Only include components actually being released in the commit message.)

### Step 4 — Tag and push

```bash
# Tag the components being released
git tag gateway/vX.Y.Z   # if releasing gateway
git tag relay/vX.Y.Z     # if releasing relay

# Push commits and tags
git push origin main
git push origin gateway/vX.Y.Z   # if releasing gateway
git push origin relay/vX.Y.Z     # if releasing relay
```

Confirm each push with the user before executing.

### Step 5 — Summary

Print a summary:
- Tags created and pushed
- Docker images that CI will build (ghcr.io/ia-generative/kevent-ai/gateway:vX.Y.Z, etc.)
- Reminder: update relay image tag in `k8s/inference-transcription.yaml` on the cluster (file is gitignored)
