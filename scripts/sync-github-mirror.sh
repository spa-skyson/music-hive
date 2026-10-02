#!/usr/bin/env bash
# Publish a snapshot of the current tree to the public GitHub mirror.
#
# The mirror does not carry the full development history. Instead, each
# publication creates a fresh snapshot commit on top of the previous one,
# so the public repository shows a single linear chain of published states.
#
# Usage: scripts/sync-github-mirror.sh [REF]
#   REF            branch or commit to publish (default: main)
#
# Environment:
#   MIRROR_REMOTE  git remote pointing to the public mirror (default: github)
#   GIT_AUTHOR_*   override the publication identity (author name/email); the
#                  committer always follows the author, a direct GIT_COMMITTER_*
#                  override is not supported. By default snapshots use the
#                  account's noreply address so that email privacy restrictions
#                  on the hosting side never reject a push
set -euo pipefail

# GitHub rejects pushes that expose a private email, so publication commits
# default to the account's noreply identity. Override via environment
# (GIT_AUTHOR_*); the committer always follows the author.
MIRROR_AUTHOR_NAME="${GIT_AUTHOR_NAME:-Alexander Gazal}"
MIRROR_AUTHOR_EMAIL="${GIT_AUTHOR_EMAIL:-88609358+spa-skyson@users.noreply.github.com}"
export GIT_AUTHOR_NAME="$MIRROR_AUTHOR_NAME" GIT_AUTHOR_EMAIL="$MIRROR_AUTHOR_EMAIL"
export GIT_COMMITTER_NAME="$MIRROR_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$MIRROR_AUTHOR_EMAIL"

REF="${1:-main}"
MIRROR_REMOTE="${MIRROR_REMOTE:-github}"
MIRROR_BRANCH="main"

if ! git rev-parse --verify --quiet "${REF}^{commit}" >/dev/null; then
	echo "error: ref '${REF}' not found locally" >&2
	exit 1
fi

if ! git remote get-url "$MIRROR_REMOTE" >/dev/null 2>&1; then
	echo "error: remote '${MIRROR_REMOTE}' is not configured" >&2
	exit 1
fi

if [ "$REF" != "$MIRROR_BRANCH" ]; then
	echo "warning: publishing '${REF}' which is not 'main'" >&2
fi

TREE=$(git rev-parse "${REF}^{tree}")

PARENT=$(git ls-remote "$MIRROR_REMOTE" "refs/heads/${MIRROR_BRANCH}" | awk 'NR==1{print $1}')

MESSAGE="chore(mirror): publish snapshot of ${REF} ($(git rev-parse --short "$REF"))"

if [ -n "$PARENT" ]; then
	COMMIT=$(git commit-tree "$TREE" -p "$PARENT" -m "$MESSAGE")
else
	COMMIT=$(git commit-tree "$TREE" -m "$MESSAGE")
fi

git push "$MIRROR_REMOTE" "${COMMIT}:refs/heads/${MIRROR_BRANCH}" --force

echo "Published snapshot of ${REF} ($(git rev-parse --short "$REF")) to ${MIRROR_REMOTE}/${MIRROR_BRANCH} as ${COMMIT}"
