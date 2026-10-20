package responsehistory

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Only canonical, unescaped Base64 is extracted. Everything else stays verbatim
// in the document. In particular, images are never decoded into pixels.
var imageURLPrefix = []byte(`"data:image/`)

type imageRef struct {
	Offset int    `json:"offset"`
	Hash   string `json:"hash"`
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func splitImages(raw []byte) ([]byte, []imageRef, map[string][]byte) {
	var packed bytes.Buffer
	var refs []imageRef
	images := make(map[string][]byte)
	pos, search := 0, 0
	// Byte scanning avoids running a regexp over every byte of multi-MB Base64.
	// Only literal quoted URLs are eligible; all other JSON bytes remain intact.
	for search < len(raw) {
		i := bytes.Index(raw[search:], imageURLPrefix)
		if i < 0 {
			break
		}
		mimeStart := search + i + len(imageURLPrefix)
		quote := bytes.IndexByte(raw[mimeStart:], '"')
		if quote < 0 {
			break
		}
		end := mimeStart + quote
		search = end + 1
		semicolon := bytes.IndexByte(raw[mimeStart:end], ';')
		if semicolon <= 0 {
			continue
		}
		validMIME := true
		for _, c := range raw[mimeStart : mimeStart+semicolon] {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '+' || c == '-') {
				validMIME = false
				break
			}
		}
		marker := mimeStart + semicolon
		if !validMIME || !bytes.HasPrefix(raw[marker:end], []byte(";base64,")) {
			continue
		}
		start := marker + len(";base64,")
		encoded := raw[start:end]
		if len(encoded) == 0 {
			continue
		}
		binary := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
		n, err := base64.StdEncoding.Strict().Decode(binary, encoded)
		// Strict validates padding bits; the length check also excludes ignored
		// CR/LF, so restoring standard Base64 reproduces the exact representation.
		if err != nil || base64.StdEncoding.EncodedLen(n) != len(encoded) {
			continue
		}
		binary = binary[:n]
		packed.Write(raw[pos:start])
		hash := digest(binary)
		refs = append(refs, imageRef{Offset: packed.Len(), Hash: hash})
		images[hash] = binary
		pos = end
	}
	if len(refs) == 0 {
		return raw, nil, images
	}
	packed.Write(raw[pos:])
	return packed.Bytes(), refs, images
}

func joinImages(packed []byte, refs []imageRef, images map[string][]byte, size int64, checksum string) ([]byte, error) {
	// The final length is checked before allocation, including repeated images.
	n := int64(len(packed))
	pos := 0
	for _, ref := range refs {
		blob, ok := images[ref.Hash]
		if !ok || ref.Offset < pos || ref.Offset > len(packed) || digest(blob) != ref.Hash {
			return nil, fmt.Errorf("invalid history image reference")
		}
		n += int64(base64.StdEncoding.EncodedLen(len(blob)))
		if n > size {
			return nil, fmt.Errorf("history size mismatch")
		}
		pos = ref.Offset
	}
	if n != size || size < 0 || uint64(size) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("history size mismatch")
	}
	out := make([]byte, 0, int(size))
	pos = 0
	for _, ref := range refs {
		out = append(out, packed[pos:ref.Offset]...)
		out = base64.StdEncoding.AppendEncode(out, images[ref.Hash])
		pos = ref.Offset
	}
	out = append(out, packed[pos:]...)
	if digest(out) != checksum {
		return nil, fmt.Errorf("history checksum mismatch")
	}
	return out, nil
}

func encodePayload(raw []byte, cold bool) ([]byte, string, error) {
	if len(raw) < 1024 {
		return raw, "raw", nil
	}
	level := zstd.SpeedFastest
	if cold {
		level = zstd.EncoderLevelFromZstd(9)
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(256<<10))
	if err != nil {
		return nil, "", err
	}
	defer encoder.Close()
	compressed := encoder.EncodeAll(raw, nil)
	if len(compressed) >= len(raw) {
		return raw, "raw", nil
	}
	return compressed, "zstd", nil
}

func decodePayload(payload []byte, codec string, size int64) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("invalid history payload size")
	}
	if codec == "raw" {
		if int64(len(payload)) != size {
			return nil, fmt.Errorf("history payload size mismatch")
		}
		return payload, nil
	}
	if codec != "zstd" {
		return nil, fmt.Errorf("unknown history codec")
	}
	limit := uint64(size)
	if limit < 1<<20 {
		limit = 1 << 20
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(limit), zstd.WithDecoderMaxWindow(256<<10))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	raw, err := decoder.DecodeAll(payload, nil)
	if err != nil || int64(len(raw)) != size {
		return nil, fmt.Errorf("invalid compressed history payload")
	}
	return raw, nil
}
