package uploads

import (
	"bytes"
	"encoding/binary"
	"image"
	"io"
	"os"

	// Registered for their DecodeConfig side effect, which reads a header
	// rather than the whole image.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// headerBytes is how much of a file is read to find its dimensions. Every
// format handled here carries them well inside this.
const headerBytes = 64 * 1024

// Dimensions reads the pixel size of an image file, reporting ok false for
// anything it cannot recognise.
//
// A client that knows the shape of an image before it loads can reserve the
// right space for it, so the conversation does not jump as pictures arrive.
// Failing to read them is not an error worth refusing an upload over: the
// client simply lays that one out once it has loaded.
func Dimensions(path string) (width, height int, ok bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()

	head := make([]byte, headerBytes)
	n, err := io.ReadFull(file, head)
	if n == 0 && err != nil {
		return 0, 0, false
	}
	head = head[:n]

	if cfg, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
		return cfg.Width, cfg.Height, cfg.Width > 0 && cfg.Height > 0
	}
	// WebP is not in the standard library and is now common enough that
	// falling back to "unknown" for it would leave most screenshots unsized.
	return webpDimensions(head)
}

// webpDimensions reads the size out of a RIFF/WEBP header. The three chunk
// kinds encode it differently, which is why each is unpacked by hand.
func webpDimensions(head []byte) (width, height int, ok bool) {
	if len(head) < 30 || !bytes.Equal(head[0:4], []byte("RIFF")) || !bytes.Equal(head[8:12], []byte("WEBP")) {
		return 0, 0, false
	}

	switch string(head[12:16]) {
	case "VP8 ":
		// Lossy: a 3 byte start code, then 14 bits of width and of height.
		if len(head) < 30 || head[23] != 0x9d || head[24] != 0x01 || head[25] != 0x2a {
			return 0, 0, false
		}
		w := int(binary.LittleEndian.Uint16(head[26:28]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(head[28:30]) & 0x3fff)
		return w, h, w > 0 && h > 0

	case "VP8L":
		// Lossless: a signature byte, then 14 bits of each dimension, minus one.
		if len(head) < 25 || head[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(head[21:25])
		w := int(bits&0x3fff) + 1
		h := int((bits>>14)&0x3fff) + 1
		return w, h, true

	case "VP8X":
		// Extended: 24 bit canvas dimensions, each stored minus one.
		if len(head) < 30 {
			return 0, 0, false
		}
		w := int(uint32(head[24]) | uint32(head[25])<<8 | uint32(head[26])<<16)
		h := int(uint32(head[27]) | uint32(head[28])<<8 | uint32(head[29])<<16)
		return w + 1, h + 1, true
	}
	return 0, 0, false
}
