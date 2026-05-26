#!/bin/sh
# Build and create the managed-plugin form of gwctr.
#
# Usage: scripts/build-plugin.sh [tag]
#   tag defaults to acul21/gwctr:latest
#
# After this runs:
#   docker plugin ls
#   docker plugin enable <tag>
#   docker network create -d <tag> --subnet ... mynet

set -e

TAG="${1:-acul21/gwctr:latest}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
PLUGIN_DIR="$HERE/plugin"
ROOTFS="$PLUGIN_DIR/rootfs"

echo "==> building builder image"
docker build --network=host -t gwctr:plugin-builder -f "$HERE/Dockerfile" "$HERE"

echo "==> exporting rootfs into $ROOTFS"
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS"
cid="$(docker create gwctr:plugin-builder true)"
docker export "$cid" | tar -x -C "$ROOTFS"
docker rm "$cid" >/dev/null

echo "==> creating managed plugin $TAG"
docker plugin rm -f "$TAG" 2>/dev/null || true
docker plugin create "$TAG" "$PLUGIN_DIR"

echo
echo "managed plugin created: $TAG"
echo "next:"
echo "  docker plugin enable $TAG"
echo "  docker network create -d $TAG --subnet 10.99.0.0/24 \\"
echo "    -o de.acul21.gwctr.gateway_ip=10.99.0.2 gwnet"
