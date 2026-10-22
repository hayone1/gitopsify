#!/bin/sh
# Writes shell completion scripts to completions/ for release archives and
# packages.
set -eu
rm -rf completions
mkdir completions
for sh in bash zsh fish; do
  go run ./cmd/gitopsify completion "$sh" > "completions/gitopsify.$sh"
done
go run ./cmd/gitopsify completion powershell > completions/gitopsify.ps1
