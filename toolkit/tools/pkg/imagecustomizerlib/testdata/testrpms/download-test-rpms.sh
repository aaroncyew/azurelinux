#!/usr/bin/env bash
set -eu

while getopts "t:" flag
do
    case "${flag}" in
        t) AZURE_LINUX_VERSION="$OPTARG";;
        h) ;;&
        ?)
            echo "Usage: download-test-rpms.sh [-t IMAGE_VERSION]"
            echo ""
            echo "Args:"
            echo "  -t MARINER_IMAGE_VERSION   The Azure Image version to download the RPMs for."
            echo "  -h Show help"
            exit 1;;
    esac
done

SCRIPT_DIR=$(dirname "${BASH_SOURCE[0]}")
CONTAINER_TAG="imagecustomizertestrpms:latest"
DOCKERFILE_DIR="$SCRIPT_DIR/downloader"

AZURELINUX_2_CONTAINER_IMAGE="mcr.microsoft.com/cbl-mariner/base/core:2.0"

if [ -z "$AZURE_LINUX_VERSION" ]; then
  AZURE_LINUX_VERSION="2.0"
fi

case "${AZURE_LINUX_VERSION}" in
  2.0)
    CONTAINER_IMAGE="$AZURELINUX_2_CONTAINER_IMAGE"
    ;;
  *)
    
    echo "error: unsupported Azure Linux version: $AZURE_LINUX_VERSION"
    exit 1;;
esac

set -x

OUT_DIR="$SCRIPT_DIR/downloadedrpms/$AZURE_LINUX_VERSION"
mkdir -p "$OUT_DIR"

# Build a container image that contains the RPMs.
docker build \
  --build-arg "baseimage=$AZURELINUX_2_CONTAINER_IMAGE" \
  --tag "$CONTAINER_TAG" \
  "$DOCKERFILE_DIR"

# Extract the RPM files.
docker run \
  --rm \
   -v $OUT_DIR:/outdir:z \
   "$CONTAINER_TAG" \
   cp -r /downloadedrpms/. "/outdir"
