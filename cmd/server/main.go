// Command server streams chunked-encrypted (*.cenc) videos over HTTP with
// Range support. The AES key never leaves the server.
//
// The server can boot locked (no key in memory). Unlock by pasting the
// master key into the web UI (POST /api/unlock); the key lives only in RAM
// until explicit lock (POST /api/lock) or the idle timeout fires.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"streaming/internal/chunked"
	"streaming/internal/config"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const sessionCookie = "sess"

// keyStore holds the master key in RAM only, plus one session token.
// Zero value = locked.
type keyStore struct {
	mu         sync.Mutex
	key        []byte // nil when locked
	session    []byte // valid only when key != nil
	open       bool   // key came from boot config: no session cookie needed
	lastActive time.Time
	idleLock   time.Duration // 0 = never auto-lock
}

func (k *keyStore) locked() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.key == nil
}

// use returns a copy of the key and marks activity, or nil when locked
// (including when the idle timeout has fired, which locks first).
func (k *keyStore) use() []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key == nil {
		return nil
	}
	if k.idleLock > 0 && time.Since(k.lastActive) > k.idleLock {
		k.wipeLocked()
		return nil
	}
	k.lastActive = time.Now()
	out := make([]byte, len(k.key))
	copy(out, k.key)
	return out
}

// authed reports whether r is authorized: always true in open mode,
// otherwise only with the current session token. Success touches activity
// so watching keeps the session alive.
func (k *keyStore) authed(r *http.Request) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key == nil {
		return false
	}
	if k.idleLock > 0 && time.Since(k.lastActive) > k.idleLock {
		k.wipeLocked()
		return false
	}
	if k.open {
		k.lastActive = time.Now()
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	tok, err := hex.DecodeString(strings.TrimSpace(c.Value))
	if err != nil {
		return false
	}
	if len(k.session) == 0 || subtle.ConstantTimeCompare(tok, k.session) != 1 {
		return false
	}
	k.lastActive = time.Now()
	return true
}

// unlock stores key (copied), mints a fresh session token, and returns it.
// Callers should verify the key first with VerifyKey.
func (k *keyStore) unlock(key []byte) []byte {
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		panic(err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.wipeLocked()
	k.key = append([]byte(nil), key...)
	k.session = tok
	k.lastActive = time.Now()
	return tok
}

// lock wipes the key and invalidates the session.
func (k *keyStore) lock() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.wipeLocked()
}

func (k *keyStore) wipeLocked() {
	for i := range k.key {
		k.key[i] = 0
	}
	k.key = nil
	k.session = nil
	k.open = false
}

type Server struct {
	videosDir string
	ks        *keyStore
	idleMin   int
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (keep localhost)")
	videosDir := flag.String("videos", config.VideosDir(), "directory with *.cenc files (env VIDEOS_DIR)")
	keyFile := flag.String("key-file", "", "path to keys.json (else env STREAM_KEY_HEX or ./keys.json)")
	lockedAtStart := flag.Bool("locked-at-start", false, "boot locked even if a key is configured")
	idleLock := flag.Int("idle-lock", 30, "auto-lock after N minutes without requests (0 disables)")
	flag.Parse()

	ks := &keyStore{idleLock: time.Duration(*idleLock) * time.Minute}
	if !*lockedAtStart {
		if key, src, err := config.LoadKey(*keyFile); err == nil {
			ks.unlock(key)
			ks.mu.Lock()
			ks.open = true // key from boot config: no session cookie needed
			ks.mu.Unlock()
			for i := range key {
				key[i] = 0
			}
			log.Printf("key loaded from %s (unlocked, open mode)", src)
		} else {
			log.Printf("no key: %v — starting LOCKED", err)
		}
	} else {
		log.Print("starting LOCKED (--locked-at-start)")
	}

	s := &Server{videosDir: *videosDir, ks: ks, idleMin: *idleLock}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/unlock", s.handleUnlock)
	mux.HandleFunc("POST /api/lock", s.handleLock)
	mux.HandleFunc("GET /api/videos", s.handleList)
	mux.HandleFunc("GET /video/{name}", s.handleVideo)
	mux.HandleFunc("HEAD /video/{name}", s.handleVideo)
	mux.HandleFunc("GET /", s.handleIndex)

	log.Printf("serving %s on http://%s", *videosDir, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.ks.mu.Lock()
	locked, open := s.ks.key == nil, s.ks.open
	s.ks.mu.Unlock()
	writeJSON(w, 200, map[string]any{"locked": locked, "open": open, "idleLockMinutes": s.idleMin})
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyHex string `json:"keyHex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON, want {\"keyHex\": \"...\"}"})
		return
	}
	key, err := hex.DecodeString(strings.TrimSpace(body.KeyHex))
	if err != nil || len(key) != 32 {
		writeJSON(w, 400, map[string]string{"error": "key must be 64 hex chars (32 bytes)"})
		return
	}
	// Best-effort check against the library so typos fail fast.
	if first, ok := s.firstVideo(); ok {
		if err := chunked.VerifyKey(first, key); err != nil {
			for i := range key {
				key[i] = 0
			}
			writeJSON(w, 401, map[string]string{"error": "key does not match library"})
			return
		}
	}
	tok := s.ks.unlock(key)
	for i := range key {
		key[i] = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    hex.EncodeToString(tok),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	log.Print("unlocked via web")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleLock(w http.ResponseWriter, r *http.Request) {
	s.ks.lock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	log.Print("locked")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// firstVideo returns the path of any .cenc file, for unlock verification.
func (s *Server) firstVideo() (string, bool) {
	entries, err := os.ReadDir(s.videosDir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".cenc") {
			return filepath.Join(s.videosDir, e.Name()), true
		}
	}
	return "", false
}

// requireAuth gates handlers that need the key. It returns a private copy
// of the key (safe to hold past lock) or writes 401 and returns nil.
func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) []byte {
	if !s.ks.authed(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "locked"})
		return nil
	}
	key := s.ks.use()
	if key == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "locked"})
		return nil
	}
	return key
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join("web", "index.html"))
	if err != nil {
		http.Error(w, "web/index.html missing", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) resolve(name string) (string, bool) {
	base := strings.TrimSuffix(name, ".cenc")
	if !safeName.MatchString(base) {
		return "", false
	}
	p := filepath.Join(s.videosDir, base+".cenc")
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return "", false
	}
	return p, true
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	entries, err := os.ReadDir(s.videosDir)
	if err != nil {
		http.Error(w, "videos dir unreadable", 500)
		return
	}
	type item struct {
		Name string `json:"name"`
		Size int64  `json:"size"` // plaintext bytes
	}
	out := []item{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cenc") {
			continue
		}
		p := filepath.Join(s.videosDir, e.Name())
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		h, err := chunked.ReadHeader(f)
		f.Close()
		if err != nil {
			continue // skip partial/corrupt files being written
		}
		out = append(out, item{Name: strings.TrimSuffix(e.Name(), ".cenc"), Size: int64(h.PlaintextSize)})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// parseRange handles "bytes=start-", "bytes=start-end", "bytes=-suffix" (single range).
func parseRange(hdr string, size int64) (start, end int64, ok bool) {
	if hdr == "" {
		return 0, size - 1, false // no range header
	}
	if !strings.HasPrefix(hdr, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(hdr, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false // multipart not supported -> 416
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" { // suffix: last N bytes
		n, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if strings.TrimSpace(parts[1]) == "" {
		return start, size - 1, true
	}
	end, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}

func (s *Server) handleVideo(w http.ResponseWriter, r *http.Request) {
	key := s.requireAuth(w, r)
	if key == nil {
		return
	}
	p, ok := s.resolve(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	h, err := chunked.ReadHeader(f)
	if err != nil {
		http.Error(w, "corrupt video file", 500)
		return
	}
	size := int64(h.PlaintextSize)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Accept-Ranges", "bytes")

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		return
	}

	start, end, hasRange := parseRange(r.Header.Get("Range"), size)
	if r.Header.Get("Range") != "" && !hasRange {
		// Malformed or unsatisfiable.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "requested range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if !hasRange {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if err := chunked.StreamRange(w, f, h, key, 0, size-1); err != nil {
			log.Printf("stream %s: %v", p, err)
		}
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	if err := chunked.StreamRange(w, f, h, key, start, end); err != nil {
		log.Printf("range %s %d-%d: %v", p, start, end, err)
	}
}
