package main

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

func testModel(items ...mediaItem) model {
	return newModel(nil, "http://127.0.0.1:8080", "mpv", nil, items, &mpvCtl{})
}

func TestMediaItemListFields(t *testing.T) {
	v := mediaItem{Name: "movie.mp4", Size: 1_500_000, Kind: kindVideo}
	if v.FilterValue() != "movie.mp4" || v.Title() != "movie.mp4" {
		t.Fatalf("title/filter = %q/%q", v.Title(), v.FilterValue())
	}
	if !strings.Contains(v.Description(), "video") || !strings.Contains(v.Description(), "MB") {
		t.Fatalf("video description = %q", v.Description())
	}
	img := mediaItem{Name: "scan.png", Size: 2000, Kind: kindImage}
	if !strings.Contains(img.Description(), "image") {
		t.Fatalf("image description = %q", img.Description())
	}
}

func TestToListItemsPreservesFilterValues(t *testing.T) {
	items := toListItems([]mediaItem{{Name: "a"}, {Name: "b"}})
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
	if items[0].FilterValue() != "a" || items[1].FilterValue() != "b" {
		t.Fatalf("filter values lost: %v", items)
	}
}

func TestHandleKeyQuits(t *testing.T) {
	m := testModel(mediaItem{Name: "a", Kind: kindVideo})
	for _, key := range []string{"q", "ctrl+c"} {
		cmd, handled := m.handleKey(key)
		if !handled {
			t.Fatalf("%q not handled", key)
		}
		if cmd == nil {
			t.Fatalf("%q returned nil command", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%q command = %T, want tea.QuitMsg", key, cmd())
		}
	}
}

func TestHandleKeyCtrlCQuitsWhileFiltering(t *testing.T) {
	m := testModel(mediaItem{Name: "a", Kind: kindVideo})
	m.list.SetFilterState(list.Filtering)

	// ctrl+c always quits, even mid-filter...
	if _, handled := m.handleKey("ctrl+c"); !handled {
		t.Fatal("ctrl+c not handled while filtering")
	}
	// ...but q must reach the filter input instead of quitting.
	if _, handled := m.handleKey("q"); handled {
		t.Fatal("q swallowed while filtering")
	}
}

func TestHandleKeyUnknownFallsThrough(t *testing.T) {
	m := testModel(mediaItem{Name: "a", Kind: kindVideo})
	if _, handled := m.handleKey("j"); handled {
		t.Fatal("navigation key should be left to the list")
	}
}

func TestHandleKeyStopClearsPlayback(t *testing.T) {
	m := testModel(mediaItem{Name: "a", Kind: kindVideo})
	m.playing, m.polling, m.hasStatus = "a", true, true
	if _, handled := m.handleKey("s"); !handled {
		t.Fatal("s not handled")
	}
	if m.playing != "" || m.polling || m.hasStatus {
		t.Fatalf("stop left state: playing=%q polling=%v hasStatus=%v", m.playing, m.polling, m.hasStatus)
	}
}

func TestToggleModeStopsWindowPlayback(t *testing.T) {
	m := testModel(mediaItem{Name: "a", Kind: kindVideo})
	m.playing, m.polling = "a", true
	m.toggleMode()
	if m.mode != modeTerminal {
		t.Fatalf("mode = %v", m.mode)
	}
	if m.playing != "" || m.polling {
		t.Fatalf("window playback survived switch: playing=%q polling=%v", m.playing, m.polling)
	}
}

func TestUpdateWindowSizeAndView(t *testing.T) {
	m := testModel(mediaItem{Name: "movie.mp4", Size: 10_000_000, Kind: kindVideo})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	um := updated.(model)

	v := um.View()
	if !v.AltScreen {
		t.Fatal("view is not on the alternate screen")
	}
	for _, want := range []string{"Private Stream", "movie.mp4", "mode: window"} {
		if !strings.Contains(v.Content, want) {
			t.Fatalf("view missing %q:\n%s", want, v.Content)
		}
	}
}

func TestUpdateQuitsOnCtrlC(t *testing.T) {
	m := testModel(mediaItem{Name: "a", Kind: kindVideo})
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+c produced no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c command = %T, want tea.QuitMsg", cmd())
	}
}

func TestUpdateItemsMsgReplacesList(t *testing.T) {
	m := testModel(mediaItem{Name: "old", Kind: kindVideo})
	updated, _ := m.Update(itemsMsg{items: []mediaItem{{Name: "new", Kind: kindImage}}})
	um := updated.(model)
	it, ok := um.list.SelectedItem().(mediaItem)
	if !ok || it.Name != "new" {
		t.Fatalf("items not replaced: %#v", um.list.SelectedItem())
	}
}

func TestStatusLine(t *testing.T) {
	m := testModel()
	if got := m.statusLine(); !strings.Contains(got, "idle") || !strings.Contains(got, "mode: window") {
		t.Fatalf("idle status = %q", got)
	}

	m.playing = "movie.mp4"
	m.hasStatus = true
	m.status = mpvStatus{Position: 30, Duration: 120}
	got := m.statusLine()
	if !strings.Contains(got, "movie.mp4") || !strings.Contains(got, "25%") {
		t.Fatalf("playing status = %q", got)
	}

	m.status.Paused = true
	if got := m.statusLine(); !strings.Contains(got, "paused") {
		t.Fatalf("paused status = %q", got)
	}
}

func TestStatusReportsIdleWithoutSpawning(t *testing.T) {
	c := &mpvCtl{}
	if _, ok := c.status(); ok {
		t.Fatal("status() claimed playback with no mpv running")
	}
}

func TestFloatProp(t *testing.T) {
	if v, ok := floatProp(float64(2.5)); !ok || v != 2.5 {
		t.Fatalf("float = %v %v", v, ok)
	}
	if v, ok := floatProp("3.5"); !ok || v != 3.5 {
		t.Fatalf("string float = %v %v", v, ok)
	}
	if _, ok := floatProp("inf"); ok {
		t.Fatal("inf should be treated as unknown")
	}
	if _, ok := floatProp(nil); ok {
		t.Fatal("nil should be unknown")
	}
}

func TestMediaURL(t *testing.T) {
	base := "http://127.0.0.1:8080"
	if got := mediaURL(base, mediaItem{Name: "a b.mp4", Kind: kindVideo}); got != base+"/video/a%20b.mp4" {
		t.Fatalf("video url = %q", got)
	}
	if got := mediaURL(base, mediaItem{Name: "p.png", Kind: kindImage}); got != base+"/image/p.png" {
		t.Fatalf("image url = %q", got)
	}
}

func TestCookieHeader(t *testing.T) {
	if cookieHeader("") != "" {
		t.Fatal("empty cookie should yield no header")
	}
	if got := cookieHeader("abc"); got != "Cookie: sess=abc" {
		t.Fatalf("header = %q", got)
	}
}
