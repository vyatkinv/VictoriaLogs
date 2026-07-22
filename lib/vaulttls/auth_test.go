package vaulttls

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

func TestAuthConfigValidate(t *testing.T) {
	f := func(cfg AuthConfig, errStrExpected string) {
		t.Helper()
		err := cfg.validate()
		if errStrExpected == "" {
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			return
		}
		if err == nil {
			t.Fatalf("expecting an error containing %q; got nil", errStrExpected)
		}
		if !strings.Contains(err.Error(), errStrExpected) {
			t.Fatalf("unexpected error; got %q; want it to contain %q", err, errStrExpected)
		}
	}

	// token
	f(AuthConfig{Token: "t"}, "")
	f(AuthConfig{Method: AuthMethodToken, TokenFile: "/f"}, "")
	f(AuthConfig{Method: AuthMethodToken}, "vault token is not configured")
	f(AuthConfig{Method: AuthMethodToken, Token: "t", TokenFile: "/f"}, "mutually exclusive")

	// approle
	f(AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"}, "")
	f(AuthConfig{Method: AuthMethodAppRole, RoleIDFile: "/r", SecretIDWrappedFile: "/w"}, "")
	f(AuthConfig{Method: AuthMethodAppRole, SecretID: "s"}, "role_id is not configured")
	f(AuthConfig{Method: AuthMethodAppRole, RoleID: "r", RoleIDFile: "/r", SecretID: "s"}, "mutually exclusive")
	f(AuthConfig{Method: AuthMethodAppRole, RoleID: "r"}, "secret_id is not configured")
	f(AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s", SecretIDFile: "/s"}, "mutually exclusive")
	f(AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretIDFile: "/s", SecretIDWrappedFile: "/w"}, "mutually exclusive")

	// kubernetes
	f(AuthConfig{Method: AuthMethodKubernetes, Role: "vl"}, "")
	f(AuthConfig{Method: AuthMethodKubernetes}, "-tls.vaultAuthRole")

	// unknown method
	f(AuthConfig{Method: "aws"}, `unsupported -tls.vaultAuthMethod="aws"`)
}

func TestAuthConfigValidateSetsDefaults(t *testing.T) {
	cfg := AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if cfg.Mount != "approle" {
		t.Fatalf("unexpected mount; got %q; want %q", cfg.Mount, "approle")
	}

	cfg = AuthConfig{Method: AuthMethodKubernetes, Role: "vl", Mount: "/k8s/"}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if cfg.Mount != "k8s" {
		t.Fatalf("unexpected mount; got %q; want %q", cfg.Mount, "k8s")
	}
	if cfg.JWTFile != DefaultKubernetesJWTFile {
		t.Fatalf("unexpected JWTFile; got %q; want %q", cfg.JWTFile, DefaultKubernetesJWTFile)
	}
}

func TestTokenSourceAppRoleLogin(t *testing.T) {
	fv := newFakeVault(t)
	ts := mustNewTokenSource(t, fv, AuthConfig{
		Method:   AuthMethodAppRole,
		RoleID:   "role-id-value",
		SecretID: "secret-id-value",
	})

	token, err := ts.get(false)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if token != "login-token-1" {
		t.Fatalf("unexpected token; got %q", token)
	}
	if got, want := fv.loginPaths[0], "/v1/auth/approle/login"; got != want {
		t.Fatalf("unexpected login path; got %q; want %q", got, want)
	}
	body := fv.loginBodies[0]
	if body["role_id"] != "role-id-value" || body["secret_id"] != "secret-id-value" {
		t.Fatalf("unexpected login body: %v", body)
	}
}

func TestTokenSourceKubernetesLoginRereadsJWT(t *testing.T) {
	fv := newFakeVault(t)
	fv.leaseDuration = 0 // non-expiring, so only forceRefresh triggers a re-login
	jwtFile := mustWriteCredential(t, "jwt", "jwt-value-1")
	ts := mustNewTokenSource(t, fv, AuthConfig{
		Method:  AuthMethodKubernetes,
		Mount:   "kubernetes-prod",
		Role:    "victoria-logs",
		JWTFile: jwtFile,
	})

	if _, err := ts.get(false); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	// The projected service account token is rotated by kubelet, so the next
	// login must present the new value rather than a cached one.
	mustWriteFile(t, jwtFile, "jwt-value-2")
	if _, err := ts.get(true); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if got, want := fv.loginPaths[0], "/v1/auth/kubernetes-prod/login"; got != want {
		t.Fatalf("unexpected login path; got %q; want %q", got, want)
	}
	if got := fv.loginBodies[0]; got["role"] != "victoria-logs" || got["jwt"] != "jwt-value-1" {
		t.Fatalf("unexpected first login body: %v", got)
	}
	if got := fv.loginBodies[1]; got["jwt"] != "jwt-value-2" {
		t.Fatalf("unexpected second login body: %v", got)
	}
}

func TestTokenSourceCaching(t *testing.T) {
	fv := newFakeVault(t)
	fv.leaseDuration = 3600
	ts := mustNewTokenSource(t, fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})

	for i := 0; i < 3; i++ {
		if _, err := ts.get(false); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	}
	if logins, _, _ := fv.stats(); logins != 1 {
		t.Fatalf("unexpected number of logins; got %d; want 1", logins)
	}

	// Two thirds of the lease are gone — the token must be replaced.
	ts.mu.Lock()
	ts.refreshAt = time.Now().Add(-time.Second)
	ts.mu.Unlock()
	token, err := ts.get(false)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if token != "login-token-2" {
		t.Fatalf("unexpected token after refresh; got %q", token)
	}
}

func TestTokenSourceNonExpiringToken(t *testing.T) {
	fv := newFakeVault(t)
	fv.leaseDuration = 0
	ts := mustNewTokenSource(t, fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})

	for i := 0; i < 3; i++ {
		if _, err := ts.get(false); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	}
	if logins, _, _ := fv.stats(); logins != 1 {
		t.Fatalf("unexpected number of logins; got %d; want 1", logins)
	}
	if !ts.refreshAt.IsZero() {
		t.Fatalf("expecting a zero refreshAt for a non-expiring token; got %s", ts.refreshAt)
	}
}

func TestTokenSourceWrappedSecretID(t *testing.T) {
	fv := newFakeVault(t)
	fv.leaseDuration = 60
	fv.wrappedSecretID = "unwrapped-secret-id"
	wrappedFile := mustWriteCredential(t, "wrapped", "wrapping-token-value")
	ts := mustNewTokenSource(t, fv, AuthConfig{
		Method:              AuthMethodAppRole,
		RoleID:              "r",
		SecretIDWrappedFile: wrappedFile,
	})

	if _, err := ts.get(false); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	// A wrapping token is single-use, so a re-login must reuse the unwrapped
	// secret_id rather than unwrap again.
	if _, err := ts.get(true); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	logins, unwraps, _ := fv.stats()
	if logins != 2 {
		t.Fatalf("unexpected number of logins; got %d; want 2", logins)
	}
	if unwraps != 1 {
		t.Fatalf("unexpected number of unwraps; got %d; want 1", unwraps)
	}
	if got, want := fv.unwrapTokens[0], "wrapping-token-value"; got != want {
		t.Fatalf("unexpected wrapping token; got %q; want %q", got, want)
	}
	for i, body := range fv.loginBodies {
		if body["secret_id"] != "unwrapped-secret-id" {
			t.Fatalf("unexpected secret_id in login #%d: %v", i, body)
		}
	}
}

func TestTokenSourceWrappedSecretIDFailure(t *testing.T) {
	fv := newFakeVault(t)
	fv.unwrapStatus = http.StatusBadRequest
	wrappedFile := mustWriteCredential(t, "wrapped", "wrapping-token-value")
	ts := mustNewTokenSource(t, fv, AuthConfig{
		Method:              AuthMethodAppRole,
		RoleID:              "r",
		SecretIDWrappedFile: wrappedFile,
	})

	_, err := ts.get(false)
	if err == nil {
		t.Fatalf("expecting an error on a failed unwrap")
	}
	// The error must tell the operator that this may be an interception, and it
	// must not contain the wrapping token itself.
	for _, s := range []string{"intercepted", "single-use", "wrapping token is not valid"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("expecting the error to contain %q; got %q", s, err)
		}
	}
	if strings.Contains(err.Error(), "wrapping-token-value") {
		t.Fatalf("the error leaks the wrapping token: %q", err)
	}
}

func TestTokenSourceRevoke(t *testing.T) {
	fv := newFakeVault(t)
	ts := mustNewTokenSource(t, fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})
	if _, err := ts.get(false); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	ts.revoke()
	fv.mu.Lock()
	revoked := append([]string(nil), fv.revokedTokens...)
	fv.mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "login-token-1" {
		t.Fatalf("unexpected revoked tokens: %v", revoked)
	}
	if ts.token != "" {
		t.Fatalf("expecting the cached token to be dropped; got %q", ts.token)
	}
}

func TestTokenSourceRevokeIsNoOpForStaticToken(t *testing.T) {
	fv := newFakeVault(t)
	ts := mustNewTokenSource(t, fv, AuthConfig{Method: AuthMethodToken, Token: "static-token"})
	if _, err := ts.get(false); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// The static token belongs to the operator: revoking it would break the next start.
	ts.revoke()
	fv.mu.Lock()
	revoked := len(fv.revokedTokens)
	fv.mu.Unlock()
	if revoked != 0 {
		t.Fatalf("expecting no revocations for the token auth method; got %d", revoked)
	}
}

func TestTokenSourceStaticTokenFileIsRereadOnEveryUse(t *testing.T) {
	fv := newFakeVault(t)
	tokenFile := mustWriteCredential(t, "token", "token-value-1")
	ts := mustNewTokenSource(t, fv, AuthConfig{Method: AuthMethodToken, TokenFile: tokenFile})

	token, err := ts.get(false)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if token != "token-value-1" {
		t.Fatalf("unexpected token; got %q", token)
	}
	mustWriteFile(t, tokenFile, "token-value-2")
	token, err = ts.get(false)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if token != "token-value-2" {
		t.Fatalf("unexpected token after rotation; got %q", token)
	}
	if logins, _, _ := fv.stats(); logins != 0 {
		t.Fatalf("expecting no logins for the token auth method; got %d", logins)
	}
}

func TestReadCredentialFileWarnsOnLoosePermissions(t *testing.T) {
	path := mustWriteCredential(t, "loose", "secret-value")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("cannot chmod: %s", err)
	}

	var buf bytes.Buffer
	logger.SetOutputForTests(&buf)
	defer logger.ResetOutputForTest()

	value, err := readCredentialFile(path, "test credential")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if value != "secret-value" {
		t.Fatalf("unexpected value; got %q", value)
	}
	logs := buf.String()
	if !strings.Contains(logs, "readable by group or other") || !strings.Contains(logs, path) {
		t.Fatalf("expecting a permissions warning for %q; got %q", path, logs)
	}
	if strings.Contains(logs, "secret-value") {
		t.Fatalf("the warning leaks the credential: %q", logs)
	}

	// The warning must be reported once per path, not on every renewal.
	buf.Reset()
	if _, err := readCredentialFile(path, "test credential"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if strings.Contains(buf.String(), "readable by group or other") {
		t.Fatalf("expecting no repeated warning; got %q", buf.String())
	}
}

func TestReadCredentialFileErrors(t *testing.T) {
	empty := mustWriteCredential(t, "empty", "  \n ")
	if _, err := readCredentialFile(empty, "test credential"); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("expecting an emptiness error; got %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := readCredentialFile(missing, "test credential"); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("expecting a read error mentioning the path; got %v", err)
	}
}

func TestVaultErrorMessage(t *testing.T) {
	f := func(respBody, resultExpected string) {
		t.Helper()
		if result := vaultErrorMessage([]byte(respBody)); result != resultExpected {
			t.Fatalf("unexpected result for %q; got %q; want %q", respBody, result, resultExpected)
		}
	}
	f(`{"errors":["permission denied"]}`, "permission denied")
	f(`{"errors":["a","b"]}`, "a; b")
	f(`{"errors":[]}`, `{"errors":[]}`)
	f(`not json`, "not json")
	f(``, "(empty response body)")
	f(`{"errors":["`+strings.Repeat("x", 600)+`"]}`, strings.Repeat("x", 600))
}

func TestVaultURL(t *testing.T) {
	f := func(addr, path, resultExpected string) {
		t.Helper()
		if result := vaultURL(addr, path); result != resultExpected {
			t.Fatalf("unexpected result for (%q, %q); got %q; want %q", addr, path, result, resultExpected)
		}
	}
	f("https://vault:8200", "pki/issue/vl", "https://vault:8200/v1/pki/issue/vl")
	f("https://vault:8200/", "/pki/issue/vl/", "https://vault:8200/v1/pki/issue/vl")
	f("https://vault:8200//", "auth/approle/login", "https://vault:8200/v1/auth/approle/login")
}

func mustNewTokenSource(t *testing.T, fv *fakeVault, cfg AuthConfig) *tokenSource {
	t.Helper()
	ts, err := newTokenSource(fv.addr(), fv.srv.Client(), cfg)
	if err != nil {
		t.Fatalf("cannot create tokenSource: %s", err)
	}
	return ts
}

// mustWriteCredential writes a credential file with 0600 permissions and returns its path.
func mustWriteCredential(t *testing.T, name, value string) string {
	t.Helper()
	// A unique directory per call keeps the global readCredentialFile warn-once
	// state from leaking between tests.
	path := filepath.Join(t.TempDir(), fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	mustWriteFile(t, path, value)
	return path
}

func mustWriteFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("cannot write %q: %s", path, err)
	}
}
