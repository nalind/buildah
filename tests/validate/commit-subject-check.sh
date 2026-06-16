#!/usr/bin/env bash
#
# Copied from from Podman hack/commit-subject-check.sh.
#
# Check that commit subject lines in a given range are non-empty and no
# longer than $MAX chars. Replaces the former github actions
# tim-actions/commit-message-checker-with-regex check.
#
# Usage: tests/validate/commit-subject-check.sh <git-range>
#   e.g. tests/validate/commit-subject-check.sh origin/main..HEAD

set -euo pipefail

MAX=90

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <git-range>" >&2
    exit 2
fi

range=$1
rc=0

while IFS=$'\t' read -r sha subject; do
    len=${#subject}
    if [[ $len -eq 0 ]]; then
        echo "$sha: empty commit subject" >&2
        rc=1
    elif [[ $len -ge $MAX ]]; then
        echo "$sha: subject is $len chars (must be < $MAX): $subject" >&2
        rc=1
    fi
done < <(git log --no-merges --format='%H%x09%s' "$range")

exit $rc
