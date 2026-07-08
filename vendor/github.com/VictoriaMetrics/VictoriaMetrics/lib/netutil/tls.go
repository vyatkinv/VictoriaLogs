package netutil

import (
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
)

// globalCertProvider, when non-nil, overrides file-based certificate loading
// in GetServerTLSConfig. Register before calling httpserver.Serve.
var globalCertProvider func(*tls.ClientHelloInfo) (*tls.Certificate, error)

// SetGlobalCertProvider registers a GetCertificate callback that takes precedence
// over the -tlsCertFile / -tlsKeyFile flags. Intended for Vault PKI integration.
// Must be called before any TLS listener is created.
func SetGlobalCertProvider(p func(*tls.ClientHelloInfo) (*tls.Certificate, error)) {
	globalCertProvider = p
}

// GetServerTLSConfig returns TLS config for the server.
// When a global cert provider has been registered via SetGlobalCertProvider,
// the provider is used instead of loading certificates from tlsCertFile/tlsKeyFile.
func GetServerTLSConfig(tlsCertFile, tlsKeyFile, tlsMinVersion string, tlsCipherSuites []string) (*tls.Config, error) {
	if globalCertProvider == nil {
		_, err := tls.LoadX509KeyPair(tlsCertFile, tlsKeyFile)
		if err != nil {
			return nil, fmt.Errorf("cannot load TLS certificate and key files: %w", err)
		}
	}

	minVersion, err := ParseTLSVersion(tlsMinVersion)
	if err != nil {
		return nil, fmt.Errorf("cannot use TLS min version from tlsMinVersion=%q. Supported TLS versions (TLS10, TLS11, TLS12, TLS13): %w", tlsMinVersion, err)
	}
	cipherSuites, err := cipherSuitesFromNames(tlsCipherSuites)
	if err != nil {
		return nil, fmt.Errorf("cannot use TLS cipher suites from tlsCipherSuites=%q: %w", tlsCipherSuites, err)
	}
	cfg := &tls.Config{
		MinVersion: minVersion,

		// Do not set MaxVersion, since this has no sense from security PoV.
		// This can only result in lower security level if improperly set.

		CipherSuites: cipherSuites,
	}

	if globalCertProvider != nil {
		cfg.GetCertificate = globalCertProvider
	} else {
		cfg.GetCertificate = newGetCertificateFunc(tlsCertFile, tlsKeyFile)
	}
	return cfg, nil
}

func newGetCertificateFunc(tlsCertFile, tlsKeyFile string) func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	var certLock sync.Mutex
	var certDeadline uint64
	var cert *tls.Certificate
	return func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
		certLock.Lock()
		defer certLock.Unlock()
		if fasttime.UnixTimestamp() > certDeadline {
			c, err := tls.LoadX509KeyPair(tlsCertFile, tlsKeyFile)
			if err != nil {
				return nil, fmt.Errorf("cannot load TLS cert from certFile=%q, keyFile=%q: %w", tlsCertFile, tlsKeyFile, err)
			}
			certDeadline = fasttime.UnixTimestamp() + 1
			cert = &c
		}
		return cert, nil
	}
}

func cipherSuitesFromNames(cipherSuiteNames []string) ([]uint16, error) {
	if len(cipherSuiteNames) == 0 {
		return nil, nil
	}

	css := tls.CipherSuites()

	cssByName := make(map[string]uint16, len(css))
	for _, cs := range css {
		cssByName[strings.ToLower(cs.Name)] = cs.ID
	}

	cssByID := make(map[uint16]bool, len(css))
	for _, cs := range css {
		cssByID[cs.ID] = true
	}

	cipherSuites := make([]uint16, 0, len(cipherSuiteNames))
	for _, name := range cipherSuiteNames {
		id, ok := cssByName[strings.ToLower(name)]
		if !ok {
			// Try searching by ID
			idKey, err := strconv.ParseUint(name, 0, 16)
			if err != nil || !cssByID[uint16(idKey)] {
				return nil, fmt.Errorf("unsupported TLS cipher suite name: %s", name)
			}
			id = uint16(idKey)
		}
		cipherSuites = append(cipherSuites, id)
	}
	return cipherSuites, nil
}

// ParseTLSVersion returns tls version from the given string s.
func ParseTLSVersion(s string) (uint16, error) {
	switch strings.ToUpper(s) {
	case "":
		// Special case - use default TLS version provided by tls package.
		return 0, nil
	case "TLS13":
		return tls.VersionTLS13, nil
	case "TLS12":
		return tls.VersionTLS12, nil
	case "TLS11":
		return tls.VersionTLS11, nil
	case "TLS10":
		return tls.VersionTLS10, nil
	default:
		return 0, fmt.Errorf("unsupported TLS version %q", s)
	}
}
