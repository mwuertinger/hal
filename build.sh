#!/bin/sh

set -e

go vet ./...
go test ./...

for arch in arm amd64
do
  echo "Building $arch"
  # GOARM=6 matches the arm32v6 base image this used to ship as, and keeps the
  # binary runnable on ARMv6 hardware (Pi Zero / Pi 1). Go defaults to GOARM=7,
  # which would SIGILL there.
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOARM=6 go build -o hal-$arch
done
