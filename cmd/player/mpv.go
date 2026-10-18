// mpv.go: driving the mpv process over its JSON IPC socket.
//
// The session cookie is installed as an mpv HTTP header over IPC so it never
// appears in process arguments (the one-shot --play path is the exception, and
// is documented as such).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type ipc struct {
	conn net.Conn
	r    *bufio.Reader
	req  int64 // last request_id sent (calls are serialized by the caller)
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
//
// mpv interleaves asynchronous {"event":...} objects with command replies, so
// each request carries a request_id and event/non-matching lines are skipped.
func (c *ipc) call(args ...any) (any, error) {
	c.req++
	raw, _ := json.Marshal(map[string]any{"command": args, "request_id": c.req})
	raw = append(raw, '\n')
	if _, err := c.conn.Write(raw); err != nil {
		return nil, err
	}
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		var reply struct {
			Error     string `json:"error"`
			Data      any    `json:"data"`
			Event     string `json:"event"`
			RequestID *int64 `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			return nil, err
		}
		if reply.Event != "" {
			continue // asynchronous event, not our reply
		}
		if reply.RequestID != nil && *reply.RequestID != c.req {
			continue // reply to some other request
		}
		if reply.Error != "success" {
			return nil, fmt.Errorf("mpv: %s", reply.Error)
		}
		return reply.Data, nil
	}
}

func (c *ipc) close() { c.conn.Close() }

// exiting flips on when the player is on its way out so a racing command
// cannot respawn mpv in the last microseconds before exit.
var exiting atomic.Bool

// mpvInstance is one running mpv process plus its IPC connection. done is
// closed as soon as the process has been reaped (window closed, quit, crash).
type mpvInstance struct {
	cmd  *exec.Cmd
	conn *ipc
	done chan struct{}
}

// startMPV spawns mpv in idle mode, connects to its IPC socket and installs
// the session cookie as an mpv HTTP header (kept out of process arguments).
// --keep-open=yes keeps the window (and still images) on screen at EOF.
func startMPV(mpvPath, sock string, extra []string, header string) (*mpvInstance, error) {
	os.Remove(sock) // stale socket from a previous mpv
	args := append([]string{"--idle=yes", "--input-ipc-server=" + sock, "--input-terminal=no", "--keep-open=yes"}, extra...)
	cmd := exec.Command(mpvPath, args...)
	// No stdio: the TUI owns the terminal, so mpv's chatter is discarded
	// rather than splattered across the alternate screen.
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mpv start: %w", err)
	}
	m := &mpvInstance{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(m.done) }()
	conn, err := ipcConnect(sock)
	if err != nil {
		m.kill()
		return nil, err
	}
	// http-header-fields is a "String list": set as one comma-separated string.
	// Empty header = open mode (no cookie); mpv needs none.
	if header != "" {
		if _, err := conn.call("set", "http-header-fields", header); err != nil {
			conn.close()
			m.kill()
			return nil, err
		}
	}
	m.conn = conn
	return m, nil
}

func (m *mpvInstance) alive() bool {
	select {
	case <-m.done:
		return false
	default:
		return true
	}
}

// waitDead reports whether mpv exited within timeout.
func (m *mpvInstance) waitDead(timeout time.Duration) bool {
	select {
	case <-m.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// kill reaps mpv unconditionally; used only when startup failed halfway.
func (m *mpvInstance) kill() {
	m.cmd.Process.Kill()
	<-m.done
}

// stop asks mpv to quit — a no-op if its window was already closed — and
// waits for the process, killing it if it lingers.
func (m *mpvInstance) stop() {
	if m.alive() {
		m.conn.call("quit") // error ignored: mpv may be exiting already
	}
	if !m.waitDead(3 * time.Second) {
		m.cmd.Process.Kill()
		m.waitDead(3 * time.Second)
	}
}

// mpvCtl owns the mpv process behind the menu. Closing the mpv window quits
// mpv, so commands go through call(), which transparently respawns mpv (and
// reinstalls the cookie header) instead of writing to a dead socket.
type mpvCtl struct {
	mu    sync.Mutex
	cur   *mpvInstance
	start func() (*mpvInstance, error)
}

// spawn replaces the current mpv instance (stopping the old one first).
func (c *mpvCtl) spawn() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spawnLocked()
}

func (c *mpvCtl) spawnLocked() error {
	if exiting.Load() {
		return fmt.Errorf("player is exiting")
	}
	if c.cur != nil {
		c.cur.stop()
		c.cur.conn.close()
	}
	m, err := c.start()
	if err != nil {
		return err
	}
	c.cur = m
	return nil
}

// call sends a command, respawning mpv first if the window was closed and
// retrying once if mpv dies mid-call.
func (c *mpvCtl) call(args ...any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil || !c.cur.alive() {
		if err := c.spawnLocked(); err != nil {
			return nil, err
		}
	}
	data, err := c.cur.conn.call(args...)
	if err == nil {
		return data, nil
	}
	// mpv may have exited between the check and the reply; give it a moment
	// to be reaped, then restart and retry the command once.
	if c.cur.waitDead(time.Second) {
		if serr := c.spawnLocked(); serr != nil {
			return nil, fmt.Errorf("%v (mpv restart: %v)", err, serr)
		}
		return c.cur.conn.call(args...)
	}
	return nil, err
}

// stopPlayback stops playback only while mpv is running — it never respawns.
func (c *mpvCtl) stopPlayback() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil && c.cur.alive() {
		c.cur.conn.call("stop")
	}
}

// alive reports whether an mpv process is currently running. It never spawns.
func (c *mpvCtl) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur != nil && c.cur.alive()
}

// setPause pauses/resumes only while mpv is running — it never respawns.
func (c *mpvCtl) setPause(paused bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil || !c.cur.alive() {
		return nil
	}
	_, err := c.cur.conn.call("set_property", "pause", paused)
	return err
}

// stop tears mpv down and waits for the process to exit.
func (c *mpvCtl) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil {
		return
	}
	c.cur.stop()
	c.cur.conn.close()
	c.cur = nil
}

// mpvStatus is the subset of playback state the UI shows.
type mpvStatus struct {
	Title    string
	Position float64
	Duration float64
	Paused   bool
}

// status reports playback state without spawning mpv. ok is false when mpv is
// not running or has no file loaded (idle).
func (c *mpvCtl) status() (mpvStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil || !c.cur.alive() {
		return mpvStatus{}, false
	}
	dv, err := c.cur.conn.call("get_property", "duration")
	if err != nil {
		return mpvStatus{}, false // idle: no file loaded
	}
	var st mpvStatus
	st.Duration, _ = floatProp(dv)
	if v, err := c.cur.conn.call("get_property", "time-pos"); err == nil {
		st.Position, _ = floatProp(v)
	}
	if v, err := c.cur.conn.call("get_property", "pause"); err == nil {
		st.Paused, _ = v.(bool)
	}
	if v, err := c.cur.conn.call("get_property", "media-title"); err == nil {
		if s, ok := v.(string); ok {
			st.Title = s
		}
	}
	return st, true
}

// floatProp coerces an mpv JSON value to float64. mpv reports infinite (still
// image) durations as the string "inf", which we treat as unknown.
func floatProp(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		if n == "inf" || n == "-inf" {
			return 0, false
		}
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// oneShot plays a single video in the foreground and returns when mpv exits.
// The cookie travels via argv here (visible in local `ps`); prefer the TUI,
// which installs it over IPC instead.
func oneShot(c *http.Client, base, mpvPath string, extra []string, it mediaItem) error {
	args := append([]string{}, extra...)
	// Open mode has no session cookie; only attach one when we hold it.
	if ck := sessCookie(c.Jar, base); ck != "" {
		args = append(args, "--http-header-fields="+cookieHeader(ck))
	}
	args = append(args, mediaURL(base, it))
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
