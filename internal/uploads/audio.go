package uploads

import (
	"encoding/binary"
	"io"
	"os"
)

// wavHeaderBytes is how far into a file the audio data chunk is looked for.
// A RIFF header is a few dozen bytes; anything a browser writes puts the data
// chunk well inside this, and a file that hides it further out is not one to
// keep reading.
const wavHeaderBytes = 4096

// WAVDuration reads how long a RIFF/WAVE file runs, in milliseconds.
//
// It exists because the soundboard has a length limit that has to be enforced
// rather than believed. A client declaring the duration of the clip it just cut
// is a client that can declare anything, and the whole point of the limit is
// that a clip is played at everybody in a channel at once.
//
// Only WAV is understood, and that is what makes the limit enforceable: the
// client re-encodes whatever it was given, so every clip arrives in the one
// format whose length can be read exactly from its header rather than guessed
// at from a bitrate.
func WAVDuration(path string) (ms int, ok bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()

	head := make([]byte, wavHeaderBytes)
	n, err := io.ReadFull(file, head)
	if n < 44 && err != nil {
		return 0, false
	}
	head = head[:n]

	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return 0, false
	}

	// Walk the chunks. "fmt " carries the rate and the sample width; "data"
	// carries the length. Neither is at a fixed offset: a file written by a
	// browser very often has a "LIST" chunk between them.
	var byteRate uint32
	at := 12
	for at+8 <= len(head) {
		id := string(head[at : at+4])
		size := binary.LittleEndian.Uint32(head[at+4 : at+8])
		body := at + 8

		switch id {
		case "fmt ":
			if body+16 > len(head) {
				return 0, false
			}
			byteRate = binary.LittleEndian.Uint32(head[body+8 : body+12])
		case "data":
			if byteRate == 0 {
				return 0, false
			}
			// The declared size is not trusted over the file: a header can
			// claim a length the bytes do not back up, and the shorter of the
			// two is the one that will actually play.
			length := int64(size)
			if info, err := file.Stat(); err == nil {
				if actual := info.Size() - int64(body); actual >= 0 && actual < length {
					length = actual
				}
			}
			return int(length * 1000 / int64(byteRate)), true
		}

		// Chunks are padded to an even length.
		at = body + int(size)
		if size%2 == 1 {
			at++
		}
		if at <= body {
			// A zero or overflowing size would walk this loop forever.
			return 0, false
		}
	}
	return 0, false
}
