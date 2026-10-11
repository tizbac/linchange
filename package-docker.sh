#!/bin/bash
# linchange Docker-based .deb package builder
# Usage: ./package-docker.sh [version] [--output-dir DIR]

set -euo pipefail

# Paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Defaults
GIT_VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo 'dev')"
VERSION="${1:-$(echo "${GIT_VERSION}" | sed 's/^[^0-9]*//;s/-dirty$/.dirty/;s/-/./g')}"
OUTPUT_DIR="${2:-${SCRIPT_DIR}/dist}"
PROJECT_NAME="linchange"
ARCH=$(docker info --format '{{.Architecture}}' 2>/dev/null | sed 's/x86_64/amd64/;s/aarch64/arm64/;s/armv7l/armhf/' || uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/;s/armv7l/armhf/')

mkdir -p "${OUTPUT_DIR}"
BUILD_DIR="${OUTPUT_DIR}/build"
OUTPUT_DEB="${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"

echo "Building ${PROJECT_NAME}_${VERSION}_${ARCH}.deb using Docker"

# Build Docker image
echo "Building Docker build image..."
docker build \
    --build-arg VERSION="${VERSION}" \
    --build-arg ARCH="${ARCH}" \
    -f "${SCRIPT_DIR}/Dockerfile.build" \
    -t "linchange-builder:${VERSION}" \
    "${SCRIPT_DIR}"

# Run container to build package
echo "Running build container..."
docker run --rm \
    -v "${OUTPUT_DIR}:/output" \
    "linchange-builder:${VERSION}" \
    cp /package.deb "/output/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"

echo "Package created: ${OUTPUT_DEB}"
echo ""
echo "Install with: sudo dpkg -i ${OUTPUT_DEB}"
echo "Then configure: sudo editor /etc/linchange/linchange.yaml"
echo "Start with: sudo systemctl start linchange"