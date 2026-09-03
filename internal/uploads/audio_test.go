package uploads

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// wavFile writes a RIFF/WAVE file of the given length, optionally with a chunk
// in between the header and the audio: browsers routinely put one there, and a
// reader that assumed a fixed offset would measure every such file wrongly.
func wavFile(t *testing.T, seconds float64, extraChunk bool) string {
	t.Helper()

	const (
		sampleRate = 48000
		channels   = 1
		bits       = 16
	)
	byteRate := sampleRate * channels * bits / 8
	dataLen := int(float64(byteRate) * seconds)

	var body bytes.Buffer
	body.WriteString("WAVE")

	body.WriteString("fmt ")
	binary.Write(&body, binary.LittleEndian, uint32(16))
	binary.Write(&body, binary.LittleEndian, uint16(1))
	binary.Write(&body, binary.LittleEndian, uint16(channels))
	binary.Write(&body, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&body, binary.LittleEndian, uint32(byteRate))
	binary.Write(&body, binary.LittleEndian, uint16(channels*bits/8))
	binary.Write(&body, binary.LittleEndian, uint16(bits))

	if extraChunk {
		body.WriteString("LIST")
		binary.Write(&body, binary.LittleEndian, uint32(10))
		body.Write(make([]byte, 10))
	}

	body.WriteString("data")
	binary.Write(&body, binary.LittleEndian, uint32(dataLen))
	body.Write(make([]byte, dataLen))

	var file bytes.Buffer
	file.WriteString("RIFF")
	binary.Write(&file, binary.LittleEndian, uint32(body.Len()))
	file.Write(body.Bytes())

	path := filepath.Join(t.TempDir(), "clip.wav")
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatalf("write test wav: %v", err)
	}
	return path
}

func TestWAVDurationReadsTheLengthOutOfTheHeader(t *testing.T) {
	ms, ok := WAVDuration(wavFile(t, 2.5, false))
	if !ok {
		t.Fatal("a well-formed WAV was not readable")
	}
	if ms < 2450 || ms > 2550 {
		t.Fatalf("measured %d ms, want about 2500", ms)
	}
}

func TestWAVDurationWalksPastAnIntermediateChunk(t *testing.T) {
	ms, ok := WAVDuration(wavFile(t, 1, true))
	if !ok {
		t.Fatal("a WAV with a LIST chunk was not readable")
	}
	if ms < 950 || ms > 1050 {
		t.Fatalf("measured %d ms, want about 1000", ms)
	}
}

func TestWAVDurationRefusesWhatIsNotWAV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.wav")
	// An MP3, or anything else somebody renamed. The length limit is only
	// enforceable because this says no rather than guessing.
	if err := os.WriteFile(path, append([]byte("ID3\x04\x00\x00\x00"), make([]byte, 200)...), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, ok := WAVDuration(path); ok {
		t.Fatal("a file that is not WAV was measured anyway")
	}
}

func TestWAVDurationTrustsTheFileOverTheHeader(t *testing.T) {
	// A header can claim a length the bytes do not back up. The shorter of the
	// two is what will actually play, so it is what the limit is checked on.
	path := wavFile(t, 10, false)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/4], 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	ms, ok := WAVDuration(path)
	if !ok {
		t.Fatal("a truncated WAV was not readable")
	}
	if ms > 4000 {
		t.Fatalf("measured %d ms from a file a quarter that long", ms)
	}
}
