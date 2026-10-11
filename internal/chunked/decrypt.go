package chunked

import (
	"fmt"
	"os"
)

// DecryptFile reverses EncryptFile: it decrypts the .cenc file at inPath
// into outPath, byte-identical to the original plaintext. Memory usage
// stays ~chunkSize regardless of file size. Every chunk is authenticated;
// on any error (wrong key, truncated/corrupt file) the partial output is
// removed so no half-decrypted file is left behind.
func DecryptFile(inPath, outPath string, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()
	h, err := ReadHeaderAt(in)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	ok := false
	defer func() {
		if !ok {
			os.Remove(outPath)
		}
	}()

	var total int64
	for idx := int64(0); idx < h.ChunkCount(); idx++ {
		pt, err := decryptChunkAt(in, aead, h, idx)
		if err != nil {
			return fmt.Errorf("decrypt: %w", err)
		}
		n, err := out.Write(pt)
		if err != nil {
			return err
		}
		total += int64(n)
	}
	if total != int64(h.PlaintextSize) {
		return fmt.Errorf("decrypt: wrote %d bytes, header promised %d", total, h.PlaintextSize)
	}
	ok = true
	return nil
}
