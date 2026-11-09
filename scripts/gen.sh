#!/usr/bin/env bash
# Everything generated, after a change to the schema (proto/) or to the API
# (api/levyan/v1/*.proto): `go tool pd gen .`, and then the API's Go.
#
# Two generations, because the API is a module of its own (api/go.mod) that
# `pd gen` leaves alone (payday#189). Both run the buf payday pins:
# `scripts/api.sh` asks for it with `go tool pd buf`.
set -euo pipefail
cd "$(dirname "$0")/.."

go tool pd gen .
scripts/api.sh
