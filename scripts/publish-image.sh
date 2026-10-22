#!/bin/sh
# Publish one multi-platform build under its release tag and latest together.
set -eu

image=${1:?Usage: publish-image.sh repository:version [platforms]}
platforms=${2:-linux/amd64,linux/arm64}
case "$image" in
  *@*|*[!a-zA-Z0-9._:/-]*)
    echo 'A version-tagged image reference is required, not a digest.' >&2
    exit 2 ;;
esac
case "${image##*/}" in
  *:*) ;;
  *) echo 'An explicit image version tag is required.' >&2; exit 2 ;;
esac
repository=${image%:*}
tag=${image##*:}
if [ -z "$repository" ] || [ -z "$tag" ] || [ "$tag" = latest ]; then
  echo 'Supply a nonempty version tag; latest is added automatically.' >&2
  exit 2
fi

exec docker buildx build --platform "$platforms" --provenance=true --sbom=true \
  --push -t "$image" -t "$repository:latest" .
