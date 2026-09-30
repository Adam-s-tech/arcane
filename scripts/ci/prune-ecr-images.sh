#!/usr/bin/env bash
set -euo pipefail

workspace=$(mktemp -d)
trap 'rm -r "$workspace"' EXIT
cutoff=$(date -u -d '1 hour ago' +%s)
for repo in manager agent; do
    directory="$workspace/$repo"
    mkdir -p "$directory/manifests"
    image="public.ecr.aws/getarcaneapp/$repo"
    aws ecr-public describe-images --repository-name "$repo" --output json >"$directory/images.json"
    jq -r '.imageDetails[].imageDigest' "$directory/images.json" >"$directory/digests"
    jq -r '.imageDetails[] | select(.imageTags | length > 0) | .imageDigest' "$directory/images.json" | sort -u >"$directory/roots"

    # Read every subject link before deciding what is safe to delete.
    while read -r digest; do
        oras manifest fetch "$image@$digest" >"$directory/manifest.json"
        jq -e '.schemaVersion == 2' "$directory/manifest.json" >/dev/null
        jq --arg digest "$digest" '. + {digest: $digest}' "$directory/manifest.json" >"$directory/manifests/$digest.json"
    done <"$directory/digests"
    jq -s '[.[] | {digest, subject: .subject.digest}]' "$directory/manifests/"*.json >"$directory/subjects.json"

    cp "$directory/roots" "$directory/pending"
    : >"$directory/keep"
    while [[ -s "$directory/pending" ]]; do
        digest=$(head -n 1 "$directory/pending")
        tail -n +2 "$directory/pending" >"$directory/remaining"
        mv "$directory/remaining" "$directory/pending"
        if grep -qxF "$digest" "$directory/keep"; then
            continue
        fi
        jq -r '.manifests[]?.digest' "$directory/manifests/$digest.json" >>"$directory/pending"
        jq -r --arg subject "$digest" '.[] | select(.subject == $subject) | .digest' "$directory/subjects.json" >>"$directory/pending"
        echo "$digest" >>"$directory/keep"
    done
    sort -u "$directory/keep" -o "$directory/keep"
    jq -r --argjson cutoff "$cutoff" '
      .imageDetails[] | select((.imageTags | length) == 0) |
      (.imagePushedAt | if type == "number" then . else sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | fromdateiso8601 end) as $pushed |
      select($pushed < $cutoff) | .imageDigest
    ' "$directory/images.json" | sort -u >"$directory/candidates"
    comm -23 "$directory/candidates" "$directory/keep" >"$directory/delete"
done

# Abort if either repository changed its tagged roots during traversal.
for repo in manager agent; do
    directory="$workspace/$repo"
    aws ecr-public describe-images --repository-name "$repo" --output json >"$directory/current.json"
    jq -r '.imageDetails[] | select(.imageTags | length > 0) | .imageDigest' "$directory/current.json" | sort -u >"$directory/current-roots"
    cmp "$directory/roots" "$directory/current-roots"
done
for repo in manager agent; do
    directory="$workspace/$repo"
    deleted=$(wc -l <"$directory/delete" | tr -d ' ')
    while [[ -s "$directory/delete" ]]; do
        args=()
        while read -r digest; do
            args+=("imageDigest=$digest")
        done < <(head -n 100 "$directory/delete")
        aws ecr-public batch-delete-image --repository-name "$repo" --image-ids "${args[@]}" --output json >"$directory/result.json"
        jq -e '(.failures // [] | length) == 0' "$directory/result.json" >/dev/null
        tail -n +101 "$directory/delete" >"$directory/remaining"
        mv "$directory/remaining" "$directory/delete"
    done
    echo "$repo: deleted $deleted stale manifests" >>"$GITHUB_STEP_SUMMARY"
done
