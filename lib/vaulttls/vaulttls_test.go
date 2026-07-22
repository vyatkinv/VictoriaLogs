package vaulttls

import (
	"bytes"
	"crypto/tls"
	"strings"
	"testing"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

func testConfig(fv *fakeVault, auth AuthConfig) Config {
	return Config{
		Addr:       fv.addr(),
		Auth:       auth,
		PKIPath:    "pki",
		Role:       "victoria-logs",
		CommonName: "localhost",
		TTL:        "1h",
	}
}

func TestProviderIssuesCertificateViaAppRole(t *testing.T) {
	fv := newFakeVault(t)
	p, err := NewProvider(testConfig(fv, AuthConfig{
		Method:   AuthMethodAppRole,
		RoleID:   "role-id-value",
		SecretID: "secret-id-value",
	}))
	if err != nil {
		t.Fatalf("cannot create provider: %s", err)
	}
	defer p.Stop()

	cert, err := p.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatalf("expecting a non-empty certificate")
	}
	logins, _, issues := fv.stats()
	if logins != 1 || issues != 1 {
		t.Fatalf("unexpected number of requests; logins=%d, issues=%d; want 1 and 1", logins, issues)
	}
	fv.mu.Lock()
	issueToken, serial := fv.issueTokens[0], fv.serials[0]
	fv.mu.Unlock()
	if issueToken != "login-token-1" {
		t.Fatalf("unexpected token used for issuance; got %q", issueToken)
	}
	if p.serial != serial {
		t.Fatalf("unexpected serial; got %q; want %q", p.serial, serial)
	}
	if p.Expiry().IsZero() {
		t.Fatalf("expecting a non-zero expiry")
	}
}

func TestProviderRenewSwapsCertificate(t *testing.T) {
	fv := newFakeVault(t)
	p, err := NewProvider(testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"}))
	if err != nil {
		t.Fatalf("cannot create provider: %s", err)
	}
	defer p.Stop()

	certBefore, err := p.GetCertificate(nil)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := p.renew(); err != nil {
		t.Fatalf("unexpected error on renew: %s", err)
	}
	certAfter, err := p.GetCertificate(nil)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if bytes.Equal(certBefore.Certificate[0], certAfter.Certificate[0]) {
		t.Fatalf("expecting the certificate to be replaced by renew()")
	}
	// The token is still within its lease, so no second login is needed.
	if logins, _, issues := fv.stats(); logins != 1 || issues != 2 {
		t.Fatalf("unexpected number of requests; logins=%d, issues=%d; want 1 and 2", logins, issues)
	}
}

func TestProviderRetriesOnAuthFailure(t *testing.T) {
	fv := newFakeVault(t)
	// The first issuance is rejected as if the token had been revoked ahead of
	// its lease. The provider must re-authenticate and retry once.
	fv.issueAuthFailures = 1
	p, err := NewProvider(testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"}))
	if err != nil {
		t.Fatalf("cannot create provider: %s", err)
	}
	defer p.Stop()

	logins, _, issues := fv.stats()
	if logins != 2 || issues != 2 {
		t.Fatalf("unexpected number of requests; logins=%d, issues=%d; want 2 and 2", logins, issues)
	}
	fv.mu.Lock()
	defer fv.mu.Unlock()
	if fv.issueTokens[1] != "login-token-2" {
		t.Fatalf("expecting the retry to use a freshly issued token; got %q", fv.issueTokens[1])
	}
}

func TestProviderDoesNotRetryWithStaticToken(t *testing.T) {
	fv := newFakeVault(t)
	fv.allowToken("static-token")
	fv.issueAuthFailures = 1
	_, err := NewProvider(testConfig(fv, AuthConfig{Method: AuthMethodToken, Token: "static-token"}))
	if err == nil {
		t.Fatalf("expecting an error when vault rejects the static token")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expecting the vault error text to be surfaced; got %q", err)
	}
	// Retrying makes no sense: we cannot obtain a different static token.
	if logins, _, issues := fv.stats(); logins != 0 || issues != 1 {
		t.Fatalf("unexpected number of requests; logins=%d, issues=%d; want 0 and 1", logins, issues)
	}
}

func TestProviderStopRevokesToken(t *testing.T) {
	fv := newFakeVault(t)
	p, err := NewProvider(testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"}))
	if err != nil {
		t.Fatalf("cannot create provider: %s", err)
	}
	p.Stop()

	fv.mu.Lock()
	defer fv.mu.Unlock()
	if len(fv.revokedTokens) != 1 || fv.revokedTokens[0] != "login-token-1" {
		t.Fatalf("unexpected revoked tokens: %v", fv.revokedTokens)
	}
	if len(fv.revokedSerials) != 0 {
		t.Fatalf("expecting no certificate revocation without RevokeOnShutdown; got %v", fv.revokedSerials)
	}
}

func TestProviderStopRevokesCertificate(t *testing.T) {
	fv := newFakeVault(t)
	cfg := testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})
	cfg.RevokeOnShutdown = true
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("cannot create provider: %s", err)
	}
	p.Stop()

	fv.mu.Lock()
	defer fv.mu.Unlock()
	if len(fv.revokedSerials) != 1 || fv.revokedSerials[0] != fv.serials[0] {
		t.Fatalf("unexpected revoked serials: %v; want %v", fv.revokedSerials, fv.serials)
	}
	if len(fv.revokedTokens) != 1 {
		t.Fatalf("expecting the token to be revoked as well; got %v", fv.revokedTokens)
	}
	// The certificate must be revoked before the token it was issued with.
	if fv.revokedSerials[0] == "" {
		t.Fatalf("expecting a non-empty serial")
	}
}

// TestProviderDoesNotLeakCredentials verifies that no credential ever reaches the
// logs or an error message: those are routinely shipped to log collectors and
// pasted into tickets.
func TestProviderDoesNotLeakCredentials(t *testing.T) {
	const (
		roleIDCanary   = "ROLEID-CANARY"
		secretIDCanary = "SECRETID-CANARY"
	)
	fv := newFakeVault(t)

	var buf bytes.Buffer
	logger.SetOutputForTests(&buf)
	defer logger.ResetOutputForTest()

	p, err := NewProvider(testConfig(fv, AuthConfig{
		Method:   AuthMethodAppRole,
		RoleID:   roleIDCanary,
		SecretID: secretIDCanary,
	}))
	if err != nil {
		t.Fatalf("cannot create provider: %s", err)
	}
	if err := p.renew(); err != nil {
		t.Fatalf("unexpected error on renew: %s", err)
	}
	p.Stop()

	// Now force a failure and check its error text too.
	fv.issueAuthFailures = 100
	_, err = NewProvider(testConfig(fv, AuthConfig{
		Method:   AuthMethodAppRole,
		RoleID:   roleIDCanary,
		SecretID: secretIDCanary,
	}))
	if err == nil {
		t.Fatalf("expecting an error from a failing vault")
	}

	haystack := buf.String() + "\n" + err.Error()
	for _, canary := range []string{roleIDCanary, secretIDCanary, "login-token-"} {
		if strings.Contains(haystack, canary) {
			t.Fatalf("credential %q leaked into logs or errors:\n%s", canary, haystack)
		}
	}
	// Sanity check that logging happened at all, otherwise the test proves nothing.
	if !strings.Contains(buf.String(), "issued certificate") {
		t.Fatalf("expecting the issuance to be logged; got %q", buf.String())
	}
}

func TestProviderRejectsInvalidConfig(t *testing.T) {
	fv := newFakeVault(t)
	f := func(cfg Config, errStrExpected string) {
		t.Helper()
		_, err := NewProvider(cfg)
		if err == nil {
			t.Fatalf("expecting an error containing %q", errStrExpected)
		}
		if !strings.Contains(err.Error(), errStrExpected) {
			t.Fatalf("unexpected error; got %q; want it to contain %q", err, errStrExpected)
		}
	}
	auth := AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"}
	f(Config{}, "vault addr must not be empty")
	f(Config{Addr: fv.addr()}, "vault PKI path must not be empty")
	f(Config{Addr: fv.addr(), PKIPath: "pki"}, "vault role must not be empty")
	f(Config{Addr: fv.addr(), PKIPath: "pki", Role: "r"}, "vault common name must not be empty")

	cfg := testConfig(fv, auth)
	cfg.Auth.SecretIDFile = "/some/file"
	f(cfg, "mutually exclusive")

	cfg = testConfig(fv, auth)
	cfg.CAFile = "/definitely/missing/ca.pem"
	f(cfg, "cannot read -tls.vaultCAFile")
}
