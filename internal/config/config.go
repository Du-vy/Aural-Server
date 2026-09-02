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

// TLS serves the WebSocket over wss:// with a certificate you provide.
type TLS struct {
	Enabled  bool   `json:"enabled"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

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
		TLS:      TLS{Enabled: false},
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

	if c.TLS.Enabled && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return errors.New("tls.enabled requires tls.cert_file and tls.key_file")
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
	if v.PublicIP != "" && net.ParseIP(v.PublicIP) == nil {
		return fmt.Errorf("voice.public_ip %q is not an IP address", v.PublicIP)
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
