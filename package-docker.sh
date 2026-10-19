#!/bin/bash
# linchange Docker-based .deb package builder
#
# Usage: ./package-docker.sh [version] [--output-dir DIR]
#
# Everything happens inside Docker: the host needs only Docker and this
# script, so a build on a laptop produces the same package a build server
# would. The finished .deb is copied out of the container into the output
# directory (dist/ by default).

set -euo pipefail

# Paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<EOF
Usage: ${0##*/} [version] [--output-dir DIR]

Build ${PROJECT_NAME:-linchange} .deb package inside Docker.

Positional:
  version                  Package version (default: derived from git describe)

Options:
  --output-dir DIR         Where to put the .deb and build tree (default: ./dist)
  -h, --help               Show this help

Environment:
  NO_CACHE=1               Disable Docker layer caching (slower, from scratch)
EOF
}

die() {
    echo "error: $*" >&2
    exit 1
}

# git_to_deb_version turns git describe output into a version Debian accepts.
# Debian requires the version to start with a digit, so a bare commit hash is
# prefixed; a tag keeps its digits. '~' would sort the result before the tag,
# so the describe separators are mapped onto '+' and '.' instead.
git_to_deb_version() {
    local v="$1"
    v="${v#v}"
    v="$(printf '%s' "$v" | sed -e 's/-dirty$/.dirty/' -e 's/-/+g/')"
    case "$v" in
        [0-9]*) ;;
        *) v="0.0.g${v}" ;;
    esac
    printf '%s' "$v"
}

# detect_arch answers in Debian architecture names, which is what the .deb
# control file and dpkg-deb need. dpkg is asked first because it knows Debian's
# own spellings (armhf, i386) that uname reports as armv7l and i686.
detect_arch() {
    local machine
    if command -v dpkg >/dev/null 2>&1; then
        dpkg --print-architecture
        return
    fi
    machine="$(docker info --format '{{.Architecture}}' 2>/dev/null || true)"
    [ -n "${machine}" ] || machine="$(uname -m)"
    case "${machine}" in
        x86_64)   echo amd64 ;;
        aarch64)  echo arm64 ;;
        armv7l)   echo armhf ;;
        i386|i686) echo i386 ;;
        *)        echo "${machine}" ;;
    esac
}

VERSION=""
OUTPUT_DIR=""

while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help)
            usage
            exit 0
            ;;
        --output-dir)
            [ $# -ge 2 ] || die "--output-dir needs a directory"
            OUTPUT_DIR="$2"
            shift 2
            ;;
        --output-dir=*)
            OUTPUT_DIR="${1#--output-dir=}"
            shift
            ;;
        -*)
            die "unknown option: $1 (try --help)"
            ;;
        *)
            [ -z "${VERSION}" ] || die "unexpected argument: $1 (version is already '${VERSION}')"
            VERSION="$1"
            shift
            ;;
    esac
done

if [ -z "${VERSION}" ]; then
    GIT_VERSION="$(git -C "${SCRIPT_DIR}" describe --tags --always --dirty 2>/dev/null || echo 'dev')"
    VERSION="$(git_to_deb_version "${GIT_VERSION}")"
fi
OUTPUT_DIR="${OUTPUT_DIR:-${SCRIPT_DIR}/dist}"
PROJECT_NAME="linchange"
ARCH="$(detect_arch)"
OUTPUT_DEB="${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"

# Layer caching makes the second build take a minute rather than ten; NO_CACHE
# is the escape hatch for "prove it still builds with a cold cache".
NO_CACHE_FLAG=()
[ -n "${NO_CACHE:-}" ] && NO_CACHE_FLAG=(--no-cache)

mkdir -p "${OUTPUT_DIR}"
OUTPUT_DIR="$(cd "${OUTPUT_DIR}" && pwd)"

echo "Building ${PROJECT_NAME}_${VERSION}_${ARCH}.deb using Docker"

# Build Docker image
echo "Building Docker build image..."
docker build \
    "${NO_CACHE_FLAG[@]+"${NO_CACHE_FLAG[@]}"}" \
    --target packager \
    --build-arg VERSION="${VERSION}" \
    --build-arg ARCH="${ARCH}" \
    -f "${SCRIPT_DIR}/Dockerfile.build" \
    -t "${PROJECT_NAME}-builder:${VERSION}-packager" \
    "${SCRIPT_DIR}"

# Run container to build package
echo "Running build container..."
# The copy runs as the invoking user so the .deb left in ./dist is not owned
# by root, which would make the next build fail on an unwritable directory.
docker run --rm \
    --user "$(id -u):$(id -g)" \
    -v "${OUTPUT_DIR}:/output" \
    "${PROJECT_NAME}-builder:${VERSION}-packager" \
    cp /package.deb "/output/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"

[ -f "${OUTPUT_DEB}" ] || die "the container did not produce ${OUTPUT_DEB}"

echo ""
echo "Package created: ${OUTPUT_DEB} ($(du -h "${OUTPUT_DEB}" | cut -f1))"
echo ""
echo "Inspect with:   dpkg-deb --info ${OUTPUT_DEB}"
echo "Install with:   sudo dpkg -i ${OUTPUT_DEB}"
echo "Then configure: linchange setup --config /etc/linchange/linchange.yaml"
echo "Start with:     sudo systemctl start linchange"
