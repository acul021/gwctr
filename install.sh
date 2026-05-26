#!/bin/sh
set -e

mkdir -p /run/docker/plugins
docker compose build
docker compose up -d
echo "plugin running; socket: /run/docker/plugins/gwbridge.sock"
