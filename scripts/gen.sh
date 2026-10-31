#!/usr/bin/env bash
# Everything generated, after a change to the schema (proto/) or to the API
# (api/levyan/v1/*.proto): `go tool pd gen .`, and then the API's Go.
#
# In that order, and both: `pd gen` sweeps away generated Go it did not write
# anywhere in the tree, and does not stop at the api module's go.mod -- so it
# takes the API's with it, and `scripts/api.sh` writes it again. A build after
# `pd gen` alone fails to compile `server/api`, which is how it is noticed.
set -euo pipefail
cd "$(dirname "$0")/.."

go tool pd gen .
scripts/api.sh
