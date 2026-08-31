package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A configuration saved by PowerShell's Out-File, or by several Windows
// editors, starts with a UTF-8 byte order mark. It is not valid JSON, so
// without stripping it a perfectly reasonable edit fails to parse.
func TestLoadAcceptsAUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"server":{"name":"With BOM","port":9999}}`)...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, created, err := Load(path)
	if err != nil {
		t.Fatalf("load config with a BOM: %v", err)
	}
	if created {
		t.Fatal("an existing file must not be reported as created")
	}
	if cfg.Server.Name != "With BOM" || cfg.Server.Port != 9999 {
		t.Fatalf("config did not load: %+v", cfg.Server)
	}
	// Anything the file left out still falls back to the default.
	if cfg.Database.Path != Default().Database.Path {
		t.Fatalf("absent keys should keep their defaults, got %q", cfg.Database.Path)
	}
}

func TestLoadCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg, created, err := Load(path)
	if err != nil {
		t.Fatalf("load missing config: %v", err)
	}
	if !created {
		t.Fatal("a missing file should be reported as created")
	}
	if cfg.Server.Port != DefaultPort {
		t.Fatalf("port: got %d, want %d", cfg.Server.Port, DefaultPort)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the default file was not written: %v", err)
	}

	// Reading it back must produce the same configuration.
	again, created, err := Load(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if created {
		t.Fatal("the second load must not report a creation")
	}
	if again.Server.Port != cfg.Server.Port || again.Server.Name != cfg.Server.Name {
		t.Fatalf("round trip changed the config: %+v vs %+v", again.Server, cfg.Server)
	}
}

// A server that admits neither guests nor registrations is one nobody can ever
// enter, which is worth refusing at startup rather than at connection time.
func TestValidateRejectsAnUnreachableServer(t *testing.T) {
	cfg := Default()
	cfg.Registration.Enabled = false
	cfg.Registration.AllowGuests = false

	if err := cfg.Validate(); err == nil {
		t.Fatal("a server nobody can enter should be refused")
	}
}
