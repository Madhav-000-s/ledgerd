#!/usr/bin/env bash
# Measure combined unit + integration coverage and enforce the gate.
#
# The gate has to span both suites. Large parts of internal/postgres are only reachable
# with a real database, so a unit-only number understates the project by roughly twenty
# points — and would create pressure to mock the database to make the number go up, which
# is exactly what this codebase refuses to do.
#
# Usage: scripts/coverage.sh [minimum-percent]
set -euo pipefail

MIN="${1:-85}"
PKGS="./internal/...,./pkg/..."

echo "==> unit coverage"
go test -count=1 -coverprofile=unit.out -coverpkg="$PKGS" ./internal/... ./pkg/...

echo "==> integration coverage (needs Docker)"
go test -count=1 -tags=integration -timeout=30m \
    -coverprofile=integration.out -coverpkg="$PKGS" ./test/integration/...

echo "==> merging"
python scripts/mergecov.py unit.out integration.out merged.out

# sqlc's output is excluded: it is machine-written, and covering it would measure the
# generator rather than this codebase.
grep -v '/internal/postgres/sqlc/' merged.out > gate.out

go tool cover -func=gate.out | tail -1

total=$(go tool cover -func=gate.out | tail -1 | awk '{print substr($3, 1, length($3)-1)}')
echo "==> combined coverage: ${total}% (gate: ${MIN}%)"

if awk "BEGIN {exit !($total < $MIN)}"; then
    echo "FAIL: coverage ${total}% is below the ${MIN}% gate" >&2
    exit 1
fi
echo "OK"
