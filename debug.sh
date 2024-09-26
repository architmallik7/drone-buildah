#!/bin/sh
set -x

echo "Debugging information:"
id
ls -la /home/build/.local/share/containers
ls -la /home/build/.config/containers
cat /home/build/.config/containers/storage.conf
env | grep BUILDAH
env | grep STORAGE
buildah version

echo "Starting drone-docker..."