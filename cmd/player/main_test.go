package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFindVideo(t *testing.T) {
	items := []videoItem{{Name: "a", Size: 1}, {Name: "b c", Size: 2}}
	if it, err := findVideo(items, "1"); err != nil || it.Name != "a" {
		t.Fatalf("index: %v %v", it, err)
	}
	if it, err := findVideo(items, "b c"); err != nil || it.Name != "b c" {
		t.Fatalf("name: %v %v", it, err)
	}
	if _, err := findVideo(items, "0"); err == nil {
		t.Fatal("index 0 accepted")
	}
	if _, err := findVideo(items, "3"); err == nil {
		t.Fatal("out of range accepted")
	}
	if _, err := findVideo(items, "zzz"); err == nil {
		t.Fatal("unknown name accepted")
	}
}

func TestSessCookie(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("http://127.0.0.1:8080")
	jar.SetCookies(u, []*http.Cookie{{Name: "sess", Value: "abc123"}})
	if got := sessCookie(jar, "http://127.0.0.1:8080"); got != "abc123" {
		t.Fatalf("got %q", got)
	}
	if got := sessCookie(jar, "http://127.0.0.2:8080"); got != "" {
		t.Fatalf("wrong host gave %q", got)
	}
}

func TestIPCCall(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	go func() {
		r := bufio.NewReader(srv)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			var msg map[string]any
			json.Unmarshal([]byte(line), &msg)
			cmd := msg["command"].([]any)[0].(string)
			if cmd == "boom" {
				srv.Write([]byte("{\"error\":\"kaput\"}\n"))
			} else {
				srv.Write([]byte("{\"error\":\"success\",\"data\":42}\n"))
			}
		}
	}()
	c := &ipc{conn: cli, r: bufio.NewReader(cli)}
	if _, err := c.call("get_property", "playback-time"); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, err := c.call("boom"); err == nil {
		t.Fatal("expected mpv error")
	}
}

func TestIPCFraming(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan string, 1)
	go func() {
		r := bufio.NewReader(b)
		line, _ := r.ReadString('\n')
		done <- line
		b.Write([]byte("{\"error\":\"success\"}\n"))
	}()
	c := &ipc{conn: a, r: bufio.NewReader(a)}
	if _, err := c.call("set", "x", []string{"y"}); err != nil {
		t.Fatal(err)
	}
	line := <-done
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatal(err)
	}
	cmd := msg["command"].([]any)
	if cmd[0].(string) != "set" || cmd[1].(string) != "x" {
		t.Fatalf("bad frame: %s", line)
	}
}

// liveInstance builds an mpvInstance whose IPC side answers every command
// with success/42, like mpv does.
func liveInstance() (*mpvInstance, func()) {
	srv, cli := net.Pipe()
	go func() {
		r := bufio.NewReader(srv)
		for {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
			if _, err := srv.Write([]byte("{\"error\":\"success\",\"data\":42}\n")); err != nil {
				return
			}
		}
	}()
	m := &mpvInstance{
		conn: &ipc{conn: cli, r: bufio.NewReader(cli)},
		done: make(chan struct{}),
	}
	return m, func() { srv.Close(); cli.Close() }
}

// deadInstance simulates mpv after the window was closed: process reaped,
// IPC socket gone (writes fail), no process handle left to kill.
func deadInstance() *mpvInstance {
	srv, cli := net.Pipe()
	srv.Close()
	done := make(chan struct{})
	close(done)
	return &mpvInstance{conn: &ipc{conn: cli, r: bufio.NewReader(cli)}, done: done}
}

func TestMPVCallRespawnsAfterWindowClose(t *testing.T) {
	fresh, cleanup := liveInstance()
	defer cleanup()
	starts := 0
	c := &mpvCtl{start: func() (*mpvInstance, error) {
		starts++
		return fresh, nil
	}}
	c.cur = deadInstance() // user closed the mpv window

	got, err := c.call("loadfile", "http://x/v", "replace")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if starts != 1 {
		t.Fatalf("mpv starts = %d, want 1", starts)
	}
	if got != float64(42) {
		t.Fatalf("data = %v, want 42", got)
	}
}

func TestMPVCallRetriesWhenExitsMidCall(t *testing.T) {
	// mpv reads the command, then dies before replying (window closed while
	// the call was in flight); the reaper closes done right after.
	srv, cli := net.Pipe()
	m := &mpvInstance{conn: &ipc{conn: cli, r: bufio.NewReader(cli)}, done: make(chan struct{})}
	go func() {
		if _, err := bufio.NewReader(srv).ReadString('\n'); err == nil {
			srv.Close()
			close(m.done)
		}
	}()

	fresh, cleanup := liveInstance()
	defer cleanup()
	starts := 0
	c := &mpvCtl{start: func() (*mpvInstance, error) {
		starts++
		return fresh, nil
	}}
	c.cur = m

	got, err := c.call("loadfile", "http://x/v", "replace")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if starts != 1 {
		t.Fatalf("mpv starts = %d, want 1", starts)
	}
	if got != float64(42) {
		t.Fatalf("data = %v, want 42", got)
	}
}

func TestMPVStopPlaybackNeverRespawns(t *testing.T) {
	starts := 0
	c := &mpvCtl{start: func() (*mpvInstance, error) {
		starts++
		t.Fatal("start() called for a dead mpv on 'stop'")
		return nil, nil
	}}
	c.cur = deadInstance()
	c.stopPlayback()
	if starts != 0 {
		t.Fatalf("mpv starts = %d, want 0", starts)
	}
}

func TestMPVInstanceStopOnDeadIsNoop(t *testing.T) {
	m := deadInstance()
	done := make(chan struct{})
	go func() { m.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() hung on an already-dead mpv (nil cmd)")
	}
	if m.alive() {
		t.Fatal("dead instance reported alive")
	}
}

// TestMPVRespawnsOnRealMPV reproduces the reported bug end-to-end: close the
// mpv "window" (kill the process) and issue another play command.
func TestMPVRespawnsOnRealMPV(t *testing.T) {
	mpvPath, err := exec.LookPath("mpv")
	if err != nil {
		t.Skip("mpv not installed")
	}
	sock := filepath.Join(t.TempDir(), "ipc.sock")
	starts := 0
	start := func() (*mpvInstance, error) {
		starts++
		return startMPV(mpvPath, sock, []string{"--vo=null", "--ao=null"}, "Cookie: sess=test")
	}
	c := &mpvCtl{start: start}
	if err := c.spawn(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer c.stop()
	first := c.cur

	// The user closes the mpv window: the process exits and the socket dies.
	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !first.waitDead(5 * time.Second) {
		t.Fatal("mpv did not exit after kill")
	}

	// The next menu command must respawn mpv instead of hitting a dead pipe.
	if _, err := c.call("get_property", "idle-active"); err != nil {
		t.Fatalf("call after window close: %v", err)
	}
	if starts != 2 {
		t.Fatalf("mpv starts = %d, want 2", starts)
	}
	if c.cur == first {
		t.Fatal("mpv was not respawned")
	}
}
