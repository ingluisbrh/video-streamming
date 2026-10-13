package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"streaming/internal/chunked"
)

// fixture builds a Server over a temp videos dir holding one valid .cenc.
func fixture(t *testing.T) (*Server, []byte, string) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	plain := make([]byte, 200<<10)
	rand.Read(plain)
	in := filepath.Join(dir, "raw")
	if err := os.WriteFile(in, plain, 0600); err != nil {
		t.Fatal(err)
	}
	if err := chunked.EncryptFile(in, filepath.Join(dir, "vid.cenc"), key, 64<<10); err != nil {
		t.Fatal(err)
	}
	return &Server{videosDir: dir, imagesDir: t.TempDir(), ks: &keyStore{}}, key, "vid"
}

// buildImage encrypts plain into s.imagesDir/<name>.cenc.
func buildImage(t *testing.T, s *Server, key []byte, name string, plain []byte) {
	t.Helper()
	in := filepath.Join(s.imagesDir, "raw.bin")
	if err := os.WriteFile(in, plain, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(in)
	if err := chunked.EncryptFile(in, filepath.Join(s.imagesDir, name+".cenc"), key, 64<<10); err != nil {
		t.Fatal(err)
	}
}

// mux is the real route table, so tests can never drift from main.
func mux(s *Server) *http.ServeMux {
	return newMux(s)
}

func TestLockUnlockFlow(t *testing.T) {
	s, key, name := fixture(t)
	m := mux(s)

	// Locked at boot.
	if body := get(t, m, "/api/status", ""); !strings.Contains(body, `"locked":true`) {
		t.Fatalf("expected locked status, got %s", body)
	}
	if code := getCode(t, m, "/api/videos", ""); code != 401 {
		t.Fatalf("videos while locked = %d, want 401", code)
	}

	// Bad hex.
	if code := postJSON(t, m, "/api/unlock", `{"keyHex":"zz"}`, ""); code != 400 {
		t.Fatalf("bad hex unlock = %d, want 400", code)
	}
	// Wrong key (library non-empty so it must be rejected).
	wrong := make([]byte, 32)
	rand.Read(wrong)
	if code := postJSON(t, m, "/api/unlock", `{"keyHex":"`+hex.EncodeToString(wrong)+`"}`, ""); code != 401 {
		t.Fatalf("wrong key unlock = %d, want 401", code)
	}
	if code := getCode(t, m, "/api/videos", ""); code != 401 {
		t.Fatalf("videos after failed unlock = %d, want 401", code)
	}

	// Correct key.
	cookie := postUnlock(t, m, hex.EncodeToString(key))
	if body := get(t, m, "/api/status", cookie); !strings.Contains(body, `"locked":false`) {
		t.Fatalf("expected unlocked status, got %s", body)
	}
	list := get(t, m, "/api/videos", cookie)
	var items []map[string]any
	if err := json.Unmarshal([]byte(list), &items); err != nil || len(items) != 1 {
		t.Fatalf("list = %s, %v", list, err)
	}

	// Range while authed.
	req := httptest.NewRequest("GET", "/video/"+name, nil)
	req.Header.Set("Range", "bytes=0-1023")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != 206 || rec.Body.Len() != 1024 {
		t.Fatalf("range = %d len %d, want 206/1024", rec.Code, rec.Body.Len())
	}

	// Lock again; old cookie dies.
	postLock(t, m, cookie)
	if code := getCode(t, m, "/api/videos", cookie); code != 401 {
		t.Fatalf("videos after lock = %d, want 401", code)
	}
}

func TestOpenMode(t *testing.T) {
	s, key, name := fixture(t)
	s.ks.unlock(key)
	s.ks.open = true
	m := mux(s)

	// No cookie needed in open mode (key came from boot config).
	if body := get(t, m, "/api/status", ""); !strings.Contains(body, `"locked":false`) {
		t.Fatalf("expected unlocked status, got %s", body)
	}
	if code := getCode(t, m, "/api/videos", ""); code != 200 {
		t.Fatalf("videos in open mode = %d, want 200", code)
	}

	// Lock drops back to locked; old behavior resumes.
	postLock(t, m, "")
	if code := getCode(t, m, "/api/videos", ""); code != 401 {
		t.Fatalf("videos after lock = %d, want 401", code)
	}
	cookie := postUnlock(t, m, hex.EncodeToString(key))
	if code := getCode(t, m, "/video/"+name, cookie); code != 200 {
		t.Fatalf("video after web unlock = %d, want 200", code)
	}
}

func TestIdleAutoLock(t *testing.T) {
	s, key, _ := fixture(t)
	s.ks.idleLock = time.Nanosecond
	m := mux(s)
	cookie := postUnlock(t, m, hex.EncodeToString(key))
	time.Sleep(2 * time.Millisecond)
	if code := getCode(t, m, "/api/videos", cookie); code != 401 {
		t.Fatalf("videos after idle = %d, want 401", code)
	}
	if body := get(t, m, "/api/status", ""); !strings.Contains(body, `"locked":true`) {
		t.Fatalf("expected re-locked status, got %s", body)
	}
}

func TestSniffImage(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}, "image/jpeg"},
		{"png", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}, "image/png"},
		{"gif87a", []byte("GIF87a..."), "image/gif"},
		{"gif89a", []byte("GIF89a..."), "image/gif"},
		{"webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"avif", []byte("\x00\x00\x00\x20ftypavif"), "image/avif"},
		{"heif", []byte("\x00\x00\x00\x20ftypmif1"), "image/heif"},
		{"bmp", []byte("BM\x36\x00\x00\x00"), "image/bmp"},
		{"unknown ftyp brand", []byte("\x00\x00\x00\x20ftypmp42"), ""},
		{"text", []byte("hello world"), ""},
		{"empty", nil, ""},
		{"jpeg truncated", []byte{0xFF, 0xD8}, ""},
		{"png truncated", []byte{0x89, 'P'}, ""},
		{"webp truncated", []byte("RIFF\x00\x00\x00\x00WE"), ""},
		{"ftyp brand truncated", []byte("\x00\x00\x00\x20ftypav"), ""},
	}
	for _, c := range cases {
		if got := sniffImage(c.in); got != c.want {
			t.Errorf("sniffImage(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestImageServe(t *testing.T) {
	s, key, _ := fixture(t)
	m := mux(s)

	jpeg := make([]byte, 4096)
	copy(jpeg, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01})
	rand.Read(jpeg[12:])
	buildImage(t, s, key, "photo.jpg", jpeg)

	// Locked: both list and bytes are refused.
	if code := getCode(t, m, "/api/images", ""); code != 401 {
		t.Fatalf("images while locked = %d, want 401", code)
	}
	if code := getCode(t, m, "/image/photo.jpg", ""); code != 401 {
		t.Fatalf("image while locked = %d, want 401", code)
	}

	// Unlocked (open mode).
	s.ks.unlock(key)
	s.ks.open = true

	list := get(t, m, "/api/images", "")
	var items []cencItem
	if err := json.Unmarshal([]byte(list), &items); err != nil || len(items) != 1 {
		t.Fatalf("images list = %s, %v", list, err)
	}
	if items[0].Name != "photo.jpg" || items[0].Size != int64(len(jpeg)) {
		t.Fatalf("item = %+v, want photo.jpg/%d", items[0], len(jpeg))
	}
	if body := get(t, m, "/api/videos", ""); strings.Contains(body, "photo.jpg") {
		t.Fatalf("video list leaked an image: %s", body)
	}

	// Full fetch: correct Content-Type and byte-identical plaintext.
	req := httptest.NewRequest("GET", "/image/photo.jpg", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET image = %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type = %q, want image/jpeg", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), jpeg) {
		t.Fatalf("plaintext differs from original (got %d bytes, want %d)", rec.Body.Len(), len(jpeg))
	}

	// Range still works on images.
	rng := httptest.NewRequest("GET", "/image/photo.jpg", nil)
	rng.Header.Set("Range", "bytes=0-3")
	rrec := httptest.NewRecorder()
	m.ServeHTTP(rrec, rng)
	if rrec.Code != 206 || !bytes.Equal(rrec.Body.Bytes(), jpeg[:4]) {
		t.Fatalf("range = %d %x, want 206 %x", rrec.Code, rrec.Body.Bytes(), jpeg[:4])
	}

	// HEAD reports size with no body.
	head := httptest.NewRequest("HEAD", "/image/photo.jpg", nil)
	hrec := httptest.NewRecorder()
	m.ServeHTTP(hrec, head)
	if hrec.Code != 200 || hrec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d len %d, want 200/0", hrec.Code, hrec.Body.Len())
	}
	if cl := hrec.Header().Get("Content-Length"); cl != strconv.Itoa(len(jpeg)) {
		t.Fatalf("HEAD content-length = %q, want %d", cl, len(jpeg))
	}

	// Unknown names never escape the images dir.
	if code := getCode(t, m, "/image/..%2f..%2fkeys", ""); code != 404 {
		t.Fatalf("traversal = %d, want 404", code)
	}
}

func TestImageContentTypeFallsBackToExtension(t *testing.T) {
	s, key, _ := fixture(t)
	// Plaintext with no recognized magic: the stored extension decides.
	plain := []byte("plain text with no image signature at all")
	buildImage(t, s, key, "note.png", plain)
	got, err := s.imageContentType(filepath.Join(s.imagesDir, "note.png.cenc"), "note.png", key)
	if err != nil {
		t.Fatal(err)
	}
	if got != "image/png" {
		t.Fatalf("content-type = %q, want image/png", got)
	}
}

func TestListRequiresAuthForImages(t *testing.T) {
	s, key, _ := fixture(t)
	m := mux(s)
	buildImage(t, s, key, "a.jpg", []byte{0xFF, 0xD8, 0xFF, 0x00})
	if code := getCode(t, m, "/api/images", ""); code != 401 {
		t.Fatalf("images locked = %d, want 401", code)
	}
	cookie := postUnlock(t, m, hex.EncodeToString(key))
	if code := getCode(t, m, "/api/images", cookie); code != 200 {
		t.Fatalf("images unlocked = %d, want 200", code)
	}
}

func TestUnlockVerifiesAgainstImages(t *testing.T) {
	// No videos at all: unlock must still verify the key — against an image.
	s, key, _ := fixture(t)
	s.videosDir = t.TempDir() // empty, so firstCenc() finds only the image
	m := mux(s)
	buildImage(t, s, key, "a.jpg", []byte{0xFF, 0xD8, 0xFF, 0x00})

	if code := postJSON(t, m, "/api/unlock", `{"keyHex":"`+hex.EncodeToString(key)+`"}`, ""); code != 200 {
		t.Fatalf("unlock with image-only library = %d, want 200", code)
	}
	wrong := make([]byte, 32)
	rand.Read(wrong)
	if code := postJSON(t, m, "/api/unlock", `{"keyHex":"`+hex.EncodeToString(wrong)+`"}`, ""); code != 401 {
		t.Fatalf("wrong key vs image-only library = %d, want 401", code)
	}
}

func get(t *testing.T, m http.Handler, path, cookie string) string {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	return rec.Body.String()
}

func getCode(t *testing.T, m http.Handler, path, cookie string) int {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec.Code
}

func postJSON(t *testing.T, m http.Handler, path, body, cookie string) int {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec.Code
}

func postUnlock(t *testing.T, m http.Handler, keyHex string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/unlock", strings.NewReader(`{"keyHex":"`+keyHex+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("unlock = %d (%s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("no session cookie set")
	return ""
}

func postLock(t *testing.T, m http.Handler, cookie string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/lock", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("lock = %d", rec.Code)
	}
}
