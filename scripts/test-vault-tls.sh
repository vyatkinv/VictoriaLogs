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
COMPOSE="docker compose -f docker-compose.vault.yml"
VAULT_EXEC="$COMPOSE exec -T -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=root vault vault"

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
step "2b. Check AppRole authentication and credential hygiene"
LOGS=$($COMPOSE logs victoria-logs 2>/dev/null)
if echo "$LOGS" | grep -q 'authenticated in vault via the "approle" auth method'; then
    echo "  PASS: authenticated via approle."
else
    echo "FAIL: no approle login found in the logs."
    exit 1
fi

# Credentials must never reach the command line or the logs.
CMDLINE=$($COMPOSE exec -T victoria-logs cat /proc/1/cmdline | tr '\0' ' ')
if echo "$CMDLINE" | grep -qE 'vaultToken=|vaultAuthSecretID='; then
    echo "FAIL: a credential is visible in /proc/1/cmdline: $CMDLINE"
    exit 1
fi
echo "  PASS: no credential in argv."

SECRET_ID=$($COMPOSE exec -T victoria-logs cat /creds/secret_id)
if echo "$LOGS" | grep -qF "$SECRET_ID"; then
    echo "FAIL: the secret_id leaked into the logs."
    exit 1
fi
echo "  PASS: no credential in the logs."

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

# ---------------------------------------------------------------------------
step "8. Verify the renewal re-authenticated in Vault"
# token_ttl=1m is shorter than the 2m certificate, so the second issuance must
# have gone through a fresh AppRole login rather than a cached token.
LOGS=$($COMPOSE logs victoria-logs 2>/dev/null)
LOGIN_COUNT=$(echo "$LOGS" | grep -c 'authenticated in vault via the "approle" auth method' || true)
ISSUE_COUNT=$(echo "$LOGS" | grep -c 'issued certificate from' || true)
echo "  approle logins   : $LOGIN_COUNT"
echo "  certificates     : $ISSUE_COUNT"
if [ "$LOGIN_COUNT" -lt 2 ] || [ "$ISSUE_COUNT" -lt 2 ]; then
    echo "FAIL: expected at least 2 logins and 2 issuances."
    exit 1
fi
echo "  PASS: renewal re-authenticated."

# ---------------------------------------------------------------------------
step "9. vlagent: TLS from Vault on both the server and the client side"
AGENT_ADDR="https://localhost:9429"
until $CURL "$AGENT_ADDR/health" | grep -q OK; do
    echo "  waiting for vlagent..."
    sleep 3
done

AGENT_LOGS=$($COMPOSE logs vlagent 2>/dev/null)
if ! echo "$AGENT_LOGS" | grep -q 'authenticated in vault via the "approle" auth method'; then
    echo "FAIL: vlagent did not authenticate via approle."
    exit 1
fi
if ! echo "$AGENT_LOGS" | grep -q 'loaded the CA of the .* pki mount'; then
    echo "FAIL: vlagent did not load the PKI CA (-tls.vaultTrustPKICA)."
    exit 1
fi
echo "  PASS: vlagent authenticated and loaded the PKI CA."

# The vlagent listener itself must serve a Vault-issued certificate.
AGENT_ISSUER=$(echo | openssl s_client -connect localhost:9429 -servername localhost 2>/dev/null \
    | openssl x509 -noout -issuer 2>/dev/null)
echo "  vlagent cert issuer: $AGENT_ISSUER"
if ! echo "$AGENT_ISSUER" | grep -q "VictoriaLogs Demo CA"; then
    echo "FAIL: vlagent is not serving a Vault-issued certificate."
    exit 1
fi

# Ingest through vlagent. Reaching victoria-logs requires vlagent to verify the
# server against the CA fetched from Vault, with no CA file configured.
AGENT_MSG="vlagent vault mtls test $(date +%s)"
$CURL -X POST "$AGENT_ADDR/insert/jsonline" \
    -H "Content-Type: application/stream+json" \
    -d "{\"_time\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"_msg\":\"$AGENT_MSG\",\"service\":\"vlagent-test\",\"_stream_fields\":\"service\"}" \
    | head -c 200

FOUND=""
for _ in $(seq 1 15); do
    sleep 2
    if $CURL "$VL_ADDR/select/logsql/query?query=service%3Avlagent-test&limit=5" | grep -qF "$AGENT_MSG"; then
        FOUND=yes
        break
    fi
done
if [ -z "$FOUND" ]; then
    echo "FAIL: the log line sent through vlagent never reached victoria-logs."
    $COMPOSE logs --tail=30 vlagent
    exit 1
fi
echo "  PASS: vlagent forwarded logs to victoria-logs over Vault-issued TLS."

# ---------------------------------------------------------------------------
step "10. Verify the certificate is revoked on shutdown"
SERIAL=$(echo "$LOGS" | grep -o 'serial [0-9a-f:]\{10,\}' | tail -1 | awk '{print $2}')
echo "  current serial: $SERIAL"
$COMPOSE stop victoria-logs >/dev/null
sleep 2
REVOCATION_TIME=$($VAULT_EXEC read -field=revocation_time "pki/cert/$SERIAL" 2>/dev/null || echo 0)
echo "  revocation_time: $REVOCATION_TIME"
if [ "$REVOCATION_TIME" = "0" ] || [ -z "$REVOCATION_TIME" ]; then
    echo "FAIL: the certificate was not revoked on shutdown."
    exit 1
fi
echo "  PASS: certificate revoked (-tls.vaultRevokeOnShutdown)."

echo
echo "PASS: all checks completed successfully."
echo "Note: victoria-logs was stopped by step 10; run '$COMPOSE up -d' to restart it."
