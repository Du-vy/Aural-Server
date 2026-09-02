// Package config loads the JSON configuration file that drives a server
// instance. A missing file is created from the defaults, so a fresh install is
// a single run with no arguments.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// Config is the whole configuration file.
type Config struct {
	Server       Server       `json:"server"`
	Registration Registration `json:"registration"`
	Voice        Voice        `json:"voice"`
	Uploads      Uploads      `json:"uploads"`
	Unfurl       Unfurl       `json:"unfurl"`
	Integrations Integrations `json:"integrations"`
	DDNS         DDNS         `json:"ddns"`
	Retention    Retention    `json:"retention"`
	TLS          TLS          `json:"tls"`
	Database     Database     `json:"database"`
	Log          Log          `json:"log"`
}

// Server holds the listener and the public identity of the instance.
type Server struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Bind        string `json:"bind"`
	Port        int    `json:"port"`
	// Password gates the whole server. Empty means no gate.
	Password string `json:"password"`
	MaxUsers int    `json:"max_users"`
	// AllowedOrigins filters the browser Origin header on the WebSocket
	// upgrade. ["*"] accepts every origin, which is the sane default for a
	// self-hosted server reached by address rather than by domain.
	AllowedOrigins []string `json:"allowed_origins"`
	// TrustedProxies are the addresses or CIDR ranges whose X-Forwarded-For
	// header this server believes.
	//
	// Behind a reverse proxy every request arrives from the proxy, and the
	// address it came from survives only in that header. The header is written
	// by whoever spoke to the proxy, so it is worth nothing unless the
	// immediate peer is known to be one: an empty list, the default, means the
	// header is ignored entirely and the peer is taken at face value.
	//
	// A proxy on the same machine is "127.0.0.1"; one in a container network
	// is that network's range.
	TrustedProxies []string `json:"trusted_proxies"`
}

// Registration controls how identities become accounts.
type Registration struct {
	// Enabled lets guests claim their identity with a username and password.
	Enabled bool `json:"enabled"`
	// AllowGuests lets unregistered users connect at all. With this off, only
	// existing accounts may sign in and registration must happen elsewhere.
	AllowGuests       bool `json:"allow_guests"`
	MinPasswordLength int  `json:"min_password_length"`
	MinUsernameLength int  `json:"min_username_length"`
	MaxUsernameLength int  `json:"max_username_length"`
}

// Voice governs the audio plane: who relays it, what the codec is told to do,
// and how the server-hosted relay reaches the outside world.
//
// Every rate here is in bits per second and every frequency in hertz. The
// bitrate triple is a range with a default inside it: a client picks a target
// within [MinBitrate, MaxBitrate] and starts from Bitrate, so an operator sets
// the ceiling their uplink can afford without having to dictate one number to
// everybody on it.
type Voice struct {
	// Enabled turns the audio plane off entirely. Voice channels remain in the
	// tree and can still be joined, they simply carry no audio, which is what
	// lets a text-only deployment keep the channels it already has.
	Enabled bool `json:"enabled"`
	// Mode is "server_host" or "client_host".
	Mode string `json:"mode"`
	// SampleRate is the highest rate Opus is asked to encode at, in hertz.
	// Opus itself always runs on a 48 kHz clock; this is the "maxplaybackrate"
	// hint, and lowering it is how an operator trades fidelity for bandwidth.
	SampleRate int `json:"sample_rate"`
	// Bitrate is what a client starts at, MinBitrate and MaxBitrate bound what
	// it may move to.
	Bitrate    int `json:"bitrate"`
	MinBitrate int `json:"min_bitrate"`
	MaxBitrate int `json:"max_bitrate"`
	// FEC asks Opus for in-band forward error correction, which recovers a
	// lost packet from the one after it. It costs a little bitrate and is
	// worth it on nearly every real network.
	FEC bool `json:"fec"`
	// DTX stops sending during silence. It saves bandwidth on a channel where
	// most people are listening, at the cost of a slightly harder job for the
	// far end when speech resumes.
	DTX bool `json:"dtx"`
	// Stereo doubles the bitrate for something a microphone rarely produces.
	Stereo bool `json:"stereo"`
	// MaxParticipants caps how many people may hold a live audio session in one
	// channel, on top of the channel's own user limit. Zero leaves it to the
	// channel. It exists because relaying is quadratic: a channel that is fine
	// to sit in is not necessarily one the relay can carry.
	MaxParticipants int `json:"max_participants"`
	// PublicIP is the address the server-hosted relay advertises in its ICE
	// candidates. Leave it empty on a host whose own interface carries the
	// address clients reach it on; set it when the server sits behind a
	// one-to-one NAT, where the interface holds a private address that no
	// client outside could ever connect to.
	//
	// A hostname is accepted as well as a literal, and is what a home server
	// wants: the name of the dynamic DNS record — a DuckDNS subdomain, a
	// Cloudflare A record — is stable while the address behind it is not, and
	// the server re-resolves it while it runs. A literal written into this
	// file on a connection whose address rotates is correct only until the
	// next time the provider changes it, and the symptom is voice that stops
	// working while everything else carries on.
	//
	// Left empty on a server that lists a STUN server in ICEServers, the
	// address is discovered from that instead, which needs no setting here at
	// all.
	PublicIP string `json:"public_ip"`
	// UDPPortMin and UDPPortMax bound the ports the relay binds media to.
	// Both zero lets the operating system choose, which is convenient on a
	// host with no firewall in front of it and useless on one with a firewall
	// that has to be told what to open.
	UDPPortMin int `json:"udp_port_min"`
	UDPPortMax int `json:"udp_port_max"`
	// ICEServers are handed to clients so peers behind NAT can find a path.
	// They matter most in client_host mode, where the two ends are both behind
	// somebody's router. Credentials here reach authenticated clients only:
	// the public server preview deliberately leaves them out.
	ICEServers []ICEServer `json:"ice_servers"`
}

// ICEServer is one STUN or TURN server, in the shape WebRTC expects.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Uploads governs file attachments. Both limits are in bytes so a server
// operator can set them exactly; the client is told about them up front, so a
// file too large is refused before it is sent rather than after.
type Uploads struct {
	// Enabled turns attachments off entirely. A server with it off still
	// serves the files it already holds, so turning it off stops new uploads
	// without breaking existing history.
	Enabled bool `json:"enabled"`
	// Path is the directory files are written under, relative to the working
	// directory unless absolute.
	Path string `json:"path"`
	// MaxFileBytes caps one file.
	MaxFileBytes int64 `json:"max_file_bytes"`
	// MaxAvatarBytes caps one user avatar image.
	MaxAvatarBytes int64 `json:"max_avatar_bytes"`
	// MaxBannerBytes caps one user profile banner image.
	MaxBannerBytes int64 `json:"max_banner_bytes"`
	// MaxTotalBytes caps everything the server stores, across all channels.
	// Zero means no ceiling beyond the disk itself.
	MaxTotalBytes int64 `json:"max_total_bytes"`
	// MaxPerMessage caps how many files one message may carry.
	MaxPerMessage int `json:"max_per_message"`
	// PendingTTLMinutes is how long a file that was uploaded but never posted
	// is kept before it is swept. Someone who picks a file and then abandons
	// the message leaves one behind, and it must not be kept forever.
	PendingTTLMinutes int `json:"pending_ttl_minutes"`
}

// Unfurl controls link preview fetching and caching.
type Unfurl struct {
	// Enabled allows the server to fetch and provide link previews.
	Enabled bool `json:"enabled"`
	// CacheTTLDays is how many days a link preview is kept before re-fetching. Default is 7.
	CacheTTLDays int `json:"cache_ttl_days"`
}

// Integrations holds third-party service credentials like Klipy.com.
type Integrations struct {
	KlipyAPIKey string `json:"klipy_api_key"`
}

// DDNS keeps a dynamic DNS record pointing at this server.
//
// It is for the deployment this project is mostly aimed at: a machine on a
// home connection, reachable only for as long as some name resolves to an
// address the provider is free to change. Something has to keep that record
// current, and the server already has to know its own public address in order
// to advertise it for voice.
//
// Setting this is not required to use a dynamic DNS name. A server whose
// record is updated by the router or by ddclient just names it in
// voice.public_ip and leaves this off.
type DDNS struct {
	Enabled bool `json:"enabled"`
	// Provider is "duckdns" or "cloudflare".
	Provider string `json:"provider"`
	// Domain is the record to keep current. For DuckDNS it is the subdomain,
	// with or without the .duckdns.org suffix; for Cloudflare it is the fully
	// qualified name.
	Domain string `json:"domain"`
	// Token authenticates to the provider. On Cloudflare it is a scoped API
	// token with DNS edit permission on the zone, never a global key.
	Token string `json:"token"`
	// ZoneID names the Cloudflare zone directly, for a token too narrow to
	// list zones. Empty looks the zone up by name.
	ZoneID string `json:"zone_id"`
	// Proxied puts a Cloudflare record behind the orange cloud.
	//
	// Leave it off for any record voice reaches this server by. Cloudflare's
	// proxy does not carry UDP, so WebRTC media cannot pass through it at all,
	// and it only carries WebSocket traffic on the ports it terminates. A
	// proxied deployment needs a second, unproxied name for the audio plane,
	// which is what voice.public_ip is then set to.
	Proxied bool `json:"proxied"`
	// IntervalMinutes is how often the address is checked. The check is a STUN
	// request; the record is only written when the answer has changed.
	IntervalMinutes int `json:"interval_minutes"`
	// STUNServers are asked what this server's public address is. The default
	// list is used when this is empty. They must be reachable from the server,
	// and are contacted whether or not voice is enabled, because this is how
	// the address being published is discovered.
	STUNServers []string `json:"stun_servers"`
}

// DefaultDDNSInterval is how often a dynamic DNS record is checked, in
// minutes. Five is frequent enough that an address change costs minutes of
// unreachability rather than hours, and rare enough that no provider minds.
const DefaultDDNSInterval = 5

// DefaultSTUNServers are asked what address the world sees this server at.
// They are the well-known public ones; an operator who would rather not talk
// to them names their own in ddns.stun_servers.
var DefaultSTUNServers = []string{
	"stun:stun.cloudflare.com:3478",
	"stun:stun.l.google.com:19302",
}

// Retention bounds what the database keeps forever.
//
// Neither of these throws away a conversation. A message records its author's
// name on itself and the foreign key back to the account is nullable, so
// history survives the identity that wrote it; what is swept here is the
// wreckage of people passing through — one-visit guest rows, and credentials
// for devices nobody has used in months.
type Retention struct {
	// TokenIdleDays revokes a session token nobody has presented in that long.
	// A registered user pays one sign-in for it. Zero keeps them forever.
	TokenIdleDays int `json:"token_idle_days"`
	// GuestIdleDays deletes an unclaimed identity last seen that long ago and
	// holding no token — which is to say, one that could never come back as
	// itself in any case. Zero keeps them forever.
	GuestIdleDays int `json:"guest_idle_days"`
}

// TLS serves the WebSocket over wss:// with a certificate you provide.
//
// The pair is re-read whenever it changes on disk, so a renewal — which for
// anything issued by an ACME certificate authority happens every couple of
// months — is picked up without restarting the server.
type TLS struct {
	Enabled  bool   `json:"enabled"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// ACME obtains that certificate automatically instead of being handed one.
	ACME ACME `json:"acme"`
}

// ACME gets a certificate from Let's Encrypt, or another certificate
// authority, over the DNS-01 challenge.
//
// DNS-01 is the only challenge offered here, and deliberately so. HTTP-01
// needs port 80 reachable from the internet, which a residential connection
// often will not give you: providers block it and there is nothing the
// operator can do about that. DNS-01 needs nothing inbound at all — the
// certificate authority looks up a TXT record, which this server publishes
// through the credentials already in the ddns block.
//
// So this needs ddns.provider and ddns.token filled in, whether or not
// ddns.enabled is on. Turning ddns off means "do not publish my address",
// not "forget how to reach my DNS".
type ACME struct {
	Enabled bool `json:"enabled"`
	// Domains are the names the certificate covers. Empty falls back to
	// ddns.domain, which is the name the server is reached by anyway.
	Domains []string `json:"domains"`
	// Email is where the certificate authority sends expiry warnings. It is
	// optional, and is the only notice anybody gets that renewal has been
	// failing for a month.
	Email string `json:"email"`
	// Staging uses Let's Encrypt's staging environment, whose certificates
	// nothing trusts and whose rate limits are generous. It is what to work
	// out a deployment against before spending a real one.
	Staging bool `json:"staging"`
	// DirectoryURL points at some other certificate authority. Empty means
	// Let's Encrypt, or its staging environment when Staging is set.
	DirectoryURL string `json:"directory_url"`
	// CacheDir is where the certificate, its key and the account key are
	// written when tls.cert_file and tls.key_file do not say otherwise.
	CacheDir string `json:"cache_dir"`
}

// DefaultACMECacheDir is where an obtained certificate is kept.
const DefaultACMECacheDir = "acme"

// Database points at the SQLite file.
type Database struct {
	Path string `json:"path"`
}

// Log configures the structured logger and optional disk logging.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `json:"level"`
	// Format is "pretty", "text" or "json".
	Format string `json:"format"`
	// File is an optional path to write logs to (e.g. "logs/aural.log").
	// Empty means file logging is disabled.
	File string `json:"file"`
	// FileLevel is the minimum log level for the file handler (debug, info, warn, error).
	// When empty, it inherits Level.
	FileLevel string `json:"file_level"`
	// FileFormat is "json" or "text" for the file handler. Defaults to "json".
	FileFormat string `json:"file_format"`
	// NoColor disables ANSI color escape codes in console output.
	NoColor bool `json:"no_color"`
}

// DefaultPort is the port an Aural server listens on unless told otherwise.
const DefaultPort = 9871

// The upload ceilings a fresh install starts from: 50 MiB for one file, 5 GiB
// for everything the server holds, 8 MiB for an avatar, 16 MiB for a banner.
const (
	DefaultMaxFileBytes   int64 = 50 * 1024 * 1024
	DefaultMaxAvatarBytes int64 = 8 * 1024 * 1024
	DefaultMaxBannerBytes int64 = 16 * 1024 * 1024
	DefaultMaxTotalBytes  int64 = 5 * 1024 * 1024 * 1024
)

// The audio plane a fresh install starts from. 48 kHz is Opus's own clock, so
// it is the one rate that asks the codec for no resampling at all; 64 kb/s is
// transparent for speech and is what a client starts at unless its operator or
// its user says otherwise.
const (
	DefaultVoiceSampleRate = 48000
	DefaultVoiceBitrate    = 64000
	DefaultVoiceMinBitrate = 16000
	DefaultVoiceMaxBitrate = 128000
)

// OpusSampleRates are the rates Opus actually encodes at. Anything else — 44100
// above all, which is what a person reaches for out of habit — is not one of
// them: Opus resamples internally to the nearest of these, so naming 44100
// would ask for something the codec would silently not do.
var OpusSampleRates = []int{8000, 12000, 16000, 24000, 48000}

// The bitrate window Opus itself accepts, which bounds what an operator may
// configure.
const (
	MinOpusBitrate = 6000
	MaxOpusBitrate = 510000
)

// Default returns the configuration a fresh install starts from.
func Default() Config {
	return Config{
		Server: Server{
			Name:           "Aural Server",
			Description:    "A self-hosted Aural server",
			Bind:           "0.0.0.0",
			Port:           DefaultPort,
			Password:       "",
			MaxUsers:       64,
			AllowedOrigins: []string{"*"},
			TrustedProxies: []string{},
		},
		Registration: Registration{
			Enabled:           true,
			AllowGuests:       true,
			MinPasswordLength: 8,
			MinUsernameLength: 3,
			MaxUsernameLength: 32,
		},
		// server_host is the default because it is the mode that works with no
		// further setup: the server already holds an address every client can
		// reach, which is the one thing NAT traversal is otherwise short of.
		// client_host saves the operator that bandwidth and needs a STUN
		// server, and usually a TURN server, to find a path between two people
		// who are both behind somebody's router.
		Voice: Voice{
			Enabled:    true,
			Mode:       protocol.VoiceModeServerHost,
			SampleRate: DefaultVoiceSampleRate,
			Bitrate:    DefaultVoiceBitrate,
			MinBitrate: DefaultVoiceMinBitrate,
			MaxBitrate: DefaultVoiceMaxBitrate,
			FEC:        true,
			DTX:        false,
			Stereo:     false,
			ICEServers: []ICEServer{},
		},
		Uploads: Uploads{
			Enabled:           true,
			Path:              "uploads",
			MaxFileBytes:      DefaultMaxFileBytes,
			MaxAvatarBytes:    DefaultMaxAvatarBytes,
			MaxBannerBytes:    DefaultMaxBannerBytes,
			MaxTotalBytes:     DefaultMaxTotalBytes,
			MaxPerMessage:     10,
			PendingTTLMinutes: 60,
		},
		Unfurl: Unfurl{
			Enabled:      true,
			CacheTTLDays: 7,
		},
		Integrations: Integrations{
			KlipyAPIKey: "",
		},
		DDNS: DDNS{
			Enabled:         false,
			IntervalMinutes: DefaultDDNSInterval,
			// Listed rather than left empty so that the file a fresh install
			// writes says which servers it would talk to, instead of naming
			// them only once somebody has switched the block on.
			STUNServers: slices.Clone(DefaultSTUNServers),
		},
		// Generous on purpose. The point is that a server left running for
		// years does not accumulate an unbounded pile of one-visit identities
		// and forever-valid credentials, not to expire anybody who is actually
		// using it.
		Retention: Retention{
			TokenIdleDays: 90,
			GuestIdleDays: 30,
		},
		TLS: TLS{
			Enabled: false,
			ACME: ACME{
				Enabled:  false,
				Domains:  []string{},
				CacheDir: DefaultACMECacheDir,
			},
		},
		Database: Database{Path: "aural.db"},
		Log: Log{
			Level:      "info",
			Format:     "pretty",
			File:       "",
			FileLevel:  "info",
			FileFormat: "json",
			NoColor:    false,
		},
	}
}

// Load reads path, filling anything the file leaves out from the defaults. When
// the file does not exist it is created with the default contents and a nil
// error is returned, so the first run of a new server needs no setup.
func Load(path string) (Config, bool, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := Save(path, cfg); err != nil {
			return cfg, false, fmt.Errorf("create default config: %w", err)
		}
		return cfg, true, cfg.Validate()
	case err != nil:
		return cfg, false, fmt.Errorf("read config: %w", err)
	}

	// Decoding onto the defaults leaves absent keys at their default value.
	if err := json.Unmarshal(stripBOM(raw), &cfg); err != nil {
		return cfg, false, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, false, cfg.Validate()
}

// utf8BOM is the byte order mark several Windows editors, and PowerShell's own
// Out-File, prepend to a UTF-8 file. It is not valid JSON, so a configuration
// saved by one of them would otherwise fail to parse with a message that gives
// no hint of the real cause.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func stripBOM(raw []byte) []byte {
	return bytes.TrimPrefix(raw, utf8BOM)
}

// Save writes cfg to path as indented JSON.
func Save(path string, cfg Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, raw, 0o600)
}

// Validate rejects a configuration that would produce a broken server, and
// normalises the values that have a canonical form.
func (c *Config) Validate() error {
	c.Server.Name = strings.TrimSpace(c.Server.Name)
	if c.Server.Name == "" {
		return errors.New("server.name must not be empty")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is out of range", c.Server.Port)
	}
	if c.Server.Bind == "" {
		c.Server.Bind = "0.0.0.0"
	}
	if c.Server.MaxUsers < 1 {
		return errors.New("server.max_users must be at least 1")
	}
	if len(c.Server.AllowedOrigins) == 0 {
		c.Server.AllowedOrigins = []string{"*"}
	}

	if !c.Registration.AllowGuests && !c.Registration.Enabled {
		return errors.New("registration.allow_guests and registration.enabled are both false, nobody could ever connect")
	}
	if c.Registration.MinPasswordLength < 4 {
		return errors.New("registration.min_password_length must be at least 4")
	}
	if c.Registration.MinUsernameLength < 1 {
		return errors.New("registration.min_username_length must be at least 1")
	}
	if c.Registration.MaxUsernameLength < c.Registration.MinUsernameLength {
		return errors.New("registration.max_username_length is below min_username_length")
	}

	if err := c.Voice.validate(); err != nil {
		return err
	}

	if c.Uploads.Enabled {
		c.Uploads.Path = strings.TrimSpace(c.Uploads.Path)
		if c.Uploads.Path == "" {
			return errors.New("uploads.path must not be empty while uploads.enabled is true")
		}
		if c.Uploads.MaxFileBytes < 1 {
			return errors.New("uploads.max_file_bytes must be at least 1")
		}
		if c.Uploads.MaxAvatarBytes < 1 {
			c.Uploads.MaxAvatarBytes = DefaultMaxAvatarBytes
		}
		if c.Uploads.MaxBannerBytes < 1 {
			c.Uploads.MaxBannerBytes = DefaultMaxBannerBytes
		}
		if c.Uploads.MaxTotalBytes < 0 {
			return errors.New("uploads.max_total_bytes must not be negative")
		}
		if c.Uploads.MaxTotalBytes > 0 && c.Uploads.MaxTotalBytes < c.Uploads.MaxFileBytes {
			return errors.New("uploads.max_total_bytes is below max_file_bytes, no file could ever be stored")
		}
		if c.Uploads.MaxPerMessage < 1 {
			return errors.New("uploads.max_per_message must be at least 1")
		}
		if c.Uploads.PendingTTLMinutes < 1 {
			return errors.New("uploads.pending_ttl_minutes must be at least 1")
		}
	}

	if err := c.DDNS.validate(); err != nil {
		return err
	}

	if c.Retention.TokenIdleDays < 0 {
		return errors.New("retention.token_idle_days must not be negative")
	}
	if c.Retention.GuestIdleDays < 0 {
		return errors.New("retention.guest_idle_days must not be negative")
	}

	if err := c.validateTLS(); err != nil {
		return err
	}

	if strings.TrimSpace(c.Database.Path) == "" {
		return errors.New("database.path must not be empty")
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q must be debug, info, warn or error", c.Log.Level)
	}
	switch c.Log.Format {
	case "pretty", "text", "json":
	default:
		return fmt.Errorf("log.format %q must be pretty, text or json", c.Log.Format)
	}
	if c.Log.FileLevel != "" {
		switch c.Log.FileLevel {
		case "debug", "info", "warn", "error":
		default:
			return fmt.Errorf("log.file_level %q must be debug, info, warn or error", c.Log.FileLevel)
		}
	}
	if c.Log.FileFormat != "" {
		switch c.Log.FileFormat {
		case "json", "text":
		default:
			return fmt.Errorf("log.file_format %q must be json or text", c.Log.FileFormat)
		}
	}
	return nil
}

// Address is the host:port the listener binds to.
func (c *Config) Address() string {
	return net.JoinHostPort(c.Server.Bind, strconv.Itoa(c.Server.Port))
}

// OriginAllowed reports whether a browser Origin header may open a WebSocket.
// An empty origin comes from a native client, which is always allowed.
func (c *Config) OriginAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	for _, allowed := range c.Server.AllowedOrigins {
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

// validate checks the audio plane and normalises the values that have a
// canonical form. It is a method so that a runtime edit over the protocol runs
// exactly the same rules the configuration file does.
func (v *Voice) validate() error {
	switch v.Mode {
	case protocol.VoiceModeClientHost, protocol.VoiceModeServerHost:
	default:
		return fmt.Errorf("voice.mode %q must be %q or %q",
			v.Mode, protocol.VoiceModeClientHost, protocol.VoiceModeServerHost)
	}

	if v.SampleRate == 0 {
		v.SampleRate = DefaultVoiceSampleRate
	}
	if !slices.Contains(OpusSampleRates, v.SampleRate) {
		return fmt.Errorf("voice.sample_rate %d is not one Opus encodes at; use one of %v",
			v.SampleRate, OpusSampleRates)
	}

	if v.MinBitrate == 0 {
		v.MinBitrate = DefaultVoiceMinBitrate
	}
	if v.MaxBitrate == 0 {
		v.MaxBitrate = DefaultVoiceMaxBitrate
	}
	if v.Bitrate == 0 {
		v.Bitrate = DefaultVoiceBitrate
	}
	if v.MinBitrate < MinOpusBitrate || v.MinBitrate > MaxOpusBitrate {
		return fmt.Errorf("voice.min_bitrate %d is outside the %d-%d bit/s Opus accepts",
			v.MinBitrate, MinOpusBitrate, MaxOpusBitrate)
	}
	if v.MaxBitrate < MinOpusBitrate || v.MaxBitrate > MaxOpusBitrate {
		return fmt.Errorf("voice.max_bitrate %d is outside the %d-%d bit/s Opus accepts",
			v.MaxBitrate, MinOpusBitrate, MaxOpusBitrate)
	}
	if v.MinBitrate > v.MaxBitrate {
		return fmt.Errorf("voice.min_bitrate %d is above voice.max_bitrate %d",
			v.MinBitrate, v.MaxBitrate)
	}
	// A default outside its own range is a typo rather than an intention, but
	// it is one with an obvious reading, so it is clamped rather than refused.
	v.Bitrate = min(max(v.Bitrate, v.MinBitrate), v.MaxBitrate)

	if v.MaxParticipants < 0 {
		return errors.New("voice.max_participants must not be negative")
	}

	v.PublicIP = strings.TrimSpace(v.PublicIP)
	if v.PublicIP != "" && net.ParseIP(v.PublicIP) == nil && !isHostname(v.PublicIP) {
		return fmt.Errorf("voice.public_ip %q is neither an IP address nor a hostname", v.PublicIP)
	}

	switch {
	case v.UDPPortMin == 0 && v.UDPPortMax == 0:
		// The operating system picks, which is what an unfirewalled host wants.
	case v.UDPPortMin < 1 || v.UDPPortMin > 65535:
		return fmt.Errorf("voice.udp_port_min %d is out of range", v.UDPPortMin)
	case v.UDPPortMax < 1 || v.UDPPortMax > 65535:
		return fmt.Errorf("voice.udp_port_max %d is out of range", v.UDPPortMax)
	case v.UDPPortMin > v.UDPPortMax:
		return fmt.Errorf("voice.udp_port_min %d is above voice.udp_port_max %d",
			v.UDPPortMin, v.UDPPortMax)
	}

	for i, srv := range v.ICEServers {
		urls := make([]string, 0, len(srv.URLs))
		for _, raw := range srv.URLs {
			u := strings.TrimSpace(raw)
			if u == "" {
				continue
			}
			scheme, _, ok := strings.Cut(u, ":")
			if !ok {
				return fmt.Errorf("voice.ice_servers[%d] url %q has no scheme", i, u)
			}
			switch strings.ToLower(scheme) {
			case "stun", "stuns", "turn", "turns":
			default:
				return fmt.Errorf("voice.ice_servers[%d] url %q must be stun:, stuns:, turn: or turns:", i, u)
			}
			urls = append(urls, u)
		}
		if len(urls) == 0 {
			return fmt.Errorf("voice.ice_servers[%d] lists no urls", i)
		}
		v.ICEServers[i].URLs = urls
	}
	return nil
}

// validateTLS checks the certificate half of the configuration.
//
// With ACME on, the two file paths stop being something the operator has to
// provide and become somewhere for the obtained certificate to live, so they
// are filled in rather than demanded. Without it, they are the certificate
// itself and there is nothing to serve without them.
func (c *Config) validateTLS() error {
	c.TLS.CertFile = strings.TrimSpace(c.TLS.CertFile)
	c.TLS.KeyFile = strings.TrimSpace(c.TLS.KeyFile)
	c.TLS.ACME.Email = strings.TrimSpace(c.TLS.ACME.Email)
	c.TLS.ACME.DirectoryURL = strings.TrimSpace(c.TLS.ACME.DirectoryURL)
	c.TLS.ACME.CacheDir = strings.TrimSpace(c.TLS.ACME.CacheDir)

	if !c.TLS.Enabled || !c.TLS.ACME.Enabled {
		if c.TLS.Enabled && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
			return errors.New("tls.enabled requires tls.cert_file and tls.key_file, or tls.acme.enabled")
		}
		return nil
	}

	// The name being certified defaults to the one being published, because on
	// the deployment this is for they are the same name.
	if len(c.TLS.ACME.Domains) == 0 && c.DDNS.Domain != "" {
		c.TLS.ACME.Domains = []string{qualify(c.DDNS.Domain, c.DDNS.Provider)}
	}
	if len(c.TLS.ACME.Domains) == 0 {
		return errors.New("tls.acme.enabled requires tls.acme.domains, or a ddns.domain to take them from")
	}
	for i, domain := range c.TLS.ACME.Domains {
		domain = strings.TrimSpace(domain)
		if !isHostname(domain) {
			return fmt.Errorf("tls.acme.domains[%d] %q is not a hostname", i, domain)
		}
		c.TLS.ACME.Domains[i] = domain
	}

	// The challenge is answered through the DNS provider, so its credentials
	// have to be there even on a server that is not publishing its address.
	switch c.DDNS.Provider {
	case "duckdns", "cloudflare":
	default:
		return errors.New("tls.acme.enabled needs ddns.provider set to \"duckdns\" or \"cloudflare\": " +
			"the DNS-01 challenge is answered through it")
	}
	if c.DDNS.Token == "" {
		return errors.New("tls.acme.enabled needs ddns.token, which is what answers the DNS-01 challenge")
	}
	if c.DDNS.Domain == "" {
		return errors.New("tls.acme.enabled needs ddns.domain, which names the zone the challenge is written to")
	}

	if c.TLS.ACME.CacheDir == "" {
		c.TLS.ACME.CacheDir = DefaultACMECacheDir
	}
	if c.TLS.CertFile == "" {
		c.TLS.CertFile = filepath.Join(c.TLS.ACME.CacheDir, "cert.pem")
	}
	if c.TLS.KeyFile == "" {
		c.TLS.KeyFile = filepath.Join(c.TLS.ACME.CacheDir, "key.pem")
	}
	return nil
}

// qualify expands a provider's shorthand into the name a certificate is
// actually issued for. DuckDNS is configured with a bare subdomain, and a
// certificate for "myserver" would be a certificate for nothing.
func qualify(domain, provider string) string {
	if provider == "duckdns" && !strings.Contains(domain, ".") {
		return domain + ".duckdns.org"
	}
	return domain
}

// ACMEAccountKeyFile is where the account key lives, beside the certificate it
// was used to obtain.
func (c *Config) ACMEAccountKeyFile() string {
	dir := c.TLS.ACME.CacheDir
	if dir == "" {
		dir = DefaultACMECacheDir
	}
	return filepath.Join(dir, "account.key")
}

// validate checks the dynamic DNS block and fills in what it leaves out. A
// disabled block is not checked at all, so a half-written one can be left in
// the file while it is being set up.
func (d *DDNS) validate() error {
	d.Provider = strings.ToLower(strings.TrimSpace(d.Provider))
	d.Domain = strings.TrimSpace(d.Domain)
	d.Token = strings.TrimSpace(d.Token)
	d.ZoneID = strings.TrimSpace(d.ZoneID)

	if d.IntervalMinutes < 1 {
		d.IntervalMinutes = DefaultDDNSInterval
	}
	if len(d.STUNServers) == 0 {
		d.STUNServers = slices.Clone(DefaultSTUNServers)
	}
	if !d.Enabled {
		return nil
	}

	switch d.Provider {
	case "duckdns", "cloudflare":
	default:
		return fmt.Errorf("ddns.provider %q must be \"duckdns\" or \"cloudflare\"", d.Provider)
	}
	if d.Domain == "" {
		return errors.New("ddns.domain must not be empty while ddns.enabled is true")
	}
	if !isHostname(d.Domain) {
		return fmt.Errorf("ddns.domain %q is not a hostname", d.Domain)
	}
	if d.Token == "" {
		return errors.New("ddns.token must not be empty while ddns.enabled is true")
	}
	if d.Proxied && d.Provider != "cloudflare" {
		return errors.New("ddns.proxied only means anything on cloudflare")
	}
	for i, raw := range d.STUNServers {
		u := strings.TrimSpace(raw)
		if scheme, _, ok := strings.Cut(u, ":"); !ok || !strings.EqualFold(scheme, "stun") {
			return fmt.Errorf("ddns.stun_servers[%d] %q must be a stun: url", i, raw)
		}
		d.STUNServers[i] = u
	}
	return nil
}

// isHostname reports whether s looks like a DNS name.
//
// The check is deliberately loose. Its job is to catch a typo — a URL pasted
// in whole, an address with a port stuck on the end — rather than to enforce
// the letter of the DNS specification on a name the resolver is about to have
// the final word on anyway.
func isHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	// A trailing dot is a legal fully qualified name.
	s = strings.TrimSuffix(s, ".")
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// SameAs reports whether two audio planes are identical. Voice holds a slice,
// so it cannot be compared with ==, and the comparison matters: it is what
// keeps an administrator saving an unrelated setting from cutting off a call.
func (v Voice) SameAs(other Voice) bool {
	if v.Enabled != other.Enabled ||
		v.Mode != other.Mode ||
		v.SampleRate != other.SampleRate ||
		v.Bitrate != other.Bitrate ||
		v.MinBitrate != other.MinBitrate ||
		v.MaxBitrate != other.MaxBitrate ||
		v.FEC != other.FEC ||
		v.DTX != other.DTX ||
		v.Stereo != other.Stereo ||
		v.MaxParticipants != other.MaxParticipants ||
		v.PublicIP != other.PublicIP ||
		v.UDPPortMin != other.UDPPortMin ||
		v.UDPPortMax != other.UDPPortMax ||
		len(v.ICEServers) != len(other.ICEServers) {
		return false
	}
	for i, srv := range v.ICEServers {
		peer := other.ICEServers[i]
		if srv.Username != peer.Username ||
			srv.Credential != peer.Credential ||
			!slices.Equal(srv.URLs, peer.URLs) {
			return false
		}
	}
	return true
}

// Validate checks an audio plane on its own, which is what a runtime edit over
// the protocol needs: the same rules as the file, without the rest of it.
func (v *Voice) Validate() error { return v.validate() }
