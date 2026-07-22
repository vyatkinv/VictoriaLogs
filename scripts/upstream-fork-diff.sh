#!/usr/bin/env bash
#
# Show what changed upstream in the packages forked into lib/.
#
# lib/httpserver, lib/writeconcurrencylimiter and lib/protoparser/protoparserutil
# are verbatim forks of github.com/VictoriaMetrics/VictoriaMetrics packages.
# See lib/httpserver/UPSTREAM.md for why.
#
# Run this before every `make vendor-update`. It compares three things:
#
#   1. the fork baseline recorded in UPSTREAM.md against the fork on disk
#      -> shows the local VL-FORK changes;
#   2. the fork baseline against the version currently required by go.mod
#      -> shows what upstream changed since the fork and must be reconciled.
#
# Requires network access on first run (fetches the module into GOMODCACHE).
# Deliberately NOT wired into `make check-all`: CI builds run air-gapped in
# vendor mode.

set -euo pipefail

cd "$(dirname "$0")/.."

UPSTREAM_MD="lib/httpserver/UPSTREAM.md"
MODULE="github.com/VictoriaMetrics/VictoriaMetrics"

# Packages forked into lib/, as "<upstream path>:<local path>".
PACKAGES=(
	"lib/httpserver:lib/httpserver"
	"lib/writeconcurrencylimiter:lib/writeconcurrencylimiter"
	"lib/protoparser/protoparserutil:lib/protoparser/protoparserutil"
)

if [ ! -f "$UPSTREAM_MD" ]; then
	echo "ERROR: $UPSTREAM_MD not found" >&2
	exit 1
fi

BASELINE_VERSION="$(grep -oP '^Version: `\K[^`]+' "$UPSTREAM_MD")"
CURRENT_VERSION="$(go list -m -f '{{.Version}}' "$MODULE")"

echo "fork baseline : $BASELINE_VERSION"
echo "go.mod        : $CURRENT_VERSION"
echo

# Module queries by explicit version need -mod=mod: the repo vendors its deps,
# and vendor mode refuses to resolve anything outside vendor/.
fetch() { # $1 = version -> prints the module dir
	local v="$1"
	GOFLAGS=-mod=mod go mod download "$MODULE@$v" >/dev/null 2>&1 || {
		echo "ERROR: cannot download $MODULE@$v (network required)" >&2
		exit 1
	}
	GOFLAGS=-mod=mod go list -m -f '{{.Dir}}' "$MODULE@$v"
}

# --- 1. local changes against the recorded baseline -------------------------

echo "=== local changes (VL-FORK) ==============================================="
baseline_dir="$(fetch "$BASELINE_VERSION")"
local_changed=0
for pkg in "${PACKAGES[@]}"; do
	up="${pkg%%:*}"
	loc="${pkg##*:}"
	for f in "$baseline_dir/$up"/*; do
		name="$(basename "$f")"
		[ -f "$loc/$name" ] || { echo "REMOVED IN FORK: $loc/$name"; local_changed=1; continue; }
		if [ "${name##*.}" != "go" ]; then
			# Non-Go assets (favicon.ico, *.qtpl) carry no provenance header.
			cmp -s "$f" "$loc/$name" || { echo "CHANGED: $loc/$name"; local_changed=1; }
			continue
		fi
		# Strip the provenance header the fork prepends to every .go file.
		stripped="$(grep -v '^// Forked from github.com/VictoriaMetrics/VictoriaMetrics \|^// See lib/httpserver/UPSTREAM.md' "$loc/$name" | sed '1{/^$/d}')"
		if ! diff -u "$f" <(printf '%s\n' "$stripped") >/dev/null 2>&1; then
			echo "--- $loc/$name"
			diff -u "$f" <(printf '%s\n' "$stripped") || true
			local_changed=1
		fi
	done
done
[ "$local_changed" -eq 0 ] && echo "(none — fork is byte-identical to $BASELINE_VERSION)"
echo

# --- 2. upstream changes since the fork baseline ---------------------------

echo "=== upstream changes to reconcile ========================================="
if [ "$BASELINE_VERSION" = "$CURRENT_VERSION" ]; then
	echo "(none — go.mod still points at the fork baseline)"
	exit 0
fi

current_dir="$(fetch "$CURRENT_VERSION")"
upstream_changed=0
for pkg in "${PACKAGES[@]}"; do
	up="${pkg%%:*}"
	loc="${pkg##*:}"
	if ! diff -rq "$baseline_dir/$up" "$current_dir/$up" >/dev/null 2>&1; then
		echo "--- $loc  ($BASELINE_VERSION -> $CURRENT_VERSION)"
		diff -ru "$baseline_dir/$up" "$current_dir/$up" || true
		upstream_changed=1
	fi
done

if [ "$upstream_changed" -eq 0 ]; then
	echo "(none — upstream did not touch the forked packages)"
	echo
	echo "Bump the Version line in $UPSTREAM_MD to $CURRENT_VERSION."
else
	echo
	echo "Apply the diffs above to lib/, keeping the VL-FORK changes, then refresh"
	echo "the Version line and the sha256 list in $UPSTREAM_MD."
fi
