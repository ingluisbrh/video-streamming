// Package chunked implements the seekable encrypted format (*.cenc).
//
// Layout (big-endian):
//
//	[magic 4:"CENC"][version u8=1][chunkSize u32][baseNonce 12][plaintextSize u64]
//	  then per chunk: [chunkLen u32][GCM(chunk plaintext)]
//
// chunkLen for a full chunk is chunkSize+overhead(16 tag bytes).
// The last chunk may be shorter. Per-chunk nonce = baseNonce XOR
// counter (counter encoded in the last 8 bytes), so every chunk gets a
// unique nonce under the same master key while staying computable from
// the chunk index — this is what makes HTTP Range seeking possible
// without decrypting the whole file.
package chunked

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	Magic                  = "CENC"
	Version          uint8 = 1
	HeaderLen              = 4 + 1 + 4 + 12 + 8 // 29
	TagLen                 = 16
	DefaultChunkSize       = 1 << 20 // 1 MiB
	MinChunkSize           = 64 << 10
	MaxChunkSize           = 16 << 20
)

// Header describes a .cenc file without decrypting anything.
type Header struct {
	ChunkSize     uint32
	BaseNonce     [12]byte
	PlaintextSize uint64
}

// ChunkCount returns how many chunks the plaintext is split into.
func (h Header) ChunkCount() int64 {
	if h.PlaintextSize == 0 {
		return 0
	}
	return int64((h.PlaintextSize + uint64(h.ChunkSize) - 1) / uint64(h.ChunkSize))
}

// PlaintextLenOf returns the plaintext length of chunk index.
func (h Header) PlaintextLenOf(index int64) int64 {
	full := int64(h.ChunkSize)
	start := index * full
	rem := int64(h.PlaintextSize) - start
	if rem > full {
		return full
	}
	return rem
}

// ChunkFileOffset returns the file offset of chunk index's length prefix.
func (h Header) ChunkFileOffset(index int64) int64 {
	// Full chunks occupy 4 + chunkSize + TagLen bytes each.
	return int64(HeaderLen) + index*int64(4+uint32(TagLen)+h.ChunkSize)
}

// nonceFor derives the GCM nonce for chunk counter.
func nonceFor(base [12]byte, counter uint64) []byte {
	n := base
	for i := 0; i < 8; i++ {
		n[4+i] ^= byte(counter >> (8 * (7 - i)))
	}
	out := make([]byte, 12)
	copy(out, n[:])
	return out
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("key length must be 32 bytes for AES-256")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ReadHeader reads and validates the 29-byte header from r.
func ReadHeader(r io.Reader) (Header, error) {
	var h Header
	buf := make([]byte, HeaderLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return h, fmt.Errorf("read header: %w", err)
	}
	return ParseHeader(buf)
}

// ParseHeader validates a 29-byte slice.
func ParseHeader(buf []byte) (Header, error) {
	var h Header
	if len(buf) < HeaderLen {
		return h, errors.New("file too short for cenc header")
	}
	if string(buf[0:4]) != Magic {
		return h, errors.New("not a cenc file (bad magic)")
	}
	if buf[4] != Version {
		return h, fmt.Errorf("unsupported cenc version %d", buf[4])
	}
	h.ChunkSize = binary.BigEndian.Uint32(buf[5:9])
	if h.ChunkSize < MinChunkSize || h.ChunkSize > MaxChunkSize {
		return h, fmt.Errorf("insane chunk size %d", h.ChunkSize)
	}
	copy(h.BaseNonce[:], buf[9:21])
	h.PlaintextSize = binary.BigEndian.Uint64(buf[21:29])
	return h, nil
}

// ReadHeaderAt reads the header from an open file without moving its cursor logic.
func ReadHeaderAt(f *os.File) (Header, error) {
	buf := make([]byte, HeaderLen)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return Header{}, fmt.Errorf("read header: %w", err)
	}
	return ParseHeader(buf)
}

// EncryptFile encrypts inPath into outPath with the chunked format.
// Memory usage stays ~chunkSize regardless of file size.
func EncryptFile(inPath, outPath string, key []byte, chunkSize uint32) error {
	if chunkSize == 0 {
		chunkSize = DefaultChunkSize
	}
	if chunkSize < MinChunkSize || chunkSize > MaxChunkSize {
		return fmt.Errorf("chunk size must be %d..%d", MinChunkSize, MaxChunkSize)
	}
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	var base [12]byte
	if _, err := rand.Read(base[:]); err != nil {
		return err
	}
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer out.Close()

	hdr := make([]byte, HeaderLen)
	copy(hdr[0:4], Magic)
	hdr[4] = Version
	binary.BigEndian.PutUint32(hdr[5:9], chunkSize)
	copy(hdr[9:21], base[:])
	binary.BigEndian.PutUint64(hdr[21:29], uint64(st.Size()))
	if _, err := out.Write(hdr); err != nil {
		return err
	}

	plain := make([]byte, chunkSize)
	var counter uint64
	for {
		n, readErr := io.ReadFull(in, plain)
		if readErr == io.EOF || (readErr == io.ErrUnexpectedEOF && n == 0) {
			break
		}
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		ct := aead.Seal(nil, nonceFor(base, counter), plain[:n], nil)
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(ct)))
		if _, err := out.Write(lb[:]); err != nil {
			return err
		}
		if _, err := out.Write(ct); err != nil {
			return err
		}
		counter++
		if readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	return nil
}

// VerifyKey checks key against path by decrypting its first chunk.
// It returns nil when the key is correct (or the file is empty and there
// is nothing to check), and an error otherwise.
func VerifyKey(path string, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h, err := ReadHeader(f)
	if err != nil {
		return err
	}
	if h.ChunkCount() == 0 {
		return nil
	}
	_, err = decryptChunkAt(f, aead, h, 0)
	return err
}

// decryptChunkAt decrypts chunk index from open file f.
func decryptChunkAt(f *os.File, aead cipher.AEAD, h Header, index int64) ([]byte, error) {
	wantPlain := h.PlaintextLenOf(index)
	wantCT := wantPlain + TagLen
	off := h.ChunkFileOffset(index)
	var lb [4]byte
	if _, err := f.ReadAt(lb[:], off); err != nil {
		return nil, fmt.Errorf("chunk %d len: %w", index, err)
	}
	if got := int64(binary.BigEndian.Uint32(lb[:])); got != wantCT {
		return nil, fmt.Errorf("chunk %d: length mismatch got %d want %d (truncated file?)", index, got, wantCT)
	}
	ct := make([]byte, wantCT)
	if _, err := f.ReadAt(ct, off+4); err != nil {
		return nil, fmt.Errorf("chunk %d data: %w", index, err)
	}
	pt, err := aead.Open(nil, nonceFor(h.BaseNonce, uint64(index)), ct, nil)
	if err != nil {
		return nil, fmt.Errorf("chunk %d auth failed: %w (wrong key or corrupt file)", index, err)
	}
	return pt, nil
}

// StreamRange writes plaintext bytes [start, end] (inclusive) to w,
// decrypting only the chunks that overlap the range.
func StreamRange(w io.Writer, f *os.File, h Header, key []byte, start, end int64) error {
	if h.PlaintextSize == 0 {
		return errors.New("empty video")
	}
	if start < 0 || end < start || end >= int64(h.PlaintextSize) {
		return fmt.Errorf("range %d-%d out of bounds (size %d)", start, end, h.PlaintextSize)
	}
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	cs := int64(h.ChunkSize)
	for idx := start / cs; idx <= end/cs; idx++ {
		pt, err := decryptChunkAt(f, aead, h, idx)
		if err != nil {
			return err
		}
		lo := start - idx*cs
		if lo < 0 {
			lo = 0
		}
		hi := end - idx*cs + 1 // exclusive
		if hi > int64(len(pt)) {
			hi = int64(len(pt))
		}
		if _, err := w.Write(pt[lo:hi]); err != nil {
			return err
		}
	}
	return nil
}
