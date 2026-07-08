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
vault write pki/roles/victoria-logs \
    allow_bare_domains=true \
    allow_subdomains=true \
    allow_ip_sans=true \
    allowed_domains="localhost,victoria-logs" \
    max_ttl="5m" \
    ttl="2m" \
    key_type="ec" \
    key_bits=256

echo "Vault PKI setup complete."
echo "  Role      : victoria-logs"
echo "  Max TTL   : 5m"
echo "  Default   : 2m"
echo "  Renewal   : automatic at 2/3 of lifetime (~80s)"
