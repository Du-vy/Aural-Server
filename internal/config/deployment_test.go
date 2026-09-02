package config

import (
	"strings"
	"testing"
)

// valid is the smallest configuration that passes, for a test to bend one
// field of at a time.
func valid() Config { return Default() }

// A literal address in voice.public_ip is only correct until the provider
// changes it. Accepting a hostname is what lets a home server name its dynamic
// DNS record instead, so the rejection that used to be here had to go — but
// not so far that a pasted URL slips through.
func TestVoicePublicIPAcceptsAHostnameAndStillRejectsNonsense(t *testing.T) {
	accepted := []string{
		"", "203.0.113.5", "2001:db8::1",
		"aural.duckdns.org", "aural.example.com", "aural.example.com.", "localhost",
	}
	for _, value := range accepted {
		cfg := valid()
		cfg.Voice.PublicIP = value
		if err := cfg.Validate(); err != nil {
			t.Errorf("voice.public_ip %q was refused: %v", value, err)
		}
	}

	refused := []string{
		"https://aural.example.com",
		"aural.example.com:9871",
		"aural example com",
		"-aural.example.com",
		"aural..example.com",
	}
	for _, value := range refused {
		cfg := valid()
		cfg.Voice.PublicIP = value
		if err := cfg.Validate(); err == nil {
			t.Errorf("voice.public_ip %q was accepted; it cannot be resolved", value)
		}
	}
}

func TestDDNSIsOnlyCheckedWhenItIsOn(t *testing.T) {
	// A half-written block can sit in the file while it is being set up.
	cfg := valid()
	cfg.DDNS = DDNS{Enabled: false, Provider: "duckdns"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled ddns block was checked: %v", err)
	}
	// The defaults are still filled in, because ACME reads them even with
	// publishing switched off.
	if cfg.DDNS.IntervalMinutes != DefaultDDNSInterval {
		t.Errorf("interval = %d, want the default", cfg.DDNS.IntervalMinutes)
	}
	if len(cfg.DDNS.STUNServers) == 0 {
		t.Error("no STUN servers were filled in; the address could never be discovered")
	}
}

func TestDDNSRequiresWhatItCannotWorkWithout(t *testing.T) {
	cases := []struct {
		name string
		ddns DDNS
	}{
		{"no provider", DDNS{Enabled: true, Domain: "aural.duckdns.org", Token: "t"}},
		{"unknown provider", DDNS{Enabled: true, Provider: "route53", Domain: "a.example.com", Token: "t"}},
		{"no domain", DDNS{Enabled: true, Provider: "duckdns", Token: "t"}},
		{"no token", DDNS{Enabled: true, Provider: "duckdns", Domain: "aural"}},
		{"a domain that is a URL", DDNS{Enabled: true, Provider: "duckdns", Domain: "https://x.org", Token: "t"}},
		{"proxied on a provider with no proxy", DDNS{
			Enabled: true, Provider: "duckdns", Domain: "aural", Token: "t", Proxied: true,
		}},
		{"a stun server that is not one", DDNS{
			Enabled: true, Provider: "duckdns", Domain: "aural", Token: "t",
			STUNServers: []string{"https://example.org"},
		}},
	}
	for _, tc := range cases {
		cfg := valid()
		cfg.DDNS = tc.ddns
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

func TestDDNSNormalisesTheProvider(t *testing.T) {
	cfg := valid()
	cfg.DDNS = DDNS{Enabled: true, Provider: "  DuckDNS  ", Domain: " aural ", Token: " t "}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.DDNS.Provider != "duckdns" || cfg.DDNS.Domain != "aural" || cfg.DDNS.Token != "t" {
		t.Errorf("not normalised: %+v", cfg.DDNS)
	}
}

// Without ACME the two paths are the certificate. With it they are only
// somewhere to put one, so demanding them would be asking the operator for
// something the server is about to produce.
func TestTLSDemandsFilesOnlyWhenNothingWillObtainThem(t *testing.T) {
	cfg := valid()
	cfg.TLS.Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Error("tls was enabled with no certificate and nothing to get one")
	}

	cfg = valid()
	cfg.TLS.Enabled = true
	cfg.TLS.CertFile, cfg.TLS.KeyFile = "cert.pem", "key.pem"
	if err := cfg.Validate(); err != nil {
		t.Errorf("a plain certificate pair was refused: %v", err)
	}
}

func TestACMEFillsInWhereTheCertificateGoes(t *testing.T) {
	cfg := valid()
	cfg.TLS.Enabled = true
	cfg.TLS.ACME.Enabled = true
	cfg.DDNS = DDNS{Provider: "duckdns", Domain: "aural", Token: "t"}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
		t.Error("no paths were filled in, so there is nowhere to write the certificate")
	}
	// A certificate for a bare DuckDNS subdomain would be a certificate for
	// nothing.
	if len(cfg.TLS.ACME.Domains) != 1 || cfg.TLS.ACME.Domains[0] != "aural.duckdns.org" {
		t.Errorf("domains = %v, want the qualified name", cfg.TLS.ACME.Domains)
	}
	if cfg.ACMEAccountKeyFile() == "" {
		t.Error("no account key path")
	}
}

// The challenge is answered through the DNS provider, so the credentials have
// to be there even on a server that is not publishing its address. Failing at
// startup with that sentence is far better than failing at the first renewal.
func TestACMERequiresDNSCredentials(t *testing.T) {
	cases := []struct {
		name string
		ddns DDNS
	}{
		{"no provider", DDNS{Domain: "aural.example.com", Token: "t"}},
		{"no token", DDNS{Provider: "cloudflare", Domain: "aural.example.com"}},
		{"no domain", DDNS{Provider: "cloudflare", Token: "t"}},
	}
	for _, tc := range cases {
		cfg := valid()
		cfg.TLS.Enabled = true
		cfg.TLS.ACME.Enabled = true
		cfg.TLS.ACME.Domains = []string{"aural.example.com"}
		cfg.DDNS = tc.ddns

		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s was accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "ddns") {
			t.Errorf("%s: the error does not say what is missing: %v", tc.name, err)
		}
	}
}

func TestTrustedProxiesDefaultToTrustingNobody(t *testing.T) {
	cfg := Default()
	if len(cfg.Server.TrustedProxies) != 0 {
		t.Error("a fresh install trusts a forwarding header; a directly reached server must not")
	}
}

func TestRetentionRefusesNegativeWindows(t *testing.T) {
	for _, retention := range []Retention{{TokenIdleDays: -1}, {GuestIdleDays: -1}} {
		cfg := valid()
		cfg.Retention = retention
		if err := cfg.Validate(); err == nil {
			t.Errorf("%+v was accepted", retention)
		}
	}
}
