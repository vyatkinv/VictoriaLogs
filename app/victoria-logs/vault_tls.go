package main

import (
	"flag"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"

	"github.com/VictoriaMetrics/VictoriaLogs/lib/vaulttls"
)

var (
	tlsVaultAddr = flag.String("tls.vaultAddr", "",
		"Vault server address for PKI certificate issuance, e.g. https://vault:8200. "+
			"When set, certificates are fetched from Vault PKI instead of -tlsCertFile/-tlsKeyFile. "+
			"Requires -tls=true. See also -tls.vaultPKIPath, -tls.vaultRole, -tls.vaultCommonName.")

	tlsVaultToken = flagutil.NewPassword("tls.vaultToken",
		"Vault authentication token for PKI certificate issuance. "+
			"Mutually exclusive with -tls.vaultTokenFile.")

	tlsVaultTokenFile = flag.String("tls.vaultTokenFile", "",
		"Path to a file containing the Vault authentication token. "+
			"The file is re-read on every certificate renewal to support token rotation. "+
			"Mutually exclusive with -tls.vaultToken.")

	tlsVaultPKIPath = flag.String("tls.vaultPKIPath", "pki",
		"Vault PKI secrets engine mount path. Used when -tls.vaultAddr is set.")

	tlsVaultRole = flag.String("tls.vaultRole", "",
		"Vault PKI role name for certificate issuance. Used when -tls.vaultAddr is set.")

	tlsVaultCommonName = flag.String("tls.vaultCommonName", "",
		"Common Name (CN) for Vault-issued certificates. Used when -tls.vaultAddr is set.")

	tlsVaultAltNames = flag.String("tls.vaultAltNames", "",
		"Comma-separated Subject Alternative Names for Vault-issued certificates. "+
			"Used when -tls.vaultAddr is set.")

	tlsVaultTTL = flag.String("tls.vaultTTL", "24h",
		"Requested TTL for Vault-issued certificates, e.g. 24h or 30m. "+
			"Vault may enforce a lower maximum from the role configuration. "+
			"Used when -tls.vaultAddr is set.")

	tlsVaultRenewBefore = flag.Duration("tls.vaultRenewBefore", 0,
		"How early before expiration to renew the Vault-issued certificate. "+
			"Defaults to 1/3 of the certificate lifetime when zero. "+
			"Used when -tls.vaultAddr is set.")
)

// vaultTLSProvider is the active Vault cert provider, stored so it can be stopped on shutdown.
var vaultTLSProvider *vaulttls.Provider

// initVaultTLS initialises the Vault PKI certificate provider if -tls.vaultAddr is set.
// Must be called after flag parsing and before httpserver.Serve.
func initVaultTLS() {
	if *tlsVaultAddr == "" {
		return
	}

	// Vault manages -tlsCertFile/-tlsKeyFile itself. If the user also set them
	// explicitly, our value would be appended after theirs in the array flag and
	// never take effect for -httpListenAddr index 0, silently ignoring Vault.
	if isFlagSet("tlsCertFile") || isFlagSet("tlsKeyFile") {
		logger.Fatalf("-tlsCertFile/-tlsKeyFile must not be set together with -tls.vaultAddr; " +
			"Vault PKI manages the certificate files itself")
	}

	token := tlsVaultToken.Get()
	cfg := vaulttls.Config{
		Addr:        *tlsVaultAddr,
		Token:       token,
		TokenFile:   *tlsVaultTokenFile,
		PKIPath:     *tlsVaultPKIPath,
		Role:        *tlsVaultRole,
		CommonName:  *tlsVaultCommonName,
		AltNames:    *tlsVaultAltNames,
		TTL:         *tlsVaultTTL,
		RenewBefore: *tlsVaultRenewBefore,
	}

	logger.Infof("initialising Vault PKI TLS provider: addr=%s, pki=%s, role=%s, cn=%s, ttl=%s",
		cfg.Addr, cfg.PKIPath, cfg.Role, cfg.CommonName, cfg.TTL)

	p, err := vaulttls.NewProvider(cfg)
	if err != nil {
		logger.Fatalf("cannot initialise Vault PKI TLS provider: %s", err)
	}
	vaultTLSProvider = p

	// Wire the Vault-managed PEM files into the standard file-based TLS path.
	// httpserver/syslog re-read these files ~once per second, so proactive
	// renewals (which rewrite the files) are picked up automatically without
	// patching any vendored code.
	setFlagOrFatal("tls", "true")
	setFlagOrFatal("tlsCertFile", p.CertFile())
	setFlagOrFatal("tlsKeyFile", p.KeyFile())

	logger.Infof("Vault PKI TLS provider ready; certificate expires at %s (renews ~1/3 before expiry)",
		p.Expiry().Format(time.RFC3339))
}

// setFlagOrFatal sets a command-line flag programmatically, aborting on error.
func setFlagOrFatal(name, value string) {
	if err := flag.Set(name, value); err != nil {
		logger.Fatalf("cannot set -%s=%q for Vault PKI TLS: %s", name, value, err)
	}
}

// isFlagSet reports whether the named flag was explicitly set on the command line.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// stopVaultTLS stops the background renewal goroutine if Vault TLS was used.
func stopVaultTLS() {
	if vaultTLSProvider != nil {
		vaultTLSProvider.Stop()
	}
}
