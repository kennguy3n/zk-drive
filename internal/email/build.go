package email

// OperatorConfig is the subset of process-level config that
// governs transactional-email service construction. Kept separate
// from SMTPConfig (which is the low-level transport config taken
// by NewSMTPClient) so the graceful-degradation contract can be
// owned end-to-end inside this package.
//
// The contract — pinned by tests in build_test.go and documented
// in README.md "Transactional email" — is: omit any one required
// var to leave email disabled WITHOUT failing startup. Required
// vars are PUBLIC_URL, SMTP_HOST, SMTP_FROM_ADDRESS.
type OperatorConfig struct {
	// PublicURL is the externally-reachable frontend base URL,
	// used to compose accept-invite links. Empty → disabled.
	PublicURL string

	// SMTPHost is the relay hostname. Empty → disabled (common
	// dev / metadata-only deployment).
	SMTPHost string

	// SMTPPort is the relay TCP port. Forwarded as-is to NewSMTPClient.
	SMTPPort int

	// SMTPUsername / SMTPPassword are PLAIN-auth credentials.
	// Empty username → unauthenticated send (relay must allow it).
	SMTPUsername string
	SMTPPassword string

	// SMTPFromAddress is the RFC 5322 sender address. Empty →
	// disabled — NewSMTPClient hard-errors on empty FromAddress
	// because a malformed MAIL FROM would be silently rejected by
	// most relays, and we don't want to ship malformed envelopes.
	SMTPFromAddress string

	// SMTPFromName is the optional display-name part of the From
	// header. Empty → bare address.
	SMTPFromName string

	// SMTPTLSMode selects the TLS handshake mode. See SMTPConfig.
	SMTPTLSMode string

	// SMTPTLSServerName overrides the SNI / certificate-hostname
	// match target. Empty → derived from SMTPHost.
	SMTPTLSServerName string

	// SMTPTLSInsecureSkipVerify disables certificate verification.
	// MUST NOT be set in production; intended for lab relays with
	// self-signed certs.
	SMTPTLSInsecureSkipVerify bool
}

// BuildFromOperatorConfig composes a *Service from operator config.
//
// Contract: every "missing required var" path returns a Service
// wired to NoopClient AND a non-nil error from NewService is
// returned ONLY when an actual SMTPClient construction fails
// (e.g. unsupported TLS mode). Each disabled path carries a
// DisabledReason that LogStartup surfaces so the operator sees
// the SPECIFIC missing var (not a generic "set SMTP_*" hint that
// would be actively misleading when other SMTP_* vars are set).
//
// The switch order is intentional: PUBLIC_URL is checked first
// because it disables the service regardless of SMTP_* (even a
// fully-configured SMTP can't usefully send an invite without a
// valid accept URL). SMTP_HOST is checked next because it's the
// most common dev-mode "no relay" path. SMTP_FROM_ADDRESS is
// checked last because it requires SMTP_HOST to be set (otherwise
// the SMTP_HOST branch already disabled us).
func BuildFromOperatorConfig(opts OperatorConfig) (*Service, error) {
	switch {
	case opts.PublicURL == "":
		return NewService(ServiceConfig{
			Sender:         NewNoopClient(),
			PublicURL:      "http://invalid.local",
			DisabledReason: "PUBLIC_URL is not set — composed accept-invite links would be malformed; set PUBLIC_URL to the externally-reachable frontend base URL (e.g. https://drive.example.com) to enable guest-invite delivery",
		})
	case opts.SMTPHost == "":
		return NewService(ServiceConfig{
			Sender:    NewNoopClient(),
			PublicURL: opts.PublicURL,
		})
	case opts.SMTPFromAddress == "":
		return NewService(ServiceConfig{
			Sender:         NewNoopClient(),
			PublicURL:      opts.PublicURL,
			DisabledReason: "SMTP_FROM_ADDRESS is not set — guest-invite emails cannot construct a valid MAIL FROM envelope; set SMTP_FROM_ADDRESS to the sender address (e.g. drive@example.com) to enable guest-invite delivery",
		})
	default:
		c, err := NewSMTPClient(SMTPConfig{
			Host:                  opts.SMTPHost,
			Port:                  opts.SMTPPort,
			Username:              opts.SMTPUsername,
			Password:              opts.SMTPPassword,
			FromAddress:           opts.SMTPFromAddress,
			FromName:              opts.SMTPFromName,
			TLSMode:               opts.SMTPTLSMode,
			TLSServerName:         opts.SMTPTLSServerName,
			TLSInsecureSkipVerify: opts.SMTPTLSInsecureSkipVerify,
		})
		if err != nil {
			return nil, err
		}
		return NewService(ServiceConfig{
			Sender:    c,
			PublicURL: opts.PublicURL,
		})
	}
}
