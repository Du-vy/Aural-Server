// Package config loads the JSON configuration file that drives a server
// instance. A missing file is created from the defaults, so a fresh install is
// a single run with no arguments.
package config

import (
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

// Log configures the structured logger.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `json:"level"`
	// Format is "text" or "json".
	Format string `json:"format"`
}

// DefaultPort is the port an Aural server listens on unless told otherwise.
const DefaultPort = 9871

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
		Voice:    Voice{Mode: protocol.VoiceModeClientHost},
		TLS:      TLS{Enabled: false},
		Database: Database{Path: "aural.db"},
		Log:      Log{Level: "info", Format: "text"},
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
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, false, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, false, cfg.Validate()
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
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q must be text or json", c.Log.Format)
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
