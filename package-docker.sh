#!/bin/bash
# linchange Docker-based .deb package builder
# Usage: ./package-docker.sh [version] [--output-dir DIR]

set -euo pipefail

# Defaults
VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo 'dev')}"
OUTPUT_DIR="${2:-/tmp/linchange-pkg}"
PROJECT_NAME="linchange"
ARCH=$(dpkg --print-architecture)

# Paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR="${OUTPUT_DIR}/build"
OUTPUT_DEB="${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"

echo "Building ${PROJECT_NAME}_${VERSION}_${ARCH}.deb using Docker"

# Build Docker image
echo "Building Docker build image..."
docker build \
    --build-arg VERSION="${VERSION}" \
    --build-arg ARCH="$(dpkg --print-architecture)" \
    -f "${SCRIPT_DIR}/Dockerfile.build" \
    -t "linchange-builder:${VERSION}" \
    "${SCRIPT_DIR}"

# Run container to build package
echo "Running build container..."
docker run --rm \
    -v "${OUTPUT_DIR}:/output" \
    "linchange-builder:${VERSION}" \
    cp -r /package /output/

# Move .deb to output location
mv "${OUTPUT_DIR}/package/DEBIAN/../${PROJECT_NAME}_${VERSION}_${ARCH}.deb" "${OUTPUT_DEB}" 2>/dev/null || true

# Also try alternative location
if [ ! -f "${OUTPUT_DEB}" ]; then
    find "${OUTPUT_DIR}" -name "*.deb" -exec mv {} "${OUTPUT_DEB}" \;
fi

echo "Package created: ${OUTPUT_DEB}"
echo ""
echo "Install with: sudo dpkg -i ${OUTPUT_DEB}"
echo "Then configure: sudo editor /etc/linchange/linchange.yaml"
echo "Start with: sudo systemctl start linchange"