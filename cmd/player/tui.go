// tui.go: the full-screen terminal UI (Bubble Tea).
//
// It is a library browser and remote control: videos/images are listed and
// filtered, and mpv does the actual playback. Two playback surfaces are
// available — mpv's own window (default, progress polled over IPC) and the
// current terminal via mpv's --vo=tct (toggled with "t"), which hands the
// terminal to mpv for the duration.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/progress"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const pollInterval = 500 * time.Millisecond

type playbackMode int

const (
	modeWindow playbackMode = iota
	modeTerminal
)

func (p playbackMode) String() string {
	if p == modeTerminal {
		return "terminal"
	}
	return "window"
}

var (
	mutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	playingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#5ee08a"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff7070"))
)

// --- messages ---

type tickMsg struct{}
type noopMsg struct{}
type playedMsg struct{}
type mpvDoneMsg struct{ err error }
type errMsg struct{ err error }
type statusMsg struct {
	st mpvStatus
	ok bool
}
type itemsMsg struct {
	items []mediaItem
	err   error
}

// --- model ---

type model struct {
	client  *http.Client
	base    string
	mpvPath string
	extra   []string
	ctl     *mpvCtl

	list list.Model
	bar  progress.Model

	mode      playbackMode
	playing   string
	status    mpvStatus
	hasStatus bool
	polling   bool
	note      string

	width, height int
}

func newModel(client *http.Client, base, mpvPath string, extra []string, items []mediaItem, ctl *mpvCtl) model {
	delegate := list.NewDefaultDelegate()
	delegate.ShowDescription = true
	l := list.New(toListItems(items), delegate, 0, 0)
	l.Title = "Private Stream"
	l.SetShowHelp(false)
	l.SetShowStatusBar(true)
	l.SetStatusBarItemName("item", "items")
	l.DisableQuitKeybindings()

	return model{
		client:  client,
		base:    base,
		mpvPath: mpvPath,
		extra:   extra,
		ctl:     ctl,
		list:    l,
		bar:     progress.New(progress.WithDefaultBlend()),
		mode:    modeWindow,
	}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		// fall through to the list below so it can resize too

	case tea.KeyPressMsg:
		if cmd, handled := m.handleKey(msg.String()); handled {
			return m, cmd
		}

	case tickMsg:
		if m.polling {
			return m, tea.Batch(m.pollCmd(), nextTick())
		}
		return m, nil

	case statusMsg:
		m.hasStatus = msg.ok
		if msg.ok {
			m.status = msg.st
		}
		// Duration is unavailable until mpv has parsed the stream, so an
		// early miss is not "playback stopped" — only a dead process is.
		if !m.ctl.alive() {
			m.polling, m.playing, m.hasStatus = false, "", false
		}
		return m, nil

	case playedMsg:
		m.polling, m.note = true, ""
		return m, nextTick()

	case mpvDoneMsg:
		m.playing, m.polling = "", false
		if msg.err != nil {
			m.note = msg.err.Error()
		}
		return m, nil

	case errMsg:
		m.playing, m.polling = "", false
		m.note = msg.err.Error()
		return m, nil

	case itemsMsg:
		if msg.err != nil {
			m.note = msg.err.Error()
			return m, nil
		}
		m.list.SetItems(toListItems(msg.items))
		m.note = ""
		return m, nil

	case noopMsg:
		return m, nil
	}

	// Navigation, filtering and resize bookkeeping are the list's job.
	l, cmd := m.list.Update(msg)
	m.list = l
	return m, cmd
}

func (m model) View() tea.View {
	var b strings.Builder
	b.WriteString(m.list.View())
	b.WriteString("\n")
	b.WriteString(m.statusLine())
	b.WriteString("\n")
	b.WriteString(m.helpLine())
	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

func (m *model) resize() {
	// reserve the status + help lines
	h := max(m.height-2, 3)
	w := max(m.width, 10)
	m.list.SetSize(w, h)
	m.bar.SetWidth(min(w-40, 44))
}

// handleKey runs the player's own keys. It returns handled=false so unhandled
// keys (navigation, filter typing) flow on to the list component.
func (m *model) handleKey(key string) (tea.Cmd, bool) {
	if key == "ctrl+c" {
		return tea.Quit, true // always quit, even while typing a filter
	}
	if m.list.SettingFilter() {
		return nil, false
	}
	switch key {
	case "q":
		return tea.Quit, true
	case "enter":
		return m.playSelected(), true
	case "space":
		if m.mode == modeWindow {
			return m.togglePause(), true
		}
		return nil, true // terminal playback: mpv owns the keys
	case "s":
		m.stop()
		return nil, true
	case "t":
		return m.toggleMode(), true
	case "r":
		m.note = ""
		return refreshCmd(m.client, m.base), true
	}
	return nil, false
}

func (m *model) playSelected() tea.Cmd {
	sel, ok := m.list.SelectedItem().(mediaItem)
	if !ok {
		return nil
	}
	if m.mode == modeTerminal {
		return m.terminalPlay(sel)
	}
	// Window mode: load into the idle mpv; playSelected returns immediately and
	// the (possibly slow) spawn runs in the command goroutine.
	m.playing = sel.Name
	ctl, base := m.ctl, m.base
	return func() tea.Msg {
		if _, err := ctl.call("loadfile", mediaURL(base, sel), "replace"); err != nil {
			return errMsg{err}
		}
		return playedMsg{}
	}
}

// terminalPlay renders the item inside the current terminal by handing the tty
// to a foreground mpv. The cookie travels via argv here, like --play.
func (m *model) terminalPlay(sel mediaItem) tea.Cmd {
	m.playing = sel.Name
	args := []string{"--vo=tct", "--keep-open=yes"}
	args = append(args, m.extra...) // a headless --vo=... still wins for tests
	if ck := sessCookie(m.client.Jar, m.base); ck != "" {
		args = append(args, "--http-header-fields="+cookieHeader(ck))
	}
	args = append(args, mediaURL(m.base, sel))
	cmd := exec.Command(m.mpvPath, args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return mpvDoneMsg{err} })
}

func (m *model) togglePause() tea.Cmd {
	m.status.Paused = !m.status.Paused
	paused := m.status.Paused
	ctl := m.ctl
	return func() tea.Msg {
		if err := ctl.setPause(paused); err != nil {
			return errMsg{err}
		}
		return noopMsg{}
	}
}

func (m *model) stop() {
	m.polling, m.playing, m.hasStatus = false, "", false
	m.ctl.stopPlayback()
}

func (m *model) toggleMode() tea.Cmd {
	if m.mode == modeWindow {
		m.mode = modeTerminal
		m.stop() // terminal mode has no background mpv to poll
		m.note = "terminal playback — mpv owns the keyboard (q to exit)"
		return nil
	}
	m.mode = modeWindow
	m.note = ""
	return nil
}

// pollCmd reads playback state from mpv without ever spawning it.
func (m *model) pollCmd() tea.Cmd {
	ctl := m.ctl
	return func() tea.Msg {
		st, ok := ctl.status()
		return statusMsg{st: st, ok: ok}
	}
}

func nextTick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func refreshCmd(c *http.Client, base string) tea.Cmd {
	return func() tea.Msg {
		items, err := listLibrary(c, base)
		return itemsMsg{items: items, err: err}
	}
}

func (m model) statusLine() string {
	mode := mutedStyle.Render(fmt.Sprintf("mode: %s (t toggles)", m.mode))
	var left string
	switch {
	case m.note != "":
		left = errStyle.Render(m.note)
	case m.playing == "":
		left = mutedStyle.Render("idle — press enter to play")
	default:
		left = playingStyle.Render("▶ " + m.playing)
		if m.hasStatus && m.status.Duration > 0 {
			frac := m.status.Position / m.status.Duration
			if frac < 0 {
				frac = 0
			} else if frac > 1 {
				frac = 1
			}
			left += "  " + m.bar.ViewAs(frac)
		}
		if m.hasStatus && m.status.Paused {
			left += mutedStyle.Render("  paused")
		}
	}
	return left + "    " + mode
}

func (m model) helpLine() string {
	return mutedStyle.Render("↑/↓ move · / filter · enter play · space pause · s stop · t terminal/window · r refresh · q quit")
}

func toListItems(items []mediaItem) []list.Item {
	out := make([]list.Item, len(items))
	for i, it := range items {
		out[i] = it
	}
	return out
}

// runTUI wires mpv (cookie header over IPC) and runs the Bubble Tea program.
func runTUI(client *http.Client, base, mpvPath string, extra []string, items []mediaItem) error {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("stream-ipc-%d.sock", os.Getpid()))
	header := cookieHeader(sessCookie(client.Jar, base))
	ctl := &mpvCtl{start: func() (*mpvInstance, error) {
		return startMPV(mpvPath, sock, extra, header)
	}}

	p := tea.NewProgram(newModel(client, base, mpvPath, extra, items, ctl))
	_, err := p.Run()

	exiting.Store(true) // stop any racing command from respawning mpv
	ctl.stop()

	if errors.Is(err, tea.ErrInterrupted) {
		return nil // signal-level Ctrl-C: a clean exit
	}
	return err
}
