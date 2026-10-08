package chunked

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 100, 1000}
	key := testKey(t)
	for _, n := range sizes {
		data := make([]byte, n)
		rand.Read(data)
		dir := t.TempDir()
		in, out := filepath.Join(dir, "i"), filepath.Join(dir, "o")
		os.WriteFile(in, data, 0600)
		if err := EncryptFile(in, out, key, 64<<10); err != nil {
			t.Fatal(err)
		}
		f, _ := os.Open(out)
		h, err := ReadHeader(f)
		if err != nil {
			t.Fatal(err)
		}
		if h.PlaintextSize != uint64(n) {
			t.Fatalf("size %d: header says %d", n, h.PlaintextSize)
		}
		if n == 0 {
			f.Close()
			continue
		}
		var buf bytes.Buffer
		if err := StreamRange(&buf, f, h, key, 0, int64(n)-1); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if !bytes.Equal(buf.Bytes(), data) {
			t.Fatalf("size %d: round-trip mismatch", n)
		}
	}
}

func TestSeekRanges(t *testing.T) {
	key := testKey(t)
	data := make([]byte, 300<<10) // spans multiple 64KiB chunks incl. partial
	rand.Read(data)
	dir := t.TempDir()
	in, out := filepath.Join(dir, "i"), filepath.Join(dir, "o")
	os.WriteFile(in, data, 0600)
	if err := EncryptFile(in, out, key, 64<<10); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	h, _ := ReadHeader(f)

	spots := [][2]int64{{0, 0}, {0, 10}, {65535, 65536}, {65536, 131073}, {int64(len(data)) - 7, int64(len(data)) - 1}}
	for _, s := range spots {
		var buf bytes.Buffer
		if err := StreamRange(&buf, f, h, key, s[0], s[1]); err != nil {
			t.Fatalf("range %v: %v", s, err)
		}
		if !bytes.Equal(buf.Bytes(), data[s[0]:s[1]+1]) {
			t.Fatalf("range %v: mismatch", s)
		}
	}
}

func TestTamperDetected(t *testing.T) {
	key := testKey(t)
	data := make([]byte, 200<<10)
	rand.Read(data)
	dir := t.TempDir()
	in, out := filepath.Join(dir, "i"), filepath.Join(dir, "o")
	os.WriteFile(in, data, 0600)
	if err := EncryptFile(in, out, key, 64<<10); err != nil {
		t.Fatal(err)
	}
	// flip one byte in first chunk payload
	rf, _ := os.OpenFile(out, os.O_RDWR, 0600)
	rf.WriteAt([]byte{0xFF}, HeaderLen+4+100)
	rf.Close()
	f, _ := os.Open(out)
	defer f.Close()
	h, _ := ReadHeader(f)
	var buf bytes.Buffer
	if err := StreamRange(&buf, f, h, key, 0, 1000); err == nil {
		t.Fatal("expected auth failure on tampered chunk")
	}
}

func TestVerifyKey(t *testing.T) {
	key := testKey(t)
	data := make([]byte, 70<<10)
	rand.Read(data)
	dir := t.TempDir()
	in, out := filepath.Join(dir, "i"), filepath.Join(dir, "o")
	os.WriteFile(in, data, 0600)
	if err := EncryptFile(in, out, key, 64<<10); err != nil {
		t.Fatal(err)
	}
	if err := VerifyKey(out, key); err != nil {
		t.Fatalf("correct key rejected: %v", err)
	}
	if err := VerifyKey(out, testKey(t)); err == nil {
		t.Fatal("wrong key accepted")
	}
	empty := filepath.Join(dir, "e")
	os.WriteFile(empty, nil, 0600)
	emptyOut := filepath.Join(dir, "eo")
	if err := EncryptFile(empty, emptyOut, key, 64<<10); err != nil {
		t.Fatal(err)
	}
	if err := VerifyKey(emptyOut, testKey(t)); err != nil {
		t.Fatalf("empty file should verify against any key: %v", err)
	}
}

func TestWrongKey(t *testing.T) {
	data := make([]byte, 70<<10)
	rand.Read(data)
	dir := t.TempDir()
	in, out := filepath.Join(dir, "i"), filepath.Join(dir, "o")
	os.WriteFile(in, data, 0600)
	if err := EncryptFile(in, out, testKey(t), 64<<10); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	h, _ := ReadHeader(f)
	var buf bytes.Buffer
	if err := StreamRange(&buf, f, h, testKey(t), 0, 100); err == nil {
		t.Fatal("expected auth failure with wrong key")
	}
}
