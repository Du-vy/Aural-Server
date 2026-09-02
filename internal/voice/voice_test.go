package voice

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFmtpLineCarriesEverySetting(t *testing.T) {
	line := Settings{
		SampleRate: 24000,
		MaxBitrate: 96000,
		FEC:        true,
		DTX:        false,
		Stereo:     false,
	}.FmtpLine()

	for _, want := range []string{
		"minptime=10",
		"useinbandfec=1",
		"usedtx=0",
		"stereo=0",
		"sprop-stereo=0",
		"maxplaybackrate=24000",
		"maxaveragebitrate=96000",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("fmtp line is missing %q: %s", want, line)
		}
	}
}

func TestFmtpLineFollowsTheFlags(t *testing.T) {
	line := Settings{SampleRate: 48000, MaxBitrate: 64000, DTX: true, Stereo: true}.FmtpLine()

	if !strings.Contains(line, "usedtx=1") {
		t.Errorf("discontinuous transmission did not reach the fmtp line: %s", line)
	}
	if !strings.Contains(line, "stereo=1") || !strings.Contains(line, "sprop-stereo=1") {
		t.Errorf("stereo did not reach both halves of the fmtp line: %s", line)
	}
	if !strings.Contains(line, "useinbandfec=0") {
		t.Errorf("forward error correction should be off: %s", line)
	}
}

// A subscriber reads a publisher's identity off the stream id it arrives on,
// so the two ends have to agree on how one is spelled. The client parses
// exactly this shape.
func TestStreamAndTrackIDsNameTheirUser(t *testing.T) {
	if got := StreamID(42); got != "av-42" {
		t.Errorf("StreamID: got %q", got)
	}
	if got := TrackID(42); got != "au-42" {
		t.Errorf("TrackID: got %q", got)
	}
}

func TestRelayRejectsAnEmptyOffer(t *testing.T) {
	relay, err := NewRelay(Settings{SampleRate: 48000, MaxBitrate: 64000}, testLogger(), nil)
	if err != nil {
		t.Fatalf("build a relay: %v", err)
	}
	defer relay.Close()

	if _, err := relay.Join(1, 1, "", func(Signal) {}); err == nil {
		t.Fatal("a session with no offer should not open")
	}
}

func TestRelayIsInertWhileNobodyCalls(t *testing.T) {
	relay, err := NewRelay(Settings{SampleRate: 48000, MaxBitrate: 64000}, testLogger(), nil)
	if err != nil {
		t.Fatalf("build a relay: %v", err)
	}
	defer relay.Close()

	// Every one of these names a session that does not exist. None of them is
	// an error worth reporting: they are what a teardown racing a frame in
	// flight looks like, which happens on every call that ends.
	relay.Leave(1, 1)
	relay.LeaveAll(1)
	relay.CloseChannel(1)
	relay.SetMuted(1, 1, true)
	relay.SetDeafened(1, 1, true)
	if relay.Connected(1, 1) {
		t.Fatal("nobody is connected to an empty relay")
	}
	if err := relay.Accept(1, 1, Signal{Kind: "candidate"}); err != ErrNoSession {
		t.Fatalf("signalling into nothing: got %v, want %v", err, ErrNoSession)
	}
}

func TestReconfigureIsANoOpWhenNothingChanged(t *testing.T) {
	settings := Settings{SampleRate: 48000, MaxBitrate: 64000, FEC: true}
	relay, err := NewRelay(settings, testLogger(), nil)
	if err != nil {
		t.Fatalf("build a relay: %v", err)
	}
	defer relay.Close()

	before := relay.api
	if err := relay.Reconfigure(settings); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if relay.api != before {
		t.Fatal("an unchanged configuration rebuilt the stack, which would cut off every call")
	}

	settings.MaxBitrate = 96000
	if err := relay.Reconfigure(settings); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if relay.api == before {
		t.Fatal("a changed configuration did not rebuild the stack")
	}
	if relay.Settings().MaxBitrate != 96000 {
		t.Fatal("the new settings were not kept")
	}
}
