// Package vaultflags registers the -tls.vault* command-line flags and wires the
// Vault PKI certificate provider into the process.
//
// It is imported by the binaries that can serve or establish TLS connections with
// a Vault-issued certificate — victoria-logs and vlagent — so that both expose the
// same flag surface and the same startup checks.
package vaultflags

import (
	"flag"
	"os"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"

	"github.com/VictoriaMetrics/VictoriaLogs/lib/vaulttls"
)

var (
	tlsVaultAddr = flag.String("tls.vaultAddr", "",
		"Vault server address for PKI certificate issuance, e.g. https://vault:8200. "+
			"When set, certificates are fetched from Vault PKI instead of -tlsCertFile/-tlsKeyFile. "+
			"Enables -tls unless it is set explicitly. See also -tls.vaultPKIPath, -tls.vaultRole, -tls.vaultCommonName.")

	tlsVaultToken = flagutil.NewPassword("tls.vaultToken",
		"Vault authentication token for PKI certificate issuance. Used with -tls.vaultAuthMethod=token. "+
			"Mutually exclusive with -tls.vaultTokenFile.")

	tlsVaultTokenFile = flag.String("tls.vaultTokenFile", "",
		"Path to a file containing the Vault authentication token. "+
			"The file is re-read on every certificate renewal to support token rotation. "+
			"Mutually exclusive with -tls.vaultToken.")

	tlsVaultPKIPath = flag.String("tls.vaultPKIPath", "pki",
		"Vault PKI secrets engine mount path. Used when -tls.vaultAddr is set.")

	tlsVaultRole = flag.String("tls.vaultRole", "",
		"Vault PKI role name for certificate issuance. Used when -tls.vaultAddr is set. "+
			"This is the PKI role; the role of the auth method is -tls.vaultAuthRole.")

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

	tlsVaultAuthMethod = flag.String("tls.vaultAuthMethod", "token",
		"Vault auth method used to obtain a token: token, approle or kubernetes. "+
			"Prefer kubernetes where available: its service account token is short-lived and rotated by kubelet, "+
			"so no long-lived credential is stored anywhere.")

	tlsVaultAuthMount = flag.String("tls.vaultAuthMount", "",
		"Mount path of the Vault auth method. Defaults to the value of -tls.vaultAuthMethod.")

	tlsVaultAuthRoleID = flag.String("tls.vaultAuthRoleID", "",
		"AppRole role_id for -tls.vaultAuthMethod=approle. Mutually exclusive with -tls.vaultAuthRoleIDFile.")

	tlsVaultAuthRoleIDFile = flag.String("tls.vaultAuthRoleIDFile", "",
		"Path to a file with the AppRole role_id for -tls.vaultAuthMethod=approle. "+
			"Mutually exclusive with -tls.vaultAuthRoleID.")

	tlsVaultAuthSecretID = flagutil.NewPassword("tls.vaultAuthSecretID",
		"AppRole secret_id for -tls.vaultAuthMethod=approle. Prefer -tls.vaultAuthSecretIDWrappedFile, "+
			"or at least a file reference: a value passed on the command line is exposed via /proc/<pid>/cmdline. "+
			"Mutually exclusive with -tls.vaultAuthSecretIDFile and -tls.vaultAuthSecretIDWrappedFile.")

	tlsVaultAuthSecretIDFile = flag.String("tls.vaultAuthSecretIDFile", "",
		"Path to a file with the AppRole secret_id for -tls.vaultAuthMethod=approle. "+
			"Mutually exclusive with -tls.vaultAuthSecretID and -tls.vaultAuthSecretIDWrappedFile.")

	tlsVaultAuthSecretIDWrappedFile = flag.String("tls.vaultAuthSecretIDWrappedFile", "",
		"Path to a file with a response-wrapping token holding the AppRole secret_id. The token is unwrapped at startup, "+
			"so the secret_id itself never touches the filesystem. A wrapping token is single-use: a fresh one must be "+
			"delivered on every start, and a failed unwrap means the credential may have been intercepted. "+
			"Mutually exclusive with -tls.vaultAuthSecretID and -tls.vaultAuthSecretIDFile.")

	tlsVaultAuthRole = flag.String("tls.vaultAuthRole", "",
		"Vault role of the auth method for -tls.vaultAuthMethod=kubernetes. "+
			"This is not the PKI role, which is -tls.vaultRole.")

	tlsVaultAuthJWTFile = flag.String("tls.vaultAuthJWTFile", vaulttls.DefaultKubernetesJWTFile,
		"Path to the service account token presented to Vault for -tls.vaultAuthMethod=kubernetes. "+
			"It is re-read on every login, so a rotated projected token is picked up.")

	tlsVaultCAFile = flag.String("tls.vaultCAFile", "",
		"Path to a PEM file with CAs trusted for the connection to Vault, in addition to the system pool. "+
			"Without it a man-in-the-middle Vault would collect the credentials sent to it.")

	tlsVaultServerName = flag.String("tls.vaultServerName", "",
		"Hostname to verify in the Vault server certificate, when it differs from the one in -tls.vaultAddr.")

	tlsVaultInsecureSkipVerify = flag.Bool("tls.vaultInsecureSkipVerify", false,
		"Whether to skip verification of the Vault server certificate. Do not enable outside of test setups: "+
			"credentials sent to Vault can then be intercepted by a man-in-the-middle.")

	tlsVaultRevokeOnShutdown = flag.Bool("tls.vaultRevokeOnShutdown", false,
		"Whether to revoke the issued certificate via pki/revoke when the process stops. "+
			"Requires the update capability on <pki>/revoke in the Vault policy.")

	tlsVaultClientAuth = flag.Bool("tls.vaultClientAuth", false,
		"Whether to present the Vault-issued certificate as a client certificate on outgoing TLS connections "+
			"to -storageNode and -remoteWrite.url. The PKI role must be created with client_flag=true. "+
			"Mutually exclusive with the corresponding -storageNode.tlsCertFile and -remoteWrite.tlsCertFile.")

	tlsVaultTrustPKICA = flag.Bool("tls.vaultTrustPKICA", false,
		"Whether to verify outgoing TLS connections to -storageNode and -remoteWrite.url against the CA of "+
			"-tls.vaultPKIPath, in addition to the system pool and to the corresponding -storageNode.tlsCAFile "+
			"and -remoteWrite.tlsCAFile. Removes the need to distribute the CA file to every node.")
)

// provider is the active Vault cert provider, stored so it can be stopped on shutdown.
var provider *vaulttls.Provider

// Init initialises the Vault PKI certificate provider if -tls.vaultAddr is set,
// and does nothing otherwise.
//
// It must be called after flag parsing and before anything that establishes TLS
// connections: the HTTP server, the syslog TCP listener and the clients of
// -storageNode and -remoteWrite.url all resolve their certificate through the
// provider registered here.
func Init() {
	if *tlsVaultAddr == "" {
		return
	}

	// Vault serves the certificate from memory, so -tlsCertFile/-tlsKeyFile are
	// never consulted. Reject them explicitly instead of ignoring them silently,
	// so the user is not left believing their files are in use.
	if isFlagSet("tlsCertFile") || isFlagSet("tlsKeyFile") {
		logger.Fatalf("-tlsCertFile/-tlsKeyFile must not be set together with -tls.vaultAddr; " +
			"Vault PKI serves the HTTP certificate from memory")
	}
	checkVaultAuthFlags()
	checkVaultClientFlags()

	cfg := vaulttls.Config{
		Addr:        *tlsVaultAddr,
		PKIPath:     *tlsVaultPKIPath,
		Role:        *tlsVaultRole,
		CommonName:  *tlsVaultCommonName,
		AltNames:    *tlsVaultAltNames,
		TTL:         *tlsVaultTTL,
		RenewBefore: *tlsVaultRenewBefore,
		Auth: vaulttls.AuthConfig{
			Method:              *tlsVaultAuthMethod,
			Mount:               *tlsVaultAuthMount,
			TokenFile:           *tlsVaultTokenFile,
			RoleID:              *tlsVaultAuthRoleID,
			RoleIDFile:          *tlsVaultAuthRoleIDFile,
			SecretIDFile:        *tlsVaultAuthSecretIDFile,
			SecretIDWrappedFile: *tlsVaultAuthSecretIDWrappedFile,
			Role:                *tlsVaultAuthRole,
			JWTFile:             *tlsVaultAuthJWTFile,
		},
		CAFile:             *tlsVaultCAFile,
		ServerName:         *tlsVaultServerName,
		InsecureSkipVerify: *tlsVaultInsecureSkipVerify,
		RevokeOnShutdown:   *tlsVaultRevokeOnShutdown,
		ClientAuth:         *tlsVaultClientAuth,
		TrustPKICA:         *tlsVaultTrustPKICA,
	}
	// Password flags are read lazily: their file:// and http:// forms are re-read
	// by flagutil.Password, so a rotated secret is picked up without a restart.
	// They are wired only when actually set, so that an unset flag doesn't look
	// like a configured-but-empty credential.
	if isFlagSet("tls.vaultToken") {
		cfg.Auth.GetToken = tlsVaultToken.Get
	}
	if isFlagSet("tls.vaultAuthSecretID") {
		cfg.Auth.GetSecretID = tlsVaultAuthSecretID.Get
	}

	logger.Infof("initialising Vault PKI TLS provider: addr=%s, pki=%s, role=%s, cn=%s, ttl=%s, auth=%s",
		cfg.Addr, cfg.PKIPath, cfg.Role, cfg.CommonName, cfg.TTL, *tlsVaultAuthMethod)

	p, err := vaulttls.NewProvider(cfg)
	if err != nil {
		logger.Fatalf("cannot initialise Vault PKI TLS provider: %s", err)
	}
	provider = p

	// Publish the provider so listeners and clients can reach it: syslog and the
	// remote clients call vaulttls.ServerTLSConfig and vaulttls.NewRoundTripper
	// directly, and the HTTP listener goes through httpserver.ServeOptions.GetTLSConfig.
	vaulttls.Register(p)

	// -tls only selects the https scheme for -httpListenAddr; the certificate
	// itself comes from the provider above, not from any file. An explicit -tls is
	// left alone, so that -tls=false can be used to obtain a certificate for the
	// syslog listener or for outgoing connections only.
	if !isFlagSet("tls") {
		setFlagOrFatal("tls", "true")
	}

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

// Stop stops the background renewal goroutine if Vault TLS was used.
func Stop() {
	if provider != nil {
		provider.Stop()
		provider = nil
	}
}

// tokenFlags and loginFlags list the credential flags of each auth method, so
// flags belonging to a method other than the selected one can be rejected
// instead of being silently ignored.
var (
	tokenFlags = []string{"tls.vaultToken", "tls.vaultTokenFile"}
	loginFlags = map[string][]string{
		vaulttls.AuthMethodAppRole: {
			"tls.vaultAuthRoleID", "tls.vaultAuthRoleIDFile",
			"tls.vaultAuthSecretID", "tls.vaultAuthSecretIDFile", "tls.vaultAuthSecretIDWrappedFile",
		},
		vaulttls.AuthMethodKubernetes: {"tls.vaultAuthRole", "tls.vaultAuthJWTFile"},
	}
)

// checkVaultAuthFlags rejects credential flags that don't belong to the selected
// auth method, and warns about credentials that were passed insecurely.
func checkVaultAuthFlags() {
	method := *tlsVaultAuthMethod
	if method == "" {
		method = vaulttls.AuthMethodToken
	}
	for m, names := range loginFlags {
		if m == method {
			continue
		}
		for _, name := range names {
			if isFlagSet(name) {
				logger.Fatalf("-%s belongs to the %q auth method, but -tls.vaultAuthMethod=%q; set -tls.vaultAuthMethod=%s",
					name, m, method, m)
			}
		}
	}
	if method != vaulttls.AuthMethodToken {
		for _, name := range tokenFlags {
			if isFlagSet(name) {
				logger.Fatalf("-%s belongs to the %q auth method, but -tls.vaultAuthMethod=%q",
					name, vaulttls.AuthMethodToken, method)
			}
		}
	}
	warnIfInlineSecret("tls.vaultToken")
	warnIfInlineSecret("tls.vaultAuthSecretID")
}

// clientCertFlags are the per-connection client certificate flags replaced by
// -tls.vaultClientAuth. They are arrays, so they may be set for one destination
// and left to Vault for another; rejecting them outright keeps the resulting
// configuration unambiguous.
var clientCertFlags = []string{
	"storageNode.tlsCertFile", "storageNode.tlsKeyFile",
	"remoteWrite.tlsCertFile", "remoteWrite.tlsKeyFile",
}

// checkVaultClientFlags rejects file-based client certificates when the Vault one
// is requested, since only one of them can be presented on a connection.
func checkVaultClientFlags() {
	if !*tlsVaultClientAuth {
		return
	}
	for _, name := range clientCertFlags {
		if isFlagSet(name) {
			logger.Fatalf("-%s must not be set together with -tls.vaultClientAuth; "+
				"Vault PKI serves the client certificate from memory", name)
		}
	}
}

// warnIfInlineSecret warns when a secret was passed literally on the command
// line: /proc/<pid>/cmdline is world-readable, and argv also leaks into `ps`,
// `docker inspect` and pod manifests. file:// and http:// references are fine,
// and so are values coming from the environment via -envflag.enable, which never
// reach os.Args.
func warnIfInlineSecret(name string) {
	value, ok := commandLineValue(name)
	if !ok {
		return
	}
	for _, prefix := range []string{"file://", "http://", "https://"} {
		if strings.HasPrefix(value, prefix) {
			return
		}
	}
	logger.Warnf("-%s is passed on the command line, so it is exposed via /proc/<pid>/cmdline, `ps` and container "+
		"inspection; pass it as -%s=file:///path/to/file or via the corresponding *File flag instead", name, name)
}

// commandLineValue returns the raw value of the named flag as it appears in
// os.Args. The flag package doesn't expose it, and Password.String() masks it.
func commandLineValue(name string) (string, bool) {
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		for _, dashes := range []string{"-", "--"} {
			if value, ok := strings.CutPrefix(arg, dashes+name+"="); ok {
				return value, true
			}
			if arg == dashes+name && i+1 < len(os.Args) {
				return os.Args[i+1], true
			}
		}
	}
	return "", false
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
