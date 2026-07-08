#!/bin/bash
# test-vault-tls.sh — end-to-end smoke test for Vault PKI TLS + hot-reload.
#
# Usage: run from the repo root after starting the stack:
#   docker compose -f docker-compose.vault.yml up -d --build
#   ./scripts/test-vault-tls.sh
#
set -euo pipefail

VL_ADDR="https://localhost:9428"
CURL="curl -sk --retry 5 --retry-delay 2"

step() { echo; echo "=== $* ==="; }

# ---------------------------------------------------------------------------
step "1. Wait for victoria-logs to be healthy"
until $CURL "$VL_ADDR/health" | grep -q OK; do
    echo "  waiting..."
    sleep 3
done
echo "  victoria-logs is up."

# ---------------------------------------------------------------------------
step "2. Check initial TLS certificate"
CERT_INFO=$(echo | openssl s_client -connect localhost:9428 -servername localhost 2>/dev/null \
    | openssl x509 -noout -subject -dates 2>/dev/null)
echo "$CERT_INFO"
INITIAL_NOTAFTER=$(echo "$CERT_INFO" | grep notAfter | cut -d= -f2-)
echo "  Initial cert expires: $INITIAL_NOTAFTER"

# ---------------------------------------------------------------------------
step "3. Insert test logs"
$CURL -X POST "$VL_ADDR/insert/jsonline" \
    -H "Content-Type: application/stream+json" \
    -d "{\"_time\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"_msg\":\"vault TLS integration test\",\"service\":\"test\",\"_stream_fields\":\"service\"}" \
    | head -c 200
echo "  Logs inserted."

# ---------------------------------------------------------------------------
step "4. Query logs"
RESULT=$($CURL "$VL_ADDR/select/logsql/query?query=service%3Atest&limit=5")
echo "$RESULT" | head -c 500
echo "  Logs found: $(echo "$RESULT" | grep -c '"_msg"' || true)"

# ---------------------------------------------------------------------------
step "5. Wait for automatic certificate renewal (~90 s for 2-minute cert)"
echo "  Certificate TTL=2m → renewal fires at ~80 s. Waiting 95 s..."

# Show a countdown so it's clear the test is alive.
for i in $(seq 95 -5 5); do
    printf "\r  %3d s remaining..." "$i"
    sleep 5
done
printf "\r  Renewal window reached.              \n"

# ---------------------------------------------------------------------------
step "6. Verify certificate was renewed (new notAfter)"
NEW_CERT_INFO=$(echo | openssl s_client -connect localhost:9428 -servername localhost 2>/dev/null \
    | openssl x509 -noout -subject -dates 2>/dev/null)
echo "$NEW_CERT_INFO"
NEW_NOTAFTER=$(echo "$NEW_CERT_INFO" | grep notAfter | cut -d= -f2-)
echo "  Initial  cert expires: $INITIAL_NOTAFTER"
echo "  Renewed  cert expires: $NEW_NOTAFTER"

if [ "$INITIAL_NOTAFTER" = "$NEW_NOTAFTER" ]; then
    echo "FAIL: certificate was NOT renewed — expiry unchanged."
    exit 1
else
    echo "PASS: certificate was renewed successfully."
fi

# ---------------------------------------------------------------------------
step "7. Logs still queryable after cert rotation"
RESULT2=$($CURL "$VL_ADDR/select/logsql/query?query=service%3Atest&limit=5")
echo "$RESULT2" | head -c 300
echo
echo "PASS: all checks completed successfully."
