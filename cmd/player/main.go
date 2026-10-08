// Command player is a local, browser-free frontend for the streaming server.
//
// It unlocks the server (key stays in server RAM), lists videos, and drives
// an mpv window — mpv keeps its stream cache in RAM and writes nothing to
// disk, unlike a browser. Interactive mode talks to mpv over IPC so the
// session cookie never appears in process arguments; one-shot --play passes
// it via argv instead (visible in `ps`, local user only).
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const sessionCookie = "sess"

type videoItem struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func main() {
	server := flag.String("server", "http://127.0.0.1:8080", "streaming server base URL")
	mpvBin := flag.String("mpv", "mpv", "mpv binary")
	vo := flag.String("vo", "", "mpv --vo value (e.g. null for headless test)")
	ao := flag.String("ao", "", "mpv --ao value (e.g. null for headless test)")
	play := flag.String("play", "", "one-shot: play this video and exit (no menu)")
	flag.Parse()

	extra := []string{}
	if *vo != "" {
		extra = append(extra, "--vo="+*vo)
	}
	if *ao != "" {
		extra = append(extra, "--ao="+*ao)
	}

	base := strings.TrimSuffix(*server, "/")
	jar, _ := cookiejar.New(nil)
	httpc := &http.Client{Jar: jar, Timeout: 15 * time.Second}

	mpvPath, err := exec.LookPath(*mpvBin)
	if err != nil {
		fatalf("mpv not found (%q). Install it: brew install mpv", *mpvBin)
	}

	st := getStatus(httpc, base)
	weUnlocked := false
	failf := func(format string, args ...any) {
		lockIfNeeded(httpc, base, weUnlocked) // never leave our session unlocked
		fatalf(format, args...)
	}
	if st.Locked {
		key := keyFromEnvOrPrompt()
		if err := unlock(httpc, base, key); err != nil {
			zero(key)
			fatalf("%v", err)
		}
		zero(key)
		weUnlocked = true
	}
	items, err := listVideos(httpc, base)
	if err != nil {
		failf("%v", err)
	}
	if len(items) == 0 {
		fmt.Println("No videos. Add one: ./scripts/add-video.sh file.mp4")
		lockIfNeeded(httpc, base, weUnlocked)
		return
	}

	if *play != "" {
		it, err := findVideo(items, *play)
		if err != nil {
			failf("%v", err)
		}
		if err := oneShot(httpc, base, mpvPath, extra, it); err != nil {
			failf("%v", err)
		}
		lockIfNeeded(httpc, base, weUnlocked)
		return
	}

	// Menu loop works on pipes too; only the hidden key prompt needs a TTY
	// (readSecret enforces that itself when STREAM_KEY_HEX is unset).
	if err := interactive(httpc, base, mpvPath, extra, items, weUnlocked); err != nil {
		failf("%v", err)
	}
	lockIfNeeded(httpc, base, weUnlocked)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "player: "+format+"\n", args...)
	os.Exit(1)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// --- server API ---

type statusResp struct {
	Locked bool `json:"locked"`
	Open   bool `json:"open"`
}

func getStatus(c *http.Client, base string) statusResp {
	resp, err := c.Get(base + "/api/status")
	if err != nil {
		fatalf("server %s unreachable (start it: ./scripts/run.sh): %v", base, err)
	}
	defer resp.Body.Close()
	var st statusResp
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		fatalf("bad /api/status: %v", err)
	}
	return st
}

func listVideos(c *http.Client, base string) ([]videoItem, error) {
	resp, err := c.Get(base + "/api/videos")
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("server locked itself (idle timeout?) — restart player")
	}
	var items []videoItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("bad /api/videos: %w", err)
	}
	return items, nil
}

func unlock(c *http.Client, base string, key []byte) error {
	body, _ := json.Marshal(map[string]string{"keyHex": hex.EncodeToString(key)})
	resp, err := c.Post(base+"/api/unlock", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unlock failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	fmt.Println("unlocked")
	return nil
}

func lockIfNeeded(c *http.Client, base string, weUnlocked bool) {
	if !weUnlocked {
		return // server was already open; leave it as found
	}
	req, _ := http.NewRequest("POST", base+"/api/lock", nil)
	if _, err := c.Do(req); err != nil {
		fmt.Fprintf(os.Stderr, "player: lock failed: %v\n", err)
		return
	}
	fmt.Println("locked")
}

// --- key input ---

func keyFromEnvOrPrompt() []byte {
	if env := strings.TrimSpace(os.Getenv("STREAM_KEY_HEX")); env != "" {
		key, err := hex.DecodeString(env)
		if err != nil || len(key) != 32 {
			fatalf("STREAM_KEY_HEX must be 64 hex chars")
		}
		return key
	}
	return readSecret("master key hex: ")
}

// readSecret reads one line without echoing it (stty-based, stdlib only).
func readSecret(prompt string) []byte {
	if !isTTY(os.Stdin) {
		fatalf("no STREAM_KEY_HEX and stdin is not a terminal")
	}
	fmt.Print(prompt)
	restore := func() {
		cmd := exec.Command("stty", "echo")
		cmd.Stdin = os.Stdin
		cmd.Run()
		fmt.Println()
	}
	cmd := exec.Command("stty", "-echo")
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fatalf("stty: %v", err)
	}
	defer restore()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(line) == 0 {
		fatalf("read: %v", err)
	}
	hexStr := strings.TrimSpace(line)
	key, err := hex.DecodeString(hexStr)
	if err != nil || len(key) != 32 {
		fatalf("key must be 64 hex chars (32 bytes)")
	}
	return key
}

// --- selection ---

// findVideo resolves "2" (1-based) or a name to an item.
func findVideo(items []videoItem, arg string) (videoItem, error) {
	arg = strings.TrimSpace(arg)
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(items) {
			return videoItem{}, fmt.Errorf("pick 1..%d", len(items))
		}
		return items[n-1], nil
	}
	for _, it := range items {
		if it.Name == arg {
			return it, nil
		}
	}
	return videoItem{}, fmt.Errorf("no video %q", arg)
}

func fmtSize(n int64) string {
	if n > 1e9 {
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	}
	return fmt.Sprintf("%d MB", n/1e6)
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

// --- mpv ---

// oneShot plays a single video in the foreground and returns when mpv exits.
// The cookie travels via argv here (visible in local `ps`); prefer
// interactive mode, which uses IPC instead.
func oneShot(c *http.Client, base, mpvPath string, extra []string, it videoItem) error {
	args := append(append([]string{}, extra...),
		"--http-header-fields=Cookie: sess="+sessCookie(c.Jar, base),
		base+"/video/"+url.PathEscape(it.Name))
	cmd := exec.Command(mpvPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	fmt.Printf("playing %s (%s)\n", it.Name, fmtSize(it.Size))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mpv: %w", err)
	}
	return nil
}

type ipc struct {
	conn net.Conn
	r    *bufio.Reader
}

func ipcConnect(sock string) (*ipc, error) {
	deadline := time.Now().Add(8 * time.Second)
	for {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			return &ipc{conn: conn, r: bufio.NewReader(conn)}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("mpv ipc %s: %v", sock, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// call sends {"command":[...]} and returns the data payload ("success" check).
func (c *ipc) call(args ...any) (any, error) {
	raw, _ := json.Marshal(map[string]any{"command": args})
	raw = append(raw, '\n')
	if _, err := c.conn.Write(raw); err != nil {
		return nil, err
	}
	line, err := c.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var reply struct {
		Error string `json:"error"`
		Data  any    `json:"data"`
	}
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		return nil, err
	}
	if reply.Error != "success" {
		return nil, fmt.Errorf("mpv: %s", reply.Error)
	}
	return reply.Data, nil
}

func (c *ipc) close() { c.conn.Close() }

func interactive(c *http.Client, base, mpvPath string, extra []string, items []videoItem, weUnlocked bool) error {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("stream-ipc-%d.sock", os.Getpid()))
	os.Remove(sock)
	args := append([]string{"--idle=yes", "--input-ipc-server=" + sock, "--input-terminal=no"}, extra...)
	cmd := exec.Command(mpvPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mpv start: %w", err)
	}
	// quit tears down mpv and reports the first error seen.
	quit := func() {
		ipcConn, err := ipcConnect(sock)
		if err == nil {
			ipcConn.call("quit")
			ipcConn.close()
		}
		cmd.Wait()
	}
	ipcConn, err := ipcConnect(sock)
	if err != nil {
		cmd.Process.Kill()
		return err
	}
	cookie := sessCookie(c.Jar, base)
	if cookie == "" {
		ipcConn.close()
		cmd.Process.Kill()
		return fmt.Errorf("no session (server locked itself?) — restart player")
	}
	// http-header-fields is a "String list": set as one comma-separated string.
	if _, err := ipcConn.call("set", "http-header-fields", "Cookie: sess="+cookie); err != nil {
		ipcConn.close()
		cmd.Process.Kill()
		return err
	}

	// Ctrl-C: quit mpv, lock our session if we made one, exit.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		quit()
		lockIfNeeded(c, base, weUnlocked)
		fmt.Println("\nbye")
		os.Exit(130)
	}()

	printList(items)
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("watch> ")
		line, err := in.ReadString('\n')
		if err != nil {
			break // EOF
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "quit", "q", "exit":
			quit()
			ipcConn.close()
			fmt.Println("bye")
			return nil
		case "list", "ls":
			fresh, err := listVideos(c, base)
			if err != nil {
				return err
			}
			items = fresh
			printList(items)
		case "play", "p":
			if len(fields) < 2 {
				fmt.Println("usage: play <number|name>")
				continue
			}
			it, err := findVideo(items, strings.Join(fields[1:], " "))
			if err != nil {
				fmt.Println(err)
				continue
			}
			if _, err := ipcConn.call("loadfile", base+"/video/"+url.PathEscape(it.Name), "replace"); err != nil {
				fmt.Println(err)
				continue
			}
			fmt.Printf("playing %s\n", it.Name)
		case "stop", "s":
			ipcConn.call("stop")
		case "lock":
			lockReq, _ := http.NewRequest("POST", base+"/api/lock", nil)
			c.Do(lockReq)
			fmt.Println("locked — restart player to watch again")
			quit()
			ipcConn.close()
			return nil
		case "help", "h", "?":
			fmt.Println("list | play <n|name> | stop | lock | quit")
		default:
			fmt.Println("unknown: try list, play <n>, stop, lock, quit")
		}
	}
	quit()
	ipcConn.close()
	return nil
}

func printList(items []videoItem) {
	for i, it := range items {
		fmt.Printf("%3d  %s (%s)\n", i+1, it.Name, fmtSize(it.Size))
	}
}
