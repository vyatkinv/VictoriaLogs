#!/bin/sh
set -e

echo "Waiting for Vault to become ready..."
until vault status 2>/dev/null | grep -q "Initialized.*true"; do
    sleep 1
done
echo "Vault is ready."

# Enable PKI secrets engine
vault secrets enable pki || echo "PKI already enabled"
vault secrets tune -max-lease-ttl=1h pki

# Generate root CA — max TTL 1 h for demo purposes
vault write -field=certificate pki/root/generate/internal \
    common_name="VictoriaLogs Demo CA" \
    ttl=1h

# Configure CRL and issuer URLs
vault write pki/config/urls \
    issuing_certificates="http://vault:8200/v1/pki/ca" \
    crl_distribution_points="http://vault:8200/v1/pki/crl"

# Create a role with a short max_ttl so hot-reload can be demonstrated quickly.
# cert_format=pem_bundle is NOT needed — the API returns leaf + key separately.
# client_flag=false: the issued certificate is a server certificate only, so a
# stolen key cannot be used to authenticate as this service elsewhere.
vault write pki/roles/victoria-logs \
    allow_bare_domains=true \
    allow_subdomains=true \
    allow_ip_sans=true \
    allowed_domains="localhost,victoria-logs" \
    client_flag=false \
    server_flag=true \
    max_ttl="5m" \
    ttl="2m" \
    key_type="ec" \
    key_bits=256

# ---------------------------------------------------------------------------
# AppRole auth: victoria-logs authenticates itself instead of carrying a static
# token that somebody has to deliver and rotate.

vault auth enable approle || echo "approle already enabled"

# Least privilege: issue certificates for exactly one PKI role, and revoke the
# ones we issued (needed only for -tls.vaultRevokeOnShutdown).
cat > /tmp/victoria-logs-policy.hcl <<'EOF'
path "pki/issue/victoria-logs" {
  capabilities = ["update"]
}

path "pki/revoke" {
  capabilities = ["update"]
}
EOF
vault policy write victoria-logs /tmp/victoria-logs-policy.hcl

# token_ttl is deliberately shorter than the certificate TTL: the second
# issuance (at ~80 s) then has to log in again, so the demo exercises the auth
# path and not just the first login.
vault write auth/approle/role/victoria-logs \
    token_policies="victoria-logs" \
    token_type=service \
    token_ttl=1m \
    token_max_ttl=5m \
    secret_id_ttl=0

# Deliver role_id/secret_id through a shared volume with owner-only permissions.
# In production prefer a response-wrapped secret_id — see SECURITY_GUIDE.md §15.
umask 077
mkdir -p /creds
vault read -field=role_id auth/approle/role/victoria-logs/role-id > /creds/role_id
vault write -f -field=secret_id auth/approle/role/victoria-logs/secret-id > /creds/secret_id
chmod 600 /creds/role_id /creds/secret_id

echo "Vault PKI setup complete."
echo "  PKI role  : victoria-logs"
echo "  Max TTL   : 5m"
echo "  Default   : 2m"
echo "  Renewal   : automatic at 2/3 of lifetime (~80s)"
echo "  Auth      : approle (token_ttl=1m, so renewal re-logins)"
echo "  Creds     : /creds/role_id, /creds/secret_id (mode 600)"
