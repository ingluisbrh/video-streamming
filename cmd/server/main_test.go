package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	return &Server{videosDir: dir, ks: &keyStore{}}, key, "vid"
}

func mux(s *Server) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/status", s.handleStatus)
	m.HandleFunc("POST /api/unlock", s.handleUnlock)
	m.HandleFunc("POST /api/lock", s.handleLock)
	m.HandleFunc("GET /api/videos", s.handleList)
	m.HandleFunc("GET /video/{name}", s.handleVideo)
	return m
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
