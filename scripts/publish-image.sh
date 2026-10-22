#!/bin/sh
# Publish one multi-platform build under its release tag and latest together.
set -eu

usage='Usage: publish-image.sh repository:version [platforms]'
die() {
    printf '%s\n' "$*" >&2
    exit 2
}

image=${1:?$usage}
platforms=${2:-linux/amd64,linux/arm64}

case "$image" in
  *@*)
    die 'A version-tagged image reference is required, not a digest.' ;;
  *[!A-Za-z0-9._:/-]*)
    die 'The image reference contains unsupported characters.' ;;
esac

case "${image##*/}" in
  *:*) ;;
  *) die 'An explicit image version tag is required.' ;;
esac

repository=${image%:*}
tag=${image##*:}

[ -n "$repository" ] || die 'A repository is required before the version tag.'
[ -n "$tag" ] || die 'An explicit image version tag is required.'

# Docker reserves "latest" as the default tag; it is added below and must not
# be supplied as the release version. The Dockerfile's non-release defaults
# must not become published version labels either.
case "$tag" in
  [Ll][Aa][Tt][Ee][Ss][Tt])
    die 'Supply a version tag; latest is added automatically.' ;;
  dev|unknown|local)
    die 'Supply a release version tag; dev, unknown and local are non-release build defaults.' ;;
esac

# Validate the repository separately from the tag so registry ports such as
# registry.example:5000/team/app:0.4.2 keep working. This is intentionally a
# conservative reference check, not a replacement for Docker's own parser.
if ! printf '%s\n' "$repository" | LC_ALL=C grep -Eq '^[a-z0-9]+([._-]+[a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-]+[a-z0-9]+)*)*$'; then
  die 'The repository must be a lowercase Docker repository, optionally with a registry port.'
fi
if [ "${#tag}" -gt 128 ] || ! printf '%s\n' "$tag" | LC_ALL=C grep -Eq '^[A-Za-z0-9_][A-Za-z0-9_.-]*$'; then
  die 'The version tag is not a valid Docker tag.'
fi

# Resolve the checkout from this script, not from the caller's current
# directory. The guard intentionally uses Git's normal status semantics:
# tracked and untracked non-ignored files block publication, while ignored
# local site files, databases and build output do not.
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)

if ! worktree=$(git -C "$repo_root" rev-parse --is-inside-work-tree 2>/dev/null); then
  die "Publication requires a Git checkout at $repo_root."
fi
[ "$worktree" = true ] || die "Publication requires a non-bare Git worktree at $repo_root."

if ! revision=$(git -C "$repo_root" rev-parse HEAD 2>/dev/null); then
  die 'Could not resolve the current Git revision.'
fi
case "$revision" in
  ''|*[!0-9a-f]*) die 'The current Git revision is not a lowercase hexadecimal object ID.' ;;
esac
case "${#revision}" in
  40|64) ;;
  *) die 'The current Git revision has an unexpected length.' ;;
esac

if ! status=$(git -C "$repo_root" status --porcelain=v1 --untracked-files=normal); then
  die "Could not inspect the Git working tree at $repo_root."
fi
if [ -n "$status" ]; then
  printf '%s\n' 'Refusing to publish from a dirty tree. Commit or remove these tracked/untracked changes:' >&2
  printf '%s\n' "$status" >&2
  exit 1
fi

# The archive is built from the exact commit, never from the working tree.
# Recheck HEAD so a concurrent commit/checkout cannot make the clean-tree
# result describe a different revision.
if ! revision_after=$(git -C "$repo_root" rev-parse HEAD 2>/dev/null); then
  die 'Could not re-resolve the current Git revision.'
fi
[ "$revision_after" = "$revision" ] || die 'Git HEAD changed during publication checks.'

cd "$repo_root"

# Build the Docker context from tracked files at the reviewed commit. This
# excludes ignored files even under cmd/ or internal/, and prevents edits made
# after the clean-tree check from entering the image. The temporary file is
# created before Docker and removed by the exit trap; archive creation is
# fail-closed and Docker is never called without it.
archive=
cleanup() {
  if [ -n "$archive" ]; then
    rm -f "$archive"
  fi
}
trap cleanup 0
trap 'exit 1' 1 2 15

if ! archive=$(mktemp "${TMPDIR:-/tmp}/kubeflow-admin-context.XXXXXX"); then
  die 'Could not create a temporary source archive.'
fi
if ! git -C "$repo_root" archive --format=tar --output="$archive" "$revision"; then
  die 'Could not create the source archive from the Git revision.'
fi
if [ ! -f "$archive" ] || [ ! -s "$archive" ]; then
  die 'The source archive is empty.'
fi

# Keep the clean-tree result tied to the exact revision. The archive already
# fixes the source bytes, but a concurrent commit/checkout should still stop
# publication rather than silently describe a newer HEAD than the one checked.
if ! revision_final=$(git -C "$repo_root" rev-parse HEAD 2>/dev/null); then
  die 'Could not re-resolve the current Git revision after archiving.'
fi
[ "$revision_final" = "$revision" ] || die 'Git HEAD changed during source archiving.'

docker buildx build --platform "$platforms" --provenance=true --sbom=true \
  --build-arg VERSION="$tag" --build-arg REVISION="$revision" \
  --push -t "$image" -t "$repository:latest" - < "$archive"
