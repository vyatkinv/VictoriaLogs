package vaulttls

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

// Supported values of AuthConfig.Method.
const (
	// AuthMethodToken uses a static Vault token supplied by the operator.
	AuthMethodToken = "token"
	// AuthMethodAppRole logs in via the AppRole auth method (role_id + secret_id).
	AuthMethodAppRole = "approle"
	// AuthMethodKubernetes logs in via the Kubernetes auth method
	// (Vault role + the pod's service account JWT).
	AuthMethodKubernetes = "kubernetes"
)

// DefaultKubernetesJWTFile is the in-cluster location of the projected service
// account token used by AuthMethodKubernetes.
const DefaultKubernetesJWTFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// AuthConfig describes how to obtain a Vault token.
//
// Prefer AuthMethodKubernetes where it is available: the service account JWT is
// short-lived, rotated by kubelet and bound to the pod identity, so there is no
// long-lived credential to steal. AuthMethodAppRole is the fallback outside
// Kubernetes; its secret_id is a bearer credential and should be delivered
// response-wrapped via SecretIDWrappedFile whenever the orchestrator can do it.
type AuthConfig struct {
	// Method is one of AuthMethodToken (default), AuthMethodAppRole, AuthMethodKubernetes.
	Method string
	// Mount is the auth method mount path. Defaults to Method.
	Mount string

	// Token is a static Vault token. Method=token only.
	Token string
	// GetToken, when set, returns the static Vault token and takes precedence
	// over Token. It is called on every use, which lets flagutil.Password re-read
	// its file:// and http:// sources. Method=token only.
	GetToken func() string
	// TokenFile is a path to a file with a static Vault token. It is re-read on
	// every use, so an externally rotated token is picked up. Method=token only.
	TokenFile string

	// RoleID and RoleIDFile hold the AppRole role_id. Method=approle only.
	RoleID     string
	RoleIDFile string

	// SecretID, SecretIDFile and SecretIDWrappedFile hold the AppRole secret_id
	// and are mutually exclusive. SecretIDWrappedFile points to a single-use
	// response-wrapping token which is unwrapped into the secret_id on first
	// login. Method=approle only.
	SecretID            string
	SecretIDFile        string
	SecretIDWrappedFile string
	// GetSecretID, when set, returns the AppRole secret_id and takes precedence
	// over SecretID. See GetToken.
	GetSecretID func() string

	// Role is the Vault Kubernetes auth role. Note this is not the PKI role from
	// Config.Role. Method=kubernetes only.
	Role string
	// JWTFile is the path to the service account token presented to Vault.
	// Defaults to DefaultKubernetesJWTFile. Method=kubernetes only.
	JWTFile string
}

// validate checks cfg for completeness and fills in defaults.
func (cfg *AuthConfig) validate() error {
	if cfg.Method == "" {
		cfg.Method = AuthMethodToken
	}
	cfg.Mount = strings.Trim(cfg.Mount, "/")
	switch cfg.Method {
	case AuthMethodToken:
		hasToken := cfg.Token != "" || cfg.GetToken != nil
		if hasToken && cfg.TokenFile != "" {
			return fmt.Errorf("-tls.vaultToken and -tls.vaultTokenFile are mutually exclusive")
		}
		if !hasToken && cfg.TokenFile == "" {
			return fmt.Errorf("vault token is not configured; set -tls.vaultToken or -tls.vaultTokenFile, " +
				"or pick a keyless auth method via -tls.vaultAuthMethod")
		}
	case AuthMethodAppRole:
		if cfg.Mount == "" {
			cfg.Mount = AuthMethodAppRole
		}
		if cfg.RoleID != "" && cfg.RoleIDFile != "" {
			return fmt.Errorf("-tls.vaultAuthRoleID and -tls.vaultAuthRoleIDFile are mutually exclusive")
		}
		if cfg.RoleID == "" && cfg.RoleIDFile == "" {
			return fmt.Errorf("approle role_id is not configured; set -tls.vaultAuthRoleID or -tls.vaultAuthRoleIDFile")
		}
		n := 0
		if cfg.SecretID != "" || cfg.GetSecretID != nil {
			n++
		}
		for _, s := range []string{cfg.SecretIDFile, cfg.SecretIDWrappedFile} {
			if s != "" {
				n++
			}
		}
		if n > 1 {
			return fmt.Errorf("-tls.vaultAuthSecretID, -tls.vaultAuthSecretIDFile and -tls.vaultAuthSecretIDWrappedFile are mutually exclusive")
		}
		if n == 0 {
			return fmt.Errorf("approle secret_id is not configured; set -tls.vaultAuthSecretID, -tls.vaultAuthSecretIDFile " +
				"or -tls.vaultAuthSecretIDWrappedFile")
		}
	case AuthMethodKubernetes:
		if cfg.Mount == "" {
			cfg.Mount = AuthMethodKubernetes
		}
		if cfg.Role == "" {
			return fmt.Errorf("kubernetes auth role is not configured; set -tls.vaultAuthRole " +
				"(this is the Vault auth role, not the PKI role from -tls.vaultRole)")
		}
		if cfg.JWTFile == "" {
			cfg.JWTFile = DefaultKubernetesJWTFile
		}
	default:
		return fmt.Errorf("unsupported -tls.vaultAuthMethod=%q; supported values are %q, %q and %q",
			cfg.Method, AuthMethodToken, AuthMethodAppRole, AuthMethodKubernetes)
	}
	return nil
}

// tokenSource obtains Vault tokens according to cfg and caches the current one.
//
// The cached token is refreshed once two thirds of its lease have been consumed.
// There is no background renewal goroutine: a token is only needed when a
// certificate is issued, which happens roughly once per certificate lifetime, and
// a fresh login is more robust than renew-self, which runs into token_max_ttl.
type tokenSource struct {
	cfg    AuthConfig
	addr   string
	client *http.Client

	mu sync.Mutex
	// token is the cached Vault token; empty when no login has happened yet.
	token string
	// accessor identifies token in the Vault audit log. It is not a credential.
	accessor string
	// refreshAt is when token must be replaced. Zero means it never expires.
	refreshAt time.Time
	// secretID caches the unwrapped AppRole secret_id. A response-wrapping token
	// is single-use, so the unwrapped value must survive for the whole process.
	secretID string
}

func newTokenSource(addr string, client *http.Client, cfg AuthConfig) (*tokenSource, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &tokenSource{
		cfg:    cfg,
		addr:   addr,
		client: client,
	}, nil
}

// get returns a Vault token, logging in if the cached one is missing, stale or
// forceRefresh is set.
func (ts *tokenSource) get(forceRefresh bool) (string, error) {
	if ts.cfg.Method == AuthMethodToken {
		return ts.staticToken()
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !forceRefresh && ts.token != "" && (ts.refreshAt.IsZero() || time.Now().Before(ts.refreshAt)) {
		return ts.token, nil
	}
	if err := ts.login(); err != nil {
		return "", err
	}
	return ts.token, nil
}

// revoke drops the cached token and revokes it in Vault, so that a token leaked
// from the process memory cannot be replayed after shutdown.
//
// It is a no-op for AuthMethodToken: that token belongs to the operator and
// revoking it would break the next start.
func (ts *tokenSource) revoke() {
	if ts.cfg.Method == AuthMethodToken {
		return
	}
	ts.mu.Lock()
	token, accessor := ts.token, ts.accessor
	ts.token, ts.accessor, ts.refreshAt = "", "", time.Time{}
	ts.mu.Unlock()

	if token == "" {
		return
	}
	if _, _, err := doVaultRequest(ts.client, http.MethodPost, vaultURL(ts.addr, "auth/token/revoke-self"), token, nil); err != nil {
		// Expected for batch tokens, which cannot be revoked, and for tokens that
		// already expired. Not worth failing the shutdown over.
		logger.Warnf("vaulttls: cannot revoke the vault token with accessor %q: %s", accessor, err)
		return
	}
	logger.Infof("vaulttls: revoked the vault token with accessor %q", accessor)
}

// staticToken returns the operator-supplied token, re-reading TokenFile on every
// call so that external rotation is picked up.
func (ts *tokenSource) staticToken() (string, error) {
	if ts.cfg.TokenFile != "" {
		return readCredentialFile(ts.cfg.TokenFile, "vault token")
	}
	token := ts.cfg.Token
	if ts.cfg.GetToken != nil {
		token = ts.cfg.GetToken()
	}
	if token == "" {
		return "", fmt.Errorf("vault token is not configured; set -tls.vaultToken or -tls.vaultTokenFile")
	}
	return token, nil
}

// login performs a login against the configured auth method and caches the
// resulting token. ts.mu must be held.
func (ts *tokenSource) login() error {
	reqBody := make(map[string]string, 2)
	switch ts.cfg.Method {
	case AuthMethodAppRole:
		roleID, err := ts.roleIDValue()
		if err != nil {
			return err
		}
		secretID, err := ts.secretIDValue()
		if err != nil {
			return err
		}
		reqBody["role_id"] = roleID
		reqBody["secret_id"] = secretID
	case AuthMethodKubernetes:
		jwt, err := readCredentialFile(ts.cfg.JWTFile, "kubernetes service account token")
		if err != nil {
			return err
		}
		reqBody["role"] = ts.cfg.Role
		reqBody["jwt"] = jwt
	default:
		return fmt.Errorf("BUG: unexpected auth method %q", ts.cfg.Method)
	}

	loginURL := vaultURL(ts.addr, "auth/"+ts.cfg.Mount+"/login")
	respBytes, _, err := doVaultRequest(ts.client, http.MethodPost, loginURL, "", reqBody)
	if err != nil {
		return fmt.Errorf("cannot authenticate in vault via the %q auth method: %w", ts.cfg.Method, err)
	}
	var resp struct {
		Auth struct {
			ClientToken   string   `json:"client_token"`
			Accessor      string   `json:"accessor"`
			TokenPolicies []string `json:"token_policies"`
			LeaseDuration int64    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return fmt.Errorf("cannot parse the vault login response from %s: %w", loginURL, err)
	}
	if resp.Auth.ClientToken == "" {
		return fmt.Errorf("the vault login response from %s contains no auth.client_token", loginURL)
	}

	ts.token = resp.Auth.ClientToken
	ts.accessor = resp.Auth.Accessor
	lease := time.Duration(resp.Auth.LeaseDuration) * time.Second
	if lease > 0 {
		// Same policy as for certificates: refresh after two thirds of the lease.
		ts.refreshAt = time.Now().Add(lease * 2 / 3)
	} else {
		ts.refreshAt = time.Time{}
	}
	logger.Infof("vaulttls: authenticated in vault via the %q auth method at mount %q; token accessor=%q, policies=%v, lease=%s",
		ts.cfg.Method, ts.cfg.Mount, ts.accessor, resp.Auth.TokenPolicies, lease)
	return nil
}

func (ts *tokenSource) roleIDValue() (string, error) {
	if ts.cfg.RoleIDFile != "" {
		return readCredentialFile(ts.cfg.RoleIDFile, "approle role_id")
	}
	return ts.cfg.RoleID, nil
}

// secretIDValue returns the AppRole secret_id, unwrapping the response-wrapped
// token on first use. ts.mu must be held.
func (ts *tokenSource) secretIDValue() (string, error) {
	if ts.cfg.SecretIDWrappedFile == "" {
		if ts.cfg.SecretIDFile != "" {
			return readCredentialFile(ts.cfg.SecretIDFile, "approle secret_id")
		}
		secretID := ts.cfg.SecretID
		if ts.cfg.GetSecretID != nil {
			secretID = ts.cfg.GetSecretID()
		}
		if secretID == "" {
			return "", fmt.Errorf("the configured approle secret_id is empty")
		}
		return secretID, nil
	}
	if ts.secretID != "" {
		return ts.secretID, nil
	}
	wrappingToken, err := readCredentialFile(ts.cfg.SecretIDWrappedFile, "wrapped approle secret_id")
	if err != nil {
		return "", err
	}
	secretID, err := ts.unwrapSecretID(wrappingToken)
	if err != nil {
		return "", err
	}
	ts.secretID = secretID
	return secretID, nil
}

// unwrapSecretID exchanges a response-wrapping token for the secret_id it holds.
//
// The wrapping token is single-use, so a failure here means either that the
// credential was never delivered, or that somebody else has already unwrapped it.
// The latter is an interception, which is exactly what response wrapping is meant
// to make detectable — hence the loud error text.
func (ts *tokenSource) unwrapSecretID(wrappingToken string) (string, error) {
	unwrapURL := vaultURL(ts.addr, "sys/wrapping/unwrap")
	respBytes, _, err := doVaultRequest(ts.client, http.MethodPost, unwrapURL, wrappingToken, nil)
	if err != nil {
		return "", fmt.Errorf("cannot unwrap the response-wrapped secret_id from %q: %w; "+
			"a wrapping token is single-use, so if it has already been used or has expired, the credential may have been "+
			"intercepted: destroy the secret_id accessor, deliver a freshly wrapped secret_id and inspect the vault audit log",
			ts.cfg.SecretIDWrappedFile, err)
	}
	var resp struct {
		Data struct {
			SecretID string `json:"secret_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return "", fmt.Errorf("cannot parse the unwrap response from %s: %w", unwrapURL, err)
	}
	if resp.Data.SecretID == "" {
		return "", fmt.Errorf("the response-wrapped payload from %q contains no secret_id field; "+
			"wrap the output of auth/approle/role/<role>/secret-id", ts.cfg.SecretIDWrappedFile)
	}
	return resp.Data.SecretID, nil
}

// warnedPaths keeps credential files whose permissions have already been reported,
// so a warning isn't repeated on every renewal.
var warnedPaths sync.Map

// readCredentialFile reads a credential from path and warns if the file is
// readable by anybody besides its owner. what describes the credential in
// error messages; the credential itself is never included in them.
func readCredentialFile(path, what string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the %s file %q: %w", what, path, err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		if _, alreadyWarned := warnedPaths.LoadOrStore(path, struct{}{}); !alreadyWarned {
			logger.Warnf("vaulttls: the %s file %q has mode %04o and is readable by group or other; restrict it to 0600",
				what, path, perm)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the %s file %q: %w", what, path, err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", fmt.Errorf("the %s file %q is empty", what, path)
	}
	return s, nil
}

// vaultURL builds a Vault API URL for the given path relative to /v1/.
func vaultURL(addr, path string) string {
	return strings.TrimRight(addr, "/") + "/v1/" + strings.Trim(path, "/")
}

// doVaultRequest performs a Vault API call and returns the response body together
// with the HTTP status code, which callers use to detect authentication failures.
//
// reqBody, when non-nil, is marshalled to JSON. It may contain credentials, so it
// is never included in the returned error.
func doVaultRequest(client *http.Client, method, reqURL, token string, reqBody any) ([]byte, int, error) {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, 0, fmt.Errorf("cannot marshal the request body for %s: %w", reqURL, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, reqURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot create a request to %s: %w", reqURL, err)
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot contact vault at %s: %w", reqURL, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("cannot read the vault response from %s: %w", reqURL, err)
	}
	if resp.StatusCode/100 != 2 {
		return respBytes, resp.StatusCode, fmt.Errorf("vault returned HTTP %d for %s: %s",
			resp.StatusCode, reqURL, vaultErrorMessage(respBytes))
	}
	return respBytes, resp.StatusCode, nil
}

// vaultErrorMessage extracts the human-readable part of a Vault error response,
// which has the form {"errors":["permission denied"]}.
func vaultErrorMessage(respBytes []byte) string {
	var resp struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(respBytes, &resp); err == nil && len(resp.Errors) > 0 {
		return strings.Join(resp.Errors, "; ")
	}
	s := string(respBytes)
	if len(s) > 512 {
		s = s[:512]
	}
	if s == "" {
		return "(empty response body)"
	}
	return s
}
