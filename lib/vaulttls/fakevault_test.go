package vaulttls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVault implements the subset of the Vault HTTP API used by this package:
// auth logins, response unwrapping, PKI issuance and both kinds of revocation.
// Every field is guarded by mu, since the provider may call it from its
// background renewer.
type fakeVault struct {
	srv *httptest.Server

	// The CA of the fake PKI mount. Every issued certificate is signed by it, so
	// tests can verify real handshakes against it.
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
	caPEM  []byte

	mu sync.Mutex

	// Recorded requests.
	logins         int
	loginPaths     []string
	loginBodies    []map[string]string
	unwraps        int
	unwrapTokens   []string
	issues         int
	issueTokens    []string
	serials        []string
	revokedSerials []string
	revokedTokens  []string
	caReads        []string
	caReadTokens   []string

	// Tunables.
	leaseDuration     int64         // lease of the token returned by login, in seconds; 0 means non-expiring
	certTTL           time.Duration // lifetime of issued certificates
	wrappedSecretID   string        // secret_id returned by sys/wrapping/unwrap
	unwrapStatus      int           // when non-zero, unwrap fails with this status code
	issueAuthFailures int           // number of leading issue requests answered with 403
	emptyCAChain      bool          // when set, cert/ca_chain returns an empty certificate, as for a root-only mount
	validTokens       map[string]bool
}

func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	fv := &fakeVault{
		leaseDuration: 3600,
		certTTL:       time.Hour,
		validTokens:   make(map[string]bool),
	}
	fv.mustInitCA(t)
	fv.srv = httptest.NewServer(fv)
	t.Cleanup(fv.srv.Close)
	return fv
}

// mustInitCA generates the root CA of the fake PKI mount.
func (fv *fakeVault) mustInitCA(t *testing.T) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("cannot generate the CA key: %s", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake vault root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cannot create the CA certificate: %s", err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("cannot parse the CA certificate: %s", err)
	}
	fv.caKey = key
	fv.caCert = caCert
	fv.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// caPool returns a pool holding the CA of the fake PKI mount.
func (fv *fakeVault) caPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(fv.caPEM)
	return pool
}

// mustIssueServerCert returns a certificate signed by the CA of the fake PKI
// mount, for use by a test TLS server.
func (fv *fakeVault) mustIssueServerCert(t *testing.T) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, _, err := fv.issueCert(time.Hour)
	if err != nil {
		t.Fatalf("cannot issue the server certificate: %s", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("cannot load the server key pair: %s", err)
	}
	return cert
}

func (fv *fakeVault) addr() string {
	return fv.srv.URL
}

// allowToken marks token as accepted by the PKI endpoints. Used for the static
// token auth method, where no login happens.
func (fv *fakeVault) allowToken(token string) {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	fv.validTokens[token] = true
}

func (fv *fakeVault) stats() (logins, unwraps, issues int) {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	return fv.logins, fv.unwraps, fv.issues
}

func (fv *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fv.mu.Lock()
	defer fv.mu.Unlock()

	body := make(map[string]string)
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err == nil && len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
	}
	token := r.Header.Get("X-Vault-Token")
	path := r.URL.Path

	switch {
	case strings.HasPrefix(path, "/v1/auth/") && strings.HasSuffix(path, "/login"):
		fv.logins++
		fv.loginPaths = append(fv.loginPaths, path)
		fv.loginBodies = append(fv.loginBodies, body)
		newToken := fmt.Sprintf("login-token-%d", fv.logins)
		fv.validTokens[newToken] = true
		writeVaultJSON(w, map[string]any{
			"auth": map[string]any{
				"client_token":   newToken,
				"accessor":       fmt.Sprintf("accessor-%d", fv.logins),
				"token_policies": []string{"default", "victoria-logs"},
				"lease_duration": fv.leaseDuration,
			},
		})
	case path == "/v1/sys/wrapping/unwrap":
		fv.unwraps++
		fv.unwrapTokens = append(fv.unwrapTokens, token)
		if fv.unwrapStatus != 0 {
			writeVaultError(w, fv.unwrapStatus, "wrapping token is not valid or does not exist")
			return
		}
		writeVaultJSON(w, map[string]any{
			"data": map[string]any{
				"secret_id": fv.wrappedSecretID,
			},
		})
	case path == "/v1/auth/token/revoke-self":
		if !fv.validTokens[token] {
			writeVaultError(w, http.StatusForbidden, "permission denied")
			return
		}
		fv.revokedTokens = append(fv.revokedTokens, token)
		delete(fv.validTokens, token)
		w.WriteHeader(http.StatusNoContent)
	case strings.Contains(path, "/issue/"):
		fv.issues++
		fv.issueTokens = append(fv.issueTokens, token)
		if fv.issueAuthFailures > 0 {
			fv.issueAuthFailures--
			writeVaultError(w, http.StatusForbidden, "permission denied")
			return
		}
		if !fv.validTokens[token] {
			writeVaultError(w, http.StatusForbidden, "permission denied")
			return
		}
		certPEM, keyPEM, serial, err := fv.issueCert(fv.certTTL)
		if err != nil {
			writeVaultError(w, http.StatusInternalServerError, err.Error())
			return
		}
		fv.serials = append(fv.serials, serial)
		writeVaultJSON(w, map[string]any{
			"data": map[string]any{
				"certificate":   string(certPEM),
				"private_key":   string(keyPEM),
				"serial_number": serial,
				"expiration":    time.Now().Add(fv.certTTL).Unix(),
			},
		})
	case strings.HasSuffix(path, "/cert/ca_chain"), strings.HasSuffix(path, "/cert/ca"):
		fv.caReads = append(fv.caReads, path)
		fv.caReadTokens = append(fv.caReadTokens, token)
		certificate := string(fv.caPEM)
		if fv.emptyCAChain && strings.HasSuffix(path, "/cert/ca_chain") {
			certificate = ""
		}
		writeVaultJSON(w, map[string]any{
			"data": map[string]any{
				"certificate": certificate,
			},
		})
	case strings.HasSuffix(path, "/revoke"):
		if !fv.validTokens[token] {
			writeVaultError(w, http.StatusForbidden, "permission denied")
			return
		}
		fv.revokedSerials = append(fv.revokedSerials, body["serial_number"])
		writeVaultJSON(w, map[string]any{
			"data": map[string]any{
				"revocation_time": time.Now().Unix(),
			},
		})
	default:
		writeVaultError(w, http.StatusNotFound, "unsupported path "+path)
	}
}

func writeVaultJSON(w http.ResponseWriter, resp map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeVaultError(w http.ResponseWriter, statusCode int, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []string{errMsg},
	})
}

// issueCert returns a certificate in the same shape as the one returned by the
// Vault PKI engine: signed by the CA of the mount and usable for both server and
// client authentication, as a role with server_flag=true and client_flag=true.
func (fv *fakeVault) issueCert(ttl time.Duration) (certPEM, keyPEM []byte, serial string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("cannot generate key: %w", err)
	}
	sn, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, "", fmt.Errorf("cannot generate serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(ttl),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, fv.caCert, &key.PublicKey, fv.caKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("cannot create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("cannot marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, formatSerial(sn), nil
}

// formatSerial renders a serial number the way Vault does: lowercase hex octets
// separated by colons.
func formatSerial(sn *big.Int) string {
	b := sn.Bytes()
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02x", v)
	}
	return strings.Join(parts, ":")
}
