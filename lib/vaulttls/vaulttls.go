package vaulttls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httputil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/netutil"
)

// registered holds the active Provider so listeners can obtain an in-memory
// GetCertificate without files. It is set once at startup, before any listener
// starts, via Register.
var registered atomic.Pointer[Provider]

// Register publishes p as the process-wide Vault certificate provider.
func Register(p *Provider) {
	registered.Store(p)
}

// ServerTLSConfig returns a *tls.Config that serves the registered Vault
// certificate via an in-memory GetCertificate callback, or (nil, nil) if no
// Vault provider is registered. It mirrors netutil.GetServerTLSConfig but never
// reads certificate files from disk, so the private key stays in memory.
//
// Used by both TLS listeners: syslog calls it directly, and the HTTP listener
// receives it as httpserver.ServeOptions.GetTLSConfig — the signature matches
// that field deliberately. Returning (nil, nil) lets the caller fall back to
// its file-based configuration.
func ServerTLSConfig(tlsMinVersion string, tlsCipherSuites []string) (*tls.Config, error) {
	p := registered.Load()
	if p == nil {
		return nil, nil
	}
	minVersion, err := netutil.ParseTLSVersion(tlsMinVersion)
	if err != nil {
		return nil, fmt.Errorf("cannot parse TLS min version %q: %w", tlsMinVersion, err)
	}
	cipherSuites, err := cipherSuitesFromNames(tlsCipherSuites)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     minVersion,
		CipherSuites:   cipherSuites,
		GetCertificate: p.GetCertificate,
	}, nil
}

// cipherSuitesFromNames resolves TLS cipher suite names (or numeric IDs) to
// their uint16 identifiers. Mirrors the unexported helper in lib/netutil.
func cipherSuitesFromNames(cipherSuiteNames []string) ([]uint16, error) {
	if len(cipherSuiteNames) == 0 {
		return nil, nil
	}
	css := tls.CipherSuites()
	byName := make(map[string]uint16, len(css))
	byID := make(map[uint16]bool, len(css))
	for _, cs := range css {
		byName[strings.ToLower(cs.Name)] = cs.ID
		byID[cs.ID] = true
	}
	cipherSuites := make([]uint16, 0, len(cipherSuiteNames))
	for _, name := range cipherSuiteNames {
		id, ok := byName[strings.ToLower(name)]
		if !ok {
			idKey, err := strconv.ParseUint(name, 0, 16)
			if err != nil || !byID[uint16(idKey)] {
				return nil, fmt.Errorf("unsupported TLS cipher suite name: %s", name)
			}
			id = uint16(idKey)
		}
		cipherSuites = append(cipherSuites, id)
	}
	return cipherSuites, nil
}

// Config holds parameters for the Vault PKI certificate provider.
type Config struct {
	// Addr is the Vault server address, e.g. "https://vault:8200".
	Addr string
	// Auth describes how to authenticate in Vault. See AuthConfig.
	Auth AuthConfig
	// PKIPath is the Vault PKI secrets engine mount path, e.g. "pki".
	PKIPath string
	// Role is the Vault PKI role name used for certificate issuance.
	Role string
	// CommonName is the Common Name embedded in issued certificates.
	CommonName string
	// TTL is the requested certificate lifetime, e.g. "24h" or "30m".
	// Vault may enforce a lower maximum from the role configuration.
	TTL string
	// AltNames is a comma-separated list of Subject Alternative Names.
	AltNames string
	// RenewBefore is how early before expiration to renew the certificate.
	// Defaults to 1/3 of the actual certificate lifetime when zero.
	RenewBefore time.Duration

	// CAFile is a path to a PEM file with CAs trusted for the connection to
	// Vault, in addition to the system pool. Without it a MITM Vault would simply
	// collect the credentials sent to it.
	CAFile string
	// ServerName overrides the hostname verified in the Vault certificate.
	ServerName string
	// InsecureSkipVerify disables verification of the Vault certificate.
	InsecureSkipVerify bool

	// RevokeOnShutdown makes the provider revoke the issued certificate via
	// pki/revoke when the process stops, so a leaked private key stops being
	// usable before the certificate expires.
	RevokeOnShutdown bool

	// ClientAuth makes outgoing connections present the Vault-issued certificate
	// as a client certificate. The PKI role must then be created with
	// client_flag=true, otherwise the certificate carries no clientAuth extended
	// key usage and the peer rejects it. See NewRoundTripper.
	ClientAuth bool
	// TrustPKICA makes outgoing connections verify the peer against the CA of the
	// configured PKI mount, in addition to the system pool, so that no CA file has
	// to be distributed to the nodes. See NewRoundTripper.
	TrustPKICA bool
}

// Provider fetches TLS certificates from a Vault PKI secrets engine and
// proactively renews them before expiration.
//
// The certificate never touches the filesystem: it is held in memory and served
// through GetCertificate. Both kinds of TLS listener reach it that way — syslog
// builds its tls.Config directly via ServerTLSConfig, and the HTTP listener gets
// the same config through httpserver.ServeOptions.GetTLSConfig.
type Provider struct {
	cfg    Config
	client *http.Client
	auth   *tokenSource

	// ca holds the CA bundle of the PKI mount when Config.TrustPKICA is set.
	// It is read on every outgoing connection, hence the atomic pointer.
	ca atomic.Pointer[caBundle]

	mu       sync.Mutex
	cert     *tls.Certificate // in-memory copy, served via GetCertificate
	serial   string           // serial of cert as reported by Vault, used for revocation
	expiry   time.Time
	issuedAt time.Time // used to compute full certificate lifetime for renewDeadline

	stopCh   chan struct{}
	stopOnce sync.Once
}

// issuedCert is a single certificate issuance result.
type issuedCert struct {
	cert   *tls.Certificate
	serial string
	expiry time.Time
}

// NewProvider creates a Provider and issues the first certificate from Vault.
// The certificate is kept in memory only. A background goroutine renews it
// proactively; call Stop to shut that goroutine down.
func NewProvider(cfg Config) (*Provider, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("vault addr must not be empty")
	}
	if cfg.PKIPath == "" {
		return nil, fmt.Errorf("vault PKI path must not be empty")
	}
	if cfg.Role == "" {
		return nil, fmt.Errorf("vault role must not be empty")
	}
	if cfg.CommonName == "" {
		return nil, fmt.Errorf("vault common name must not be empty")
	}
	if cfg.TTL == "" {
		cfg.TTL = "24h"
	}
	if strings.HasPrefix(strings.ToLower(cfg.Addr), "http://") {
		logger.Warnf("vaulttls: -tls.vaultAddr=%q uses plain HTTP, so the vault credentials and the issued private key "+
			"are transmitted in cleartext; use https:// outside of test setups", cfg.Addr)
	}

	client, err := newVaultClient(cfg)
	if err != nil {
		return nil, err
	}
	auth, err := newTokenSource(cfg.Addr, client, cfg.Auth)
	if err != nil {
		return nil, err
	}

	p := &Provider{
		cfg:    cfg,
		client: client,
		auth:   auth,
		stopCh: make(chan struct{}),
	}
	if cfg.TrustPKICA {
		// A missing CA bundle is a configuration error rather than a transient
		// failure, so fail loudly instead of leaving outgoing connections without
		// the CA they are supposed to verify against.
		if err := p.refreshCA(); err != nil {
			return nil, err
		}
	}
	if err := p.renew(); err != nil {
		return nil, fmt.Errorf("cannot issue initial certificate from Vault PKI at %s: %w", cfg.Addr, err)
	}
	go p.backgroundRenewer()
	return p, nil
}

// newVaultClient builds the HTTP client used for every call to Vault.
func newVaultClient(cfg Config) (*http.Client, error) {
	tr := httputil.NewTransport(false, "vl_vaulttls_client")
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tlsCfg := tr.TLSClientConfig
	tlsCfg.MinVersion = tls.VersionTLS12
	tlsCfg.ServerName = cfg.ServerName
	tlsCfg.InsecureSkipVerify = cfg.InsecureSkipVerify
	if cfg.InsecureSkipVerify {
		logger.Warnf("vaulttls: -tls.vaultInsecureSkipVerify is set, so the vault certificate isn't verified; " +
			"credentials sent to vault can be intercepted by a man-in-the-middle")
	}
	if cfg.CAFile != "" {
		rootCAs, err := x509.SystemCertPool()
		if err != nil {
			// Not fatal: continue with a pool holding just the configured CA.
			logger.Warnf("vaulttls: cannot load the system certificate pool: %s; trusting only -tls.vaultCAFile=%q", err, cfg.CAFile)
			rootCAs = x509.NewCertPool()
		}
		data, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read -tls.vaultCAFile=%q: %w", cfg.CAFile, err)
		}
		if !rootCAs.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("cannot parse any certificate from -tls.vaultCAFile=%q", cfg.CAFile)
		}
		tlsCfg.RootCAs = rootCAs
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: tr,
	}, nil
}

// Stop shuts down the background renewal goroutine and releases the Vault
// credentials held by p: the issued certificate is revoked when
// Config.RevokeOnShutdown is set, and the login token is always revoked so it
// cannot be replayed after the process exits.
func (p *Provider) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
	if p.cfg.RevokeOnShutdown {
		p.revokeCert()
	}
	p.auth.revoke()
}

// GetCertificate implements tls.Config.GetCertificate, serving the current
// in-memory certificate. Used by listeners whose tls.Config we build ourselves
// (syslog), so their private key never touches disk. The background renewer
// swaps p.cert under the mutex, so callers always get a fresh certificate.
func (p *Provider) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cert == nil {
		return nil, fmt.Errorf("vaulttls: no certificate available")
	}
	return p.cert, nil
}

// Expiry returns the expiration time of the currently active certificate.
func (p *Provider) Expiry() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.expiry
}

func (p *Provider) renewDeadline() time.Time {
	renewBefore := p.cfg.RenewBefore
	if renewBefore <= 0 {
		// Use the full certificate lifetime (expiry − issuedAt) so the
		// deadline stays stable and doesn't oscillate as time passes.
		lifetime := p.expiry.Sub(p.issuedAt)
		if lifetime <= 0 {
			return p.expiry
		}
		renewBefore = lifetime / 3
	}
	return p.expiry.Add(-renewBefore)
}

func (p *Provider) backgroundRenewer() {
	for {
		p.mu.Lock()
		renewAt := p.renewDeadline()
		p.mu.Unlock()

		sleepDur := time.Until(renewAt)
		if sleepDur < time.Second {
			// Guard against a hot loop when the deadline is already in the past.
			sleepDur = time.Second
		}
		select {
		case <-time.After(sleepDur):
		case <-p.stopCh:
			return
		}

		if err := p.renew(); err != nil {
			logger.Warnf("vaulttls: background renewal failed: %s; will retry in 10s", err)
			select {
			case <-time.After(10 * time.Second):
			case <-p.stopCh:
				return
			}
			continue
		}
		if p.cfg.TrustPKICA {
			// Picks up a rotated PKI CA. Best-effort: the previously fetched bundle
			// stays in use on failure, and it is still valid until the CA rotates.
			if err := p.refreshCA(); err != nil {
				logger.Warnf("vaulttls: cannot refresh the PKI CA bundle: %s; keeping the previously fetched one", err)
			}
		}
	}
}

// renew issues a new certificate and publishes it. The Vault round-trips happen
// outside p.mu, so TLS handshakes served from the current certificate aren't
// blocked for the duration of the request.
func (p *Provider) renew() error {
	ic, err := p.fetchCert()
	if err != nil {
		return err
	}
	now := time.Now()
	logger.Infof("vaulttls: issued certificate from %s for CN=%q, serial %s, expires %s (in %s)",
		p.cfg.Addr, p.cfg.CommonName, ic.serial, ic.expiry.Format(time.RFC3339), ic.expiry.Sub(now).Round(time.Second))

	p.mu.Lock()
	p.cert = ic.cert
	p.serial = ic.serial
	p.expiry = ic.expiry
	p.issuedAt = now
	p.mu.Unlock()
	return nil
}

// fetchCert authenticates in Vault and issues a certificate. It must be called
// without holding p.mu.
func (p *Provider) fetchCert() (*issuedCert, error) {
	token, err := p.auth.get(false)
	if err != nil {
		return nil, err
	}
	ic, statusCode, err := p.issueCert(token)
	if err == nil {
		return ic, nil
	}
	// p.auth.cfg.Method is the normalized method; p.cfg.Auth.Method may still be empty.
	if p.auth.cfg.Method == AuthMethodToken || (statusCode != http.StatusUnauthorized && statusCode != http.StatusForbidden) {
		return nil, err
	}
	// The cached token was revoked or expired ahead of its lease. Log in again and
	// retry once. Pointless for a static token, which cannot be replaced by us.
	logger.Warnf("vaulttls: vault rejected the cached token with HTTP %d; re-authenticating", statusCode)
	token, err = p.auth.get(true)
	if err != nil {
		return nil, err
	}
	ic, _, err = p.issueCert(token)
	return ic, err
}

// issueCert requests a certificate from the PKI engine. The HTTP status code is
// returned alongside the error so the caller can detect authentication failures.
func (p *Provider) issueCert(token string) (*issuedCert, int, error) {
	issueURL := vaultURL(p.cfg.Addr, strings.Trim(p.cfg.PKIPath, "/")+"/issue/"+p.cfg.Role)
	reqBody := map[string]string{
		"common_name": p.cfg.CommonName,
		"ttl":         p.cfg.TTL,
	}
	if p.cfg.AltNames != "" {
		reqBody["alt_names"] = p.cfg.AltNames
	}
	respBytes, statusCode, err := doVaultRequest(p.client, http.MethodPost, issueURL, token, reqBody)
	if err != nil {
		return nil, statusCode, err
	}

	var vaultResp struct {
		Data struct {
			Certificate  string `json:"certificate"`
			PrivateKey   string `json:"private_key"`
			SerialNumber string `json:"serial_number"`
			Expiration   int64  `json:"expiration"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &vaultResp); err != nil {
		return nil, statusCode, fmt.Errorf("cannot parse the vault response from %s: %w", issueURL, err)
	}
	if vaultResp.Data.Certificate == "" || vaultResp.Data.PrivateKey == "" {
		return nil, statusCode, fmt.Errorf("the vault response from %s contains an empty certificate or private_key", issueURL)
	}

	// Parse (and validate) the key pair before publishing it, so a bad issuance
	// never replaces a currently valid certificate.
	cert, err := tls.X509KeyPair([]byte(vaultResp.Data.Certificate), []byte(vaultResp.Data.PrivateKey))
	if err != nil {
		return nil, statusCode, fmt.Errorf("cannot load the vault-issued key pair: %w", err)
	}
	return &issuedCert{
		cert:   &cert,
		serial: vaultResp.Data.SerialNumber,
		expiry: time.Unix(vaultResp.Data.Expiration, 0),
	}, statusCode, nil
}

// revokeCert revokes the currently held certificate in Vault. Best-effort: a
// failure only shortens the window during which the key stays usable, so it is
// logged rather than propagated.
func (p *Provider) revokeCert() {
	p.mu.Lock()
	serial := p.serial
	p.mu.Unlock()
	if serial == "" {
		return
	}
	token, err := p.auth.get(false)
	if err != nil {
		logger.Warnf("vaulttls: cannot revoke certificate %s: %s", serial, err)
		return
	}
	revokeURL := vaultURL(p.cfg.Addr, strings.Trim(p.cfg.PKIPath, "/")+"/revoke")
	reqBody := map[string]string{
		"serial_number": serial,
	}
	if _, _, err := doVaultRequest(p.client, http.MethodPost, revokeURL, token, reqBody); err != nil {
		logger.Warnf("vaulttls: cannot revoke certificate %s: %s; it stays valid until %s",
			serial, err, p.Expiry().Format(time.RFC3339))
		return
	}
	logger.Infof("vaulttls: revoked certificate %s in vault", serial)
}
