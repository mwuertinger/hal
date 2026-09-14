#!/bin/sh

set -e

go vet ./...
go test ./...

# The binary that ships is the arm one, so type-check that target too: a
# 32-bit-only compile error would otherwise pass every check on an amd64
# developer machine and CI, and only surface here.
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go vet ./...

for arch in arm amd64
do
  echo "Building $arch"
  # GOARM=6 matches the arm32v6 base image this used to ship as, and keeps the
  # binary runnable on ARMv6 hardware (Pi Zero / Pi 1). Go defaults to GOARM=7,
  # which would SIGILL there.
  #
  # -trimpath so the build does not depend on where the source happens to sit:
  # without it the builder's absolute paths are baked in and two people building
  # the same commit get different binaries. -s -w drops the symbol table and
  # DWARF, which is a third of the size of a file that lives on an SD card.
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOARM=6 \
    go build -trimpath -ldflags="-s -w" -o hal-$arch
done
