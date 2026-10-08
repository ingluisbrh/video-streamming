// Command server streams chunked-encrypted (*.cenc) videos over HTTP with
// Range support. The AES key never leaves the server.
package main

import (
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

	"streaming/internal/chunked"
	"streaming/internal/config"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type Server struct {
	videosDir string
	key       []byte
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (keep localhost)")
	videosDir := flag.String("videos", "videos", "directory with *.cenc files")
	keyFile := flag.String("key-file", "", "path to keys.json (else env STREAM_KEY_HEX or ./keys.json)")
	flag.Parse()

	key, src, err := config.LoadKey(*keyFile)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	log.Printf("key loaded from %s", src)

	s := &Server{videosDir: *videosDir, key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/videos", s.handleList)
	mux.HandleFunc("GET /video/{name}", s.handleVideo)
	mux.HandleFunc("HEAD /video/{name}", s.handleVideo)
	mux.HandleFunc("GET /", s.handleIndex)

	log.Printf("serving %s on http://%s", *videosDir, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
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
		return 0, 0, false // multipart not supported; caller serves full
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
		if err := chunked.StreamRange(w, f, h, s.key, 0, size-1); err != nil {
			log.Printf("stream %s: %v", p, err)
		}
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	if err := chunked.StreamRange(w, f, h, s.key, start, end); err != nil {
		log.Printf("range %s %d-%d: %v", p, start, end, err)
	}
}
