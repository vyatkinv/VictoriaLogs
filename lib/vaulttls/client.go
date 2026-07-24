package vaulttls

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promauth"
)

// caBundle is the CA of the PKI mount, kept both as PEM (for merging into a pool
// configured elsewhere) and as a ready-to-use pool on top of the system one.
type caBundle struct {
	pem  []byte
	pool *x509.CertPool
}

// GetClientCertificate implements tls.Config.GetClientCertificate, presenting the
// in-memory Vault certificate on outgoing connections.
//
// The certificate is the same one served to incoming connections, so the PKI role
// must allow both usages (server_flag=true and client_flag=true). Without
// client_flag the certificate has no clientAuth extended key usage and the peer
// rejects the handshake.
func (p *Provider) GetClientCertificate(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cert == nil {
		return nil, fmt.Errorf("vaulttls: no certificate available")
	}
	return p.cert, nil
}

// CACertPEM returns the PEM-encoded CA bundle of the PKI mount, or nil when
// Config.TrustPKICA is not set.
func (p *Provider) CACertPEM() []byte {
	b := p.ca.Load()
	if b == nil {
		return nil
	}
	return b.pem
}

// refreshCA fetches the CA of the PKI mount and publishes it for outgoing
// connections.
func (p *Provider) refreshCA() error {
	caPEM, err := p.fetchCA()
	if err != nil {
		return err
	}
	if prev := p.ca.Load(); prev != nil && bytes.Equal(prev.pem, caPEM) {
		return nil
	}
	pool, err := newCertPoolWithCA(caPEM)
	if err != nil {
		return err
	}
	p.ca.Store(&caBundle{
		pem:  caPEM,
		pool: pool,
	})
	logger.Infof("vaulttls: loaded the CA of the %q pki mount at %s for verifying outgoing connections",
		p.cfg.PKIPath, p.cfg.Addr)
	return nil
}

// fetchCA reads the CA certificate chain of the PKI mount.
//
// The cert/* paths of a PKI mount are unauthenticated in Vault, so no token is
// needed and the CA can be fetched before the first login. cert/ca_chain is empty
// on a mount holding a self-signed root, hence the fallback to cert/ca.
func (p *Provider) fetchCA() ([]byte, error) {
	pkiPath := strings.Trim(p.cfg.PKIPath, "/")
	var lastErr error
	for _, path := range []string{pkiPath + "/cert/ca_chain", pkiPath + "/cert/ca"} {
		caURL := vaultURL(p.cfg.Addr, path)
		respBytes, _, err := doVaultRequest(p.client, http.MethodGet, caURL, "", nil)
		if err != nil {
			lastErr = err
			continue
		}
		var resp struct {
			Data struct {
				Certificate string `json:"certificate"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respBytes, &resp); err != nil {
			lastErr = fmt.Errorf("cannot parse the vault response from %s: %w", caURL, err)
			continue
		}
		if resp.Data.Certificate == "" {
			lastErr = fmt.Errorf("the vault response from %s contains an empty certificate", caURL)
			continue
		}
		return []byte(resp.Data.Certificate), nil
	}
	return nil, fmt.Errorf("cannot fetch the CA of the %q pki mount at %s: %w", p.cfg.PKIPath, p.cfg.Addr, lastErr)
}

// newCertPoolWithCA returns the system pool extended with caPEM.
func newCertPoolWithCA(caPEM []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		// Not fatal: continue with a pool holding just the vault CA.
		logger.Warnf("vaulttls: cannot load the system certificate pool: %s; trusting only the vault pki CA", err)
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("cannot parse any certificate from the vault pki CA bundle")
	}
	return pool, nil
}

// NewRoundTripper returns an http.RoundTripper for outgoing requests authenticated
// by ac, which additionally presents the Vault-issued certificate as a client
// certificate when -tls.vaultClientAuth is set, and verifies the peer against the
// CA of the Vault PKI mount when -tls.vaultTrustPKICA is set.
//
// It falls back to ac.NewRoundTripper(tr) when no Vault provider is registered or
// neither option is enabled, so callers can use it unconditionally in place of
// ac.NewRoundTripper. The caller must not modify tr afterwards, since the returned
// RoundTripper owns it.
func NewRoundTripper(ac *promauth.Config, tr *http.Transport) http.RoundTripper {
	p := registered.Load()
	if p == nil || (!p.cfg.ClientAuth && !p.cfg.TrustPKICA) {
		return ac.NewRoundTripper(tr)
	}
	return &vaultRoundTripper{
		ac:     ac,
		p:      p,
		trBase: tr,
	}
}

// vaultRoundTripper layers the Vault client certificate and the Vault PKI CA on
// top of the TLS config built by promauth.
//
// It mirrors the caching done by promauth's own RoundTripper: the transport is
// rebuilt only when the underlying TLS config or the CA bundle changes. The client
// certificate is served through a callback, so its rotation needs no rebuild.
type vaultRoundTripper struct {
	ac     *promauth.Config
	p      *Provider
	trBase *http.Transport

	// mu protects the cached transport and the inputs it was built from.
	mu         sync.Mutex
	tlsCfgPrev *tls.Config
	caPrev     *caBundle
	trPrev     *http.Transport
}

// RoundTrip implements the http.RoundTripper interface.
func (rt *vaultRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tr, err := rt.getTransport()
	if err != nil {
		return nil, fmt.Errorf("cannot initialize Transport: %w", err)
	}
	return tr.RoundTrip(req)
}

func (rt *vaultRoundTripper) getTransport() (*http.Transport, error) {
	var tlsCfg *tls.Config
	if rt.ac != nil {
		var err error
		// promauth caches the config for a second, so the pointer is stable enough
		// to be used as the cache key below.
		tlsCfg, err = rt.ac.GetTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("cannot initialize TLS config: %w", err)
		}
	}
	ca := rt.p.ca.Load()

	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.trPrev != nil && rt.tlsCfgPrev == tlsCfg && rt.caPrev == ca {
		return rt.trPrev, nil
	}

	var cfg *tls.Config
	if tlsCfg != nil {
		cfg = tlsCfg.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if rt.p.cfg.ClientAuth {
		cfg.GetClientCertificate = rt.p.GetClientCertificate
	}
	if ca != nil {
		if cfg.RootCAs == nil {
			cfg.RootCAs = ca.pool
		} else {
			// A CA file is configured for this connection as well: trust both, so
			// that -tls.vaultTrustPKICA doesn't silently drop it.
			pool := cfg.RootCAs.Clone()
			if !pool.AppendCertsFromPEM(ca.pem) {
				return nil, fmt.Errorf("cannot parse any certificate from the vault pki CA bundle")
			}
			cfg.RootCAs = pool
		}
	}

	if rt.trPrev != nil {
		rt.trPrev.CloseIdleConnections()
	}
	tr := rt.trBase.Clone()
	tr.TLSClientConfig = cfg

	rt.trPrev = tr
	rt.tlsCfgPrev = tlsCfg
	rt.caPrev = ca
	return tr, nil
}
