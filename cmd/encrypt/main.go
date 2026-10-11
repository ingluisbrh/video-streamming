// Command encrypt converts videos into the seekable chunked format (*.cenc).
//
// Usage (prefer the scripts/ wrappers):
//
//	encrypt --gen-key --key-file keys.json
//	encrypt --in movie.mp4 --out videos/movie.cenc
//	encrypt --migrate-old --in movie.enc --out videos/movie.cenc
//	encrypt --decrypt --in videos/movie.cenc --out movie.mp4
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"streaming/internal/chunked"
	"streaming/internal/config"
	"streaming/internal/legacy"
)

func main() {
	genKey := flag.Bool("gen-key", false, "generate a new master key and exit")
	migrateOld := flag.Bool("migrate-old", false, "decrypt legacy whole-file .enc (aes.go format) instead of raw mp4")
	decrypt := flag.Bool("decrypt", false, "decrypt a .cenc back to the original file instead of encrypting")
	in := flag.String("in", "", "input file (.mp4, .cenc with --decrypt, or legacy .enc with --migrate-old)")
	out := flag.String("out", "", "output file (.cenc when encrypting, original name when --decrypt)")
	chunkSize := flag.Uint("chunk-size", chunked.DefaultChunkSize, "bytes per chunk")
	keyFile := flag.String("key-file", "", "keys.json path (else env STREAM_KEY_HEX or ./keys.json)")
	fixFaststart := flag.Bool("fix-faststart", false, "if ffmpeg exists and moov is at end, remake with +faststart first")
	flag.Parse()

	if *genKey {
		p := *keyFile
		if p == "" {
			p = "keys.json"
		}
		if err := config.GenerateKeyFile(p); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("key written to %s (mode 0600). Keep it safe.\n", p)
		return
	}

	if *in == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *decrypt && *migrateOld {
		log.Fatal("--decrypt and --migrate-old are mutually exclusive")
	}
	key, src, err := config.LoadKey(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("key from %s", src)

	if *decrypt {
		if err := chunked.DecryptFile(*in, *out, key); err != nil {
			log.Fatalf("decrypt failed (wrong key or corrupt file?): %v", err)
		}
		st, _ := os.Stat(*out)
		fmt.Printf("wrote %s (%d bytes decrypted)\n", *out, st.Size())
		return
	}

	srcPath := *in
	if !*migrateOld && strings.HasSuffix(strings.ToLower(srcPath), ".mp4") {
		dd, ok, oerr := faststartStatus(srcPath)
		if oerr == nil && !ok {
			log.Printf("WARNING: moov atom is after mdat — browsers will buffer before playing.")
			if *fixFaststart {
				fixed, ferr := faststartFix(srcPath)
				if ferr != nil {
					log.Fatalf("faststart fix failed: %v", ferr)
				}
				srcPath = fixed
				log.Printf("faststart fixed: %s", fixed)
			} else {
				log.Printf("hint: re-run with --fix-faststart (needs ffmpeg) or: ffmpeg -i %q -c copy -movflags +faststart fixed.mp4", srcPath)
			}
		}
		_ = dd
	}

	if *migrateOld {
		if err := migrateLegacy(*in, *out, key, uint32(*chunkSize)); err != nil {
			log.Fatal(err)
		}
	} else {
		if err := chunked.EncryptFile(srcPath, *out, key, uint32(*chunkSize)); err != nil {
			log.Fatal(err)
		}
	}
	st, _ := os.Stat(*out)
	fmt.Printf("wrote %s (%d bytes encrypted)\n", *out, st.Size())
}

// migrateLegacy decrypts the original whole-file GCM blob (nonce+ciphertext)
// then re-encrypts it chunked. Needs ~plaintext size in RAM transiently —
// one-time cost per file.
func migrateLegacy(inPath, outPath string, key []byte, chunkSize uint32) error {
	raw, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	pt, err := legacy.Decrypt(raw, key)
	if err != nil {
		return fmt.Errorf("legacy decrypt failed (wrong key?): %w", err)
	}
	tmp := outPath + ".plain.tmp"
	if err := os.WriteFile(tmp, pt, 0600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	// Best-effort faststart hint for migrated mp4s.
	if st, ok, _ := faststartStatus(tmp); !ok && st != "" {
		log.Printf("WARNING: migrated video has moov at end; consider remuxing with +faststart before encrypting.")
	}
	// Clear plaintext from memory ASAP.
	for i := range pt {
		pt[i] = 0
	}
	if err := chunked.EncryptFile(tmp, outPath, key, chunkSize); err != nil {
		return err
	}
	return nil
}

// faststartStatus reports (dir, isFaststart, err): whether moov precedes mdat.
func faststartStatus(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	var moovAt, mdatAt int64 = -1, -1
	var off int64
	hdr := make([]byte, 8)
	for i := 0; i < 64; i++ { // enough to find both in practice
		if _, err := f.ReadAt(hdr, off); err != nil {
			break
		}
		size := int64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		if size == 1 { // largesize
			lb := make([]byte, 8)
			if _, err := f.ReadAt(lb, off+8); err != nil {
				break
			}
			size = int64(binary.BigEndian.Uint64(lb))
		}
		if size <= 0 {
			break
		}
		switch typ {
		case "moov":
			if moovAt < 0 {
				moovAt = off
			}
		case "mdat":
			if mdatAt < 0 {
				mdatAt = off
			}
		}
		if moovAt >= 0 && mdatAt >= 0 {
			dir := filepath.Dir(path)
			return dir, moovAt < mdatAt, nil
		}
		off += size
	}
	if moovAt < 0 {
		return "", false, io.ErrUnexpectedEOF
	}
	return filepath.Dir(path), true, nil
}

// faststartFix remuxes with ffmpeg -movflags +faststart next to the input.
func faststartFix(path string) (string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("ffmpeg not found")
	}
	ext := filepath.Ext(path)
	fixed := strings.TrimSuffix(path, ext) + ".faststart" + ext
	cmd := exec.Command("ffmpeg", "-y", "-i", path, "-c", "copy", "-movflags", "+faststart", fixed)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return fixed, nil
}
