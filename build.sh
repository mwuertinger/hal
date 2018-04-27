#!/bin/sh

set -e

go test ./...

for arch in arm amd64
do
  echo "Building $arch"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o hau-$arch
done

docker build .