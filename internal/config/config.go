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

// Voice selects who relays audio. The media plane itself is not implemented in
// v0.1; the mode is already advertised so clients can be built against it.
type Voice struct {
	// Mode is "client_host" or "server_host".
	Mode string `json:"mode"`
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
// for everything the server holds.
const (
	DefaultMaxFileBytes  int64 = 50 * 1024 * 1024
	DefaultMaxTotalBytes int64 = 5 * 1024 * 1024 * 1024
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
		Voice: Voice{Mode: protocol.VoiceModeClientHost},
		Uploads: Uploads{
			Enabled:           true,
			Path:              "uploads",
			MaxFileBytes:      DefaultMaxFileBytes,
			MaxTotalBytes:     DefaultMaxTotalBytes,
			MaxPerMessage:     10,
			PendingTTLMinutes: 60,
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

	switch c.Voice.Mode {
	case protocol.VoiceModeClientHost, protocol.VoiceModeServerHost:
	default:
		return fmt.Errorf("voice.mode %q must be %q or %q",
			c.Voice.Mode, protocol.VoiceModeClientHost, protocol.VoiceModeServerHost)
	}

	if c.Uploads.Enabled {
		c.Uploads.Path = strings.TrimSpace(c.Uploads.Path)
		if c.Uploads.Path == "" {
			return errors.New("uploads.path must not be empty while uploads.enabled is true")
		}
		if c.Uploads.MaxFileBytes < 1 {
			return errors.New("uploads.max_file_bytes must be at least 1")
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
