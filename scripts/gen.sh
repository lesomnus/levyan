#!/usr/bin/env bash
# Everything generated, after a change to the schema (proto/) or to the API
# (api/levyan/v1/*.proto): `go tool pd gen .`, and then the API's Go.
#
# Two generations, because the API is a module of its own (api/go.mod) that
# `pd gen` leaves alone (payday#189). The API's second only because
# `scripts/api.sh` runs the buf that `pd gen` fetched.
set -euo pipefail
cd "$(dirname "$0")/.."

go tool pd gen .
scripts/api.sh
