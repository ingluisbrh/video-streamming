// api.go: HTTP client for the streaming server and the shared media types.
//
// The player never sees the master key beyond the initial unlock: the server
// mints a session cookie and the key stays in the server's RAM.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const sessionCookie = "sess"

// mediaKind distinguishes the two encrypted libraries the server exposes.
type mediaKind int

const (
	kindVideo mediaKind = iota
	kindImage
)

func (k mediaKind) endpoint() string {
	if k == kindImage {
		return "/api/images"
	}
	return "/api/videos"
}

func (k mediaKind) path() string {
	if k == kindImage {
		return "image"
	}
	return "video"
}

func (k mediaKind) label() string {
	if k == kindImage {
		return "image"
	}
	return "video"
}

// mediaItem is one entry of /api/videos or /api/images plus its kind. It is
// both the JSON payload and a bubbles list item.
type mediaItem struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Kind mediaKind `json:"-"`
}

// bubbles/list.Item + DefaultItem: what the list filters and renders.
func (i mediaItem) FilterValue() string { return i.Name }
func (i mediaItem) Title() string       { return i.Name }
func (i mediaItem) Description() string {
	return fmt.Sprintf("%s · %s", i.Kind.label(), fmtSize(i.Size))
}

func newHTTPClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 15 * time.Second}
}

type statusResp struct {
	Locked bool `json:"locked"`
	Open   bool `json:"open"`
	Idle   int  `json:"idleLockMinutes"`
}

func getStatus(c *http.Client, base string) (statusResp, error) {
	resp, err := c.Get(base + "/api/status")
	if err != nil {
		return statusResp{}, fmt.Errorf("server %s unreachable (start it: ./scripts/run.sh): %w", base, err)
	}
	defer resp.Body.Close()
	var st statusResp
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return statusResp{}, fmt.Errorf("bad /api/status: %w", err)
	}
	return st, nil
}

func listMedia(c *http.Client, base string, kind mediaKind) ([]mediaItem, error) {
	resp, err := c.Get(base + kind.endpoint())
	if err != nil {
		return nil, fmt.Errorf("list %ss: %w", kind.label(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("server locked itself (idle timeout?) — restart player")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %ss: %s", kind.label(), resp.Status)
	}
	var items []mediaItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("bad %s: %w", kind.endpoint(), err)
	}
	for i := range items {
		items[i].Kind = kind
	}
	return items, nil
}

// listLibrary fetches videos and images, videos first.
func listLibrary(c *http.Client, base string) ([]mediaItem, error) {
	vids, err := listMedia(c, base, kindVideo)
	if err != nil {
		return nil, err
	}
	imgs, err := listMedia(c, base, kindImage)
	if err != nil {
		return nil, err
	}
	return append(vids, imgs...), nil
}

func unlock(c *http.Client, base string, key []byte) error {
	body, _ := json.Marshal(map[string]string{"keyHex": hex.EncodeToString(key)})
	resp, err := c.Post(base+"/api/unlock", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unlock failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	fmt.Println("unlocked")
	return nil
}

func lock(c *http.Client, base string) error {
	req, _ := http.NewRequest("POST", base+"/api/lock", nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// lockIfNeeded locks the server only when this player created the session,
// leaving a server that booted open exactly as it was found.
func lockIfNeeded(c *http.Client, base string, weUnlocked bool) {
	if !weUnlocked {
		return
	}
	if err := lock(c, base); err != nil {
		fmt.Fprintf(os.Stderr, "player: lock failed: %v\n", err)
		return
	}
	fmt.Println("locked")
}

// sessCookie extracts our session cookie for base (for mpv auth).
func sessCookie(jar http.CookieJar, base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	for _, c := range jar.Cookies(u) {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

func cookieHeader(ck string) string {
	if ck == "" {
		return ""
	}
	return "Cookie: sess=" + ck
}

// mediaURL is the server URL mpv should fetch for an item.
func mediaURL(base string, it mediaItem) string {
	return base + "/" + it.Kind.path() + "/" + url.PathEscape(it.Name)
}

// findVideo resolves "2" (1-based) or a name to an item.
func findVideo(items []mediaItem, arg string) (mediaItem, error) {
	arg = strings.TrimSpace(arg)
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(items) {
			return mediaItem{}, fmt.Errorf("pick 1..%d", len(items))
		}
		return items[n-1], nil
	}
	for _, it := range items {
		if it.Name == arg {
			return it, nil
		}
	}
	return mediaItem{}, fmt.Errorf("no video %q", arg)
}

func fmtSize(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%d MB", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%d KB", n/1e3)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
