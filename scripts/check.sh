#!/bin/sh
# Runs what CI runs, for every module: gofmt, go mod tidy -diff, vet,
# tests with the race detector. Usage: scripts/check.sh [module ...]
set -eu

cd "$(dirname "$0")/.."

# Dependency order: core first.
modules="core mail mongo natsx objectstore observability web"
if [ "$#" -gt 0 ]; then
	modules="$*"
fi

for m in $modules; do
	echo "== $m"
	unformatted="$(gofmt -l "$m")"
	if [ -n "$unformatted" ]; then
		echo "gofmt needed:"
		echo "$unformatted"
		exit 1
	fi
	(
		cd "$m"
		go mod tidy -diff
		go vet ./...
		go test -race -count=1 ./...
	)
done
echo "all modules OK"
