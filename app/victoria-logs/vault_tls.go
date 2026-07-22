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

	// Vault serves the HTTP certificate from memory, so -tlsCertFile/-tlsKeyFile
	// are never consulted. Reject them explicitly instead of ignoring them
	// silently, so the user is not left believing their files are in use.
	if isFlagSet("tlsCertFile") || isFlagSet("tlsKeyFile") {
		logger.Fatalf("-tlsCertFile/-tlsKeyFile must not be set together with -tls.vaultAddr; " +
			"Vault PKI serves the HTTP certificate from memory")
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

	// Publish the provider so listeners can obtain an in-memory tls.Config via
	// vaulttls.ServerTLSConfig: syslog calls it directly, and the HTTP listener
	// reaches it through httpserver.ServeOptions.GetTLSConfig (see main).
	vaulttls.Register(p)

	// -tls only selects the https scheme for -httpListenAddr; the certificate
	// itself comes from the provider above, not from any file.
	setFlagOrFatal("tls", "true")

	// syslog builds its own tls.Config and, when -syslog.tls is enabled, prefers
	// the in-memory Vault provider. Reject conflicting explicit cert files so the
	// user isn't surprised that Vault silently wins over them.
	if syslogTLSEnabled() {
		if isFlagSet("syslog.tlsCertFile") || isFlagSet("syslog.tlsKeyFile") {
			logger.Fatalf("-syslog.tlsCertFile/-syslog.tlsKeyFile must not be set together with -tls.vaultAddr; " +
				"Vault PKI serves the syslog certificate from memory")
		}
		logger.Infof("Vault PKI TLS also serves -syslog.listenAddr.tcp (via -syslog.tls) from memory")
	}

	logger.Infof("Vault PKI TLS provider ready; certificate expires at %s (renews ~1/3 before expiry)",
		p.Expiry().Format(time.RFC3339))
}

// syslogTLSEnabled reports whether -syslog.tls is enabled for at least one
// syslog TCP listener.
func syslogTLSEnabled() bool {
	f := flag.Lookup("syslog.tls")
	if f == nil {
		return false
	}
	ab, ok := f.Value.(*flagutil.ArrayBool)
	if !ok {
		return false
	}
	for _, v := range *ab {
		if v {
			return true
		}
	}
	return false
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
