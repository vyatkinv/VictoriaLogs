package vaulttls

import (
	"bytes"
	"crypto/tls"
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

// Config holds parameters for the Vault PKI certificate provider.
type Config struct {
	// Addr is the Vault server address, e.g. "https://vault:8200".
	Addr string
	// Token is a static Vault authentication token.
	// Mutually exclusive with TokenFile.
	Token string
	// TokenFile is a path to a file containing the Vault token.
	// Re-read on every renewal to support token rotation.
	TokenFile string
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
}

// Provider fetches TLS certificates from a Vault PKI secrets engine
// and proactively renews them before expiration.
type Provider struct {
	cfg    Config
	client *http.Client

	mu       sync.Mutex
	cert     *tls.Certificate
	expiry   time.Time
	issuedAt time.Time // used to compute full certificate lifetime for renewDeadline

	stopCh chan struct{}
}

// NewProvider creates a Provider and issues the first certificate from Vault.
// A background goroutine is started to renew the certificate proactively.
// Call Stop to shut down the background goroutine.
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

	p := &Provider{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		stopCh: make(chan struct{}),
	}
	if err := p.renew(); err != nil {
		return nil, fmt.Errorf("cannot issue initial certificate from Vault PKI at %s: %w", cfg.Addr, err)
	}
	go p.backgroundRenewer()
	return p, nil
}

// Stop shuts down the background renewal goroutine.
func (p *Provider) Stop() {
	close(p.stopCh)
}

// GetCertificate implements the tls.Config.GetCertificate callback.
// Returns the current certificate, renewing lazily if the background
// goroutine hasn't fired yet (e.g., right at the renewal boundary).
func (p *Provider) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if time.Now().After(p.renewDeadline()) {
		if err := p.renew(); err != nil {
			logger.Warnf("vaulttls: lazy renewal failed: %v; using existing cert (expires %s)",
				err, p.expiry.Format(time.RFC3339))
		}
	}
	if time.Now().After(p.expiry) {
		return nil, fmt.Errorf("vaulttls: certificate expired at %s and renewal failed", p.expiry.Format(time.RFC3339))
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
		if sleepDur > 0 {
			select {
			case <-time.After(sleepDur):
			case <-p.stopCh:
				return
			}
		}

		p.mu.Lock()
		// Re-check after sleep; another goroutine (GetCertificate) may have
		// already renewed between our sleep and acquiring the lock.
		if time.Now().After(p.renewDeadline()) {
			if err := p.renew(); err != nil {
				logger.Warnf("vaulttls: background renewal failed: %v; will retry in 10s", err)
				p.mu.Unlock()
				select {
				case <-time.After(10 * time.Second):
				case <-p.stopCh:
					return
				}
				continue
			}
		}
		p.mu.Unlock()
	}
}

func (p *Provider) token() (string, error) {
	if p.cfg.TokenFile != "" {
		data, err := os.ReadFile(p.cfg.TokenFile)
		if err != nil {
			return "", fmt.Errorf("cannot read vault token file %q: %w", p.cfg.TokenFile, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if p.cfg.Token == "" {
		return "", fmt.Errorf("vault token not configured; set -tls.vaultToken or -tls.vaultTokenFile")
	}
	return p.cfg.Token, nil
}

func (p *Provider) renew() error {
	token, err := p.token()
	if err != nil {
		return err
	}

	issueURL := fmt.Sprintf("%s/v1/%s/issue/%s",
		strings.TrimRight(p.cfg.Addr, "/"),
		strings.Trim(p.cfg.PKIPath, "/"),
		p.cfg.Role,
	)

	reqBody := map[string]string{
		"common_name": p.cfg.CommonName,
		"ttl":         p.cfg.TTL,
	}
	if p.cfg.AltNames != "" {
		reqBody["alt_names"] = p.cfg.AltNames
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("cannot marshal vault request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, issueURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("cannot create request to %s: %w", issueURL, err)
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot contact vault at %s: %w", issueURL, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("cannot read vault response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Include first 512 bytes of the error body for diagnostics.
		excerpt := string(respBytes)
		if len(excerpt) > 512 {
			excerpt = excerpt[:512]
		}
		return fmt.Errorf("vault returned HTTP %d: %s", resp.StatusCode, excerpt)
	}

	var vaultResp struct {
		Data struct {
			Certificate string `json:"certificate"`
			PrivateKey  string `json:"private_key"`
			Expiration  int64  `json:"expiration"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &vaultResp); err != nil {
		return fmt.Errorf("cannot parse vault response: %w", err)
	}
	if vaultResp.Data.Certificate == "" || vaultResp.Data.PrivateKey == "" {
		return fmt.Errorf("vault response contains empty certificate or private_key")
	}

	cert, err := tls.X509KeyPair([]byte(vaultResp.Data.Certificate), []byte(vaultResp.Data.PrivateKey))
	if err != nil {
		return fmt.Errorf("cannot load vault-issued key pair: %w", err)
	}

	now := time.Now()
	expiry := time.Unix(vaultResp.Data.Expiration, 0)
	logger.Infof("vaulttls: issued certificate from %s for CN=%q, expires %s (in %s)",
		p.cfg.Addr, p.cfg.CommonName, expiry.Format(time.RFC3339), expiry.Sub(now).Round(time.Second))

	p.cert = &cert
	p.expiry = expiry
	p.issuedAt = now
	return nil
}
