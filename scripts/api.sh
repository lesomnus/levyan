#!/usr/bin/env bash
# The API an app calls levyan with (api/): its Go generated from its .proto by
# the generators this module pins, and held to what was promised.
#
#   scripts/api.sh                 lint and generate
#   scripts/api.sh --check         and fail if anything moved, or if the api
#                                  module requires anything of levyan's or of
#                                  payday's -- what CI runs
#   scripts/api.sh --breaking REF  fail if the API breaks what it was at REF
#                                  (a commit, a branch); nothing to break when
#                                  REF has no API
#
# buf is $PD_BUF, or the one `go tool pd gen` fetched and checked against its
# release when it last ran, so that one buf, the one payday pins, generates
# both halves: run `go tool pd gen .` once first.
set -euo pipefail
cd "$(dirname "$0")/.."

buf="${PD_BUF:-}"
if [ -z "${buf}" ]; then
	cache="${XDG_CACHE_HOME:-${HOME}/.cache}/payday/buf"
	buf="$(ls -d "${cache}"/*/buf 2>/dev/null | sort -V | tail -n 1 || true)"
fi
if [ -z "${buf}" ] || [ ! -x "${buf}" ]; then
	echo "scripts/api.sh: no buf; run \`go tool pd gen .\` once, which fetches the one payday pins, or name one in PD_BUF" >&2
	exit 1
fi

if [ "${1:-}" = "--breaking" ]; then
	ref="${2:?scripts/api.sh --breaking REF}"
	if ! git cat-file -e "${ref}:api/buf.yaml" 2>/dev/null; then
		echo "scripts/api.sh: ${ref} has no API, so there is nothing for this one to break"
		exit 0
	fi
	was="$(mktemp -d)"
	trap 'rm -rf "${was}"' EXIT
	git archive "${ref}" api | tar -x -C "${was}"
	"${buf}" breaking api --against "${was}/api"
	exit 0
fi

"${buf}" lint api
"${buf}" generate --template api/buf.gen.yaml
(cd api && go mod tidy)

if [ "${1:-}" != "--check" ]; then
	exit 0
fi

if [ -n "$(git status --porcelain -- api)" ]; then
	git status --short -- api
	git diff -- api
	echo "scripts/api.sh: the API's generated Go is not what its .proto says; run scripts/api.sh and commit what it wrote" >&2
	exit 1
fi

# What an app that requires the api module is made to require with it.
if (cd api && go list -m all) | grep -E '^github\.com/lesomnus/(payday|levyan )'; then
	echo "scripts/api.sh: the api module requires the above, which every app calling levyan would then require too" >&2
	exit 1
fi
