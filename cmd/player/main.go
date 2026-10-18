// Command player is a local, browser-free frontend for the streaming server.
//
// It unlocks the server (the key stays in server RAM), shows a full-screen
// terminal UI listing videos and images, and drives mpv to play them — mpv
// keeps its stream cache in RAM and writes nothing to disk, unlike a browser.
// The session cookie is handed to mpv over its IPC socket so it never appears
// in process arguments (the one-shot --play path is the documented exception).
package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	server := flag.String("server", "http://127.0.0.1:8080", "streaming server base URL")
	mpvBin := flag.String("mpv", "mpv", "mpv binary")
	vo := flag.String("vo", "", "mpv --vo value (e.g. null for headless test)")
	ao := flag.String("ao", "", "mpv --ao value (e.g. null for headless test)")
	play := flag.String("play", "", "one-shot: play this video and exit (no UI)")
	flag.Parse()

	extra := []string{}
	if *vo != "" {
		extra = append(extra, "--vo="+*vo)
	}
	if *ao != "" {
		extra = append(extra, "--ao="+*ao)
	}

	base := strings.TrimSuffix(*server, "/")

	mpvPath, err := exec.LookPath(*mpvBin)
	if err != nil {
		fatalf("mpv not found (%q). Install it: brew install mpv", *mpvBin)
	}

	httpc := newHTTPClient()
	st, err := getStatus(httpc, base)
	if err != nil {
		fatalf("%v", err)
	}

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

	items, err := listLibrary(httpc, base)
	if err != nil {
		failf("%v", err)
	}

	if *play != "" {
		it, err := findVideo(items, *play)
		if err != nil {
			failf("%v", err)
		}
		if it.Kind != kindVideo {
			failf("%q is an image, not a video", it.Name)
		}
		if err := oneShot(httpc, base, mpvPath, extra, it); err != nil {
			failf("%v", err)
		}
		lockIfNeeded(httpc, base, weUnlocked)
		return
	}

	if !isTTY(os.Stdin) {
		failf("player needs a terminal; use --play NAME for one-shot playback")
	}

	if err := runTUI(httpc, base, mpvPath, extra, items); err != nil {
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
