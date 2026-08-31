package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/store"
)

func captureStdout(fn func()) string {
	r, w, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = w

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outC <- buf.String()
	}()

	fn()

	_ = w.Close()
	os.Stdout = oldStdout
	out := <-outC
	_ = r.Close()
	return out
}

func TestPrintBannerOutput(t *testing.T) {
	cfg := config.Default()
	cfg.Log.NoColor = true
	cfg.Server.Name = "Test Aural Server"
	stats := store.ServerStats{
		RegisteredUsers: 5,
		GuestUsers:      2,
		TotalUsers:      7,
		Categories:      2,
		TextChannels:    4,
		VoiceChannels:   2,
		TotalChannels:   8,
		TotalRoles:      4,
		TotalMessages:   42,
	}

	out := captureStdout(func() {
		PrintBanner(&cfg, stats)
	})

	if !strings.Contains(out, "Test Aural Server") {
		t.Errorf("banner output missing server name: %s", out)
	}
	if !strings.Contains(out, "5 registered (2 guests)") {
		t.Errorf("banner output missing member stats: %s", out)
	}
	if !strings.Contains(out, "8 total (4 text, 2 voice, 2 categories)") {
		t.Errorf("banner output missing channel stats: %s", out)
	}
	if !strings.Contains(out, "4 roles configured") {
		t.Errorf("banner output missing role stats: %s", out)
	}
}

func TestPrintOwnerTokenOutput(t *testing.T) {
	out := captureStdout(func() {
		PrintOwnerToken("test-token-12345", true)
	})

	if !strings.Contains(out, "test-token-12345") {
		t.Errorf("owner token missing token string: %s", out)
	}
	if !strings.Contains(out, "ONE-TIME OWNER TOKEN GENERATED") {
		t.Errorf("owner token missing header: %s", out)
	}
}
