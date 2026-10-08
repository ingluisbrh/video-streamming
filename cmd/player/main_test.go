package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"
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
