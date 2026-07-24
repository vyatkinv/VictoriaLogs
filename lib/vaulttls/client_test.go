package vaulttls

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httputil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promauth"
)

// mustNewProvider creates a provider against fv and registers it as the
// process-wide one, so that NewRoundTripper picks it up.
func mustNewProvider(t *testing.T, cfg Config) *Provider {
	t.Helper()
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("cannot create provider: %s", err)
	}
	t.Cleanup(p.Stop)
	Register(p)
	t.Cleanup(func() {
		registered.Store(nil)
	})
	return p
}

func mustNewAuthConfig(t *testing.T, tlsCfg *promauth.TLSConfig) *promauth.Config {
	t.Helper()
	opts := &promauth.Options{
		TLSConfig: tlsCfg,
	}
	ac, err := opts.NewConfig()
	if err != nil {
		t.Fatalf("cannot create auth config: %s", err)
	}
	return ac
}

// newMTLSServer starts a TLS server which requires a client certificate signed by
// the CA of fv.
func newMTLSServer(t *testing.T, fv *fakeVault, requireClientCert bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := ""
		if len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		_, _ = io.WriteString(w, "peer="+cn)
	}))
	clientAuth := tls.NoClientCert
	if requireClientCert {
		clientAuth = tls.RequireAndVerifyClientCert
	}
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{fv.mustIssueServerCert(t)},
		ClientAuth:   clientAuth,
		ClientCAs:    fv.caPool(),
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func doGet(t *testing.T, rt http.RoundTripper, url string) (string, error) {
	t.Helper()
	c := &http.Client{
		Transport: rt,
		Timeout:   10 * time.Second,
	}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("cannot read the response body: %s", err)
	}
	return string(body), nil
}

func TestProviderFetchesPKICA(t *testing.T) {
	fv := newFakeVault(t)
	cfg := testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})
	cfg.TrustPKICA = true
	p := mustNewProvider(t, cfg)

	if !bytes.Equal(p.CACertPEM(), fv.caPEM) {
		t.Fatalf("unexpected CA bundle; got %q; want %q", p.CACertPEM(), fv.caPEM)
	}
	fv.mu.Lock()
	reads, tokens := append([]string{}, fv.caReads...), append([]string{}, fv.caReadTokens...)
	fv.mu.Unlock()
	if len(reads) != 1 || !strings.HasSuffix(reads[0], "/pki/cert/ca_chain") {
		t.Fatalf("unexpected CA reads: %v", reads)
	}
	// pki/cert/* is unauthenticated in Vault, so the CA must be fetched without a token.
	if tokens[0] != "" {
		t.Fatalf("expecting no vault token on the CA read; got %q", tokens[0])
	}
}

func TestProviderFallsBackToCertCA(t *testing.T) {
	fv := newFakeVault(t)
	fv.emptyCAChain = true
	cfg := testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})
	cfg.TrustPKICA = true
	p := mustNewProvider(t, cfg)

	if !bytes.Equal(p.CACertPEM(), fv.caPEM) {
		t.Fatalf("unexpected CA bundle; got %q", p.CACertPEM())
	}
	fv.mu.Lock()
	reads := append([]string{}, fv.caReads...)
	fv.mu.Unlock()
	if len(reads) != 2 || !strings.HasSuffix(reads[1], "/pki/cert/ca") {
		t.Fatalf("expecting a fallback to pki/cert/ca; got the reads %v", reads)
	}
}

func TestProviderWithoutTrustPKICAHasNoCA(t *testing.T) {
	fv := newFakeVault(t)
	p := mustNewProvider(t, testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"}))

	if p.CACertPEM() != nil {
		t.Fatalf("expecting no CA bundle without TrustPKICA")
	}
	fv.mu.Lock()
	reads := len(fv.caReads)
	fv.mu.Unlock()
	if reads != 0 {
		t.Fatalf("expecting no CA reads without TrustPKICA; got %d", reads)
	}
}

func TestNewRoundTripperFallsBackToPromauth(t *testing.T) {
	fv := newFakeVault(t)
	srv := newMTLSServer(t, fv, false)

	// No provider registered: the CA of the test server must come from the auth
	// config alone, and no client certificate is presented.
	ac := mustNewAuthConfig(t, &promauth.TLSConfig{CA: string(fv.caPEM)})
	rt := NewRoundTripper(ac, httputil.NewTransport(false, "test"))
	body, err := doGet(t, rt, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if body != "peer=" {
		t.Fatalf("expecting no client certificate; got %q", body)
	}

	// A provider without ClientAuth and TrustPKICA must not change anything either.
	mustNewProvider(t, testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"}))
	rt = NewRoundTripper(ac, httputil.NewTransport(false, "test"))
	if _, ok := rt.(*vaultRoundTripper); ok {
		t.Fatalf("expecting the promauth round tripper when neither ClientAuth nor TrustPKICA is set")
	}
}

func TestRoundTripperPresentsVaultClientCertificate(t *testing.T) {
	fv := newFakeVault(t)
	srv := newMTLSServer(t, fv, true)

	cfg := testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})
	cfg.ClientAuth = true
	cfg.TrustPKICA = true
	mustNewProvider(t, cfg)

	// No CA file and no client certificate file are configured: both come from Vault.
	ac := mustNewAuthConfig(t, &promauth.TLSConfig{})
	rt := NewRoundTripper(ac, httputil.NewTransport(false, "test"))
	body, err := doGet(t, rt, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if body != "peer=localhost" {
		t.Fatalf("unexpected client certificate presented to the server; got %q", body)
	}
}

func TestRoundTripperWithoutClientAuthIsRejected(t *testing.T) {
	fv := newFakeVault(t)
	srv := newMTLSServer(t, fv, true)

	cfg := testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})
	cfg.TrustPKICA = true
	mustNewProvider(t, cfg)

	ac := mustNewAuthConfig(t, &promauth.TLSConfig{})
	rt := NewRoundTripper(ac, httputil.NewTransport(false, "test"))
	if _, err := doGet(t, rt, srv.URL); err == nil {
		t.Fatalf("expecting the server to reject a connection without a client certificate")
	}
}

func TestRoundTripperKeepsConfiguredCA(t *testing.T) {
	// The peer is verified against the CA file configured for the connection,
	// while the Vault PKI CA is added on top of it. An unrelated CA in the auth
	// config must not be dropped by -tls.vaultTrustPKICA, and must not be enough
	// on its own either.
	fvPeer := newFakeVault(t)
	srv := newMTLSServer(t, fvPeer, false)

	fv := newFakeVault(t)
	cfg := testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})
	cfg.TrustPKICA = true
	p := mustNewProvider(t, cfg)

	ac := mustNewAuthConfig(t, &promauth.TLSConfig{CA: string(fvPeer.caPEM)})
	rt := NewRoundTripper(ac, httputil.NewTransport(false, "test"))
	if _, err := doGet(t, rt, srv.URL); err != nil {
		t.Fatalf("unexpected error with the configured CA: %s", err)
	}

	// Sanity check: the Vault CA alone doesn't verify this peer, so the request
	// above indeed succeeded thanks to the configured CA.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.CACertPEM())
	tr := httputil.NewTransport(false, "test")
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if _, err := doGet(t, tr, srv.URL); err == nil {
		t.Fatalf("expecting a verification failure against the vault CA alone")
	}
}

func TestRoundTripperServesRotatedCertificate(t *testing.T) {
	fv := newFakeVault(t)
	srv := newMTLSServer(t, fv, true)

	cfg := testConfig(fv, AuthConfig{Method: AuthMethodAppRole, RoleID: "r", SecretID: "s"})
	cfg.ClientAuth = true
	cfg.TrustPKICA = true
	p := mustNewProvider(t, cfg)

	rt := NewRoundTripper(mustNewAuthConfig(t, &promauth.TLSConfig{}), httputil.NewTransport(false, "test"))
	if _, err := doGet(t, rt, srv.URL); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := p.renew(); err != nil {
		t.Fatalf("unexpected error on renew: %s", err)
	}
	// The rotated certificate is served through the GetClientCertificate callback,
	// so no transport rebuild is needed for the next connection to use it.
	cert, err := p.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	fv.mu.Lock()
	lastSerial := fv.serials[len(fv.serials)-1]
	fv.mu.Unlock()
	if got := formatSerial(cert.Leaf.SerialNumber); got != lastSerial {
		t.Fatalf("unexpected client certificate serial; got %q; want %q", got, lastSerial)
	}
	if _, err := doGet(t, rt, srv.URL); err != nil {
		t.Fatalf("unexpected error after renewal: %s", err)
	}
}
