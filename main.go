// jabberwock is a tiny terminal YouTube, SoundCloud, and Genius client. It uses only
// the Go standard library; mpv and optionally cava handle playback/visuals.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"unsafe"

	"jabberwock/genius"
	"jabberwock/soundcloud"
)

const (
	viewVideos = iota
	viewFilters
	viewSettings
)

var viewNames = []string{"Home", "Filters", "Settings"}

type App struct {
	cfg            Config
	cfgPath        string
	filters        Filters
	query          string
	videos         []Video
	selected       int
	page           int
	cursor         SearchCursor
	hasMore        bool
	view           int
	filterRow      int
	settingRow     int
	inputMode      string // search / instance / Soundcloak preference
	input          []rune
	notice         string
	fatalErr       error
	quit           bool
	term           terminal
	provider       string // youtube, soundcloud, or genius
	sourcePicker   bool
	sourceSelected int
	scCloak        soundcloud.CloakClient
	scDirect       soundcloud.Client
	geniusClient   genius.Client
	lyricsOpen     bool
	lyrics         string
	lyricsSong     Video
	lyricsScroll   int
}

type terminal struct {
	fd            int
	original      syscall.Termios
	active        bool
	playbackInput bool
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
		printHelp()
		return
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		fmt.Fprintln(os.Stderr, "jabberwock needs an interactive terminal. Run it directly in your terminal (not through a pipe).")
		os.Exit(2)
	}

	cfg, cfgPath, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jabberwock:", err)
		os.Exit(1)
	}
	app := &App{cfg: cfg, cfgPath: cfgPath, filters: DefaultFilters(), view: viewVideos, sourcePicker: true, scCloak: soundcloud.CloakClient{InstancesURL: cfg.SoundCloudInstancesURL, PreferredBase: cfg.SoundCloudInstance}, scDirect: soundcloud.Client{ClientID: cfg.SoundCloudClientID}, geniusClient: genius.Client{UseAPI: cfg.UseAPIs, Frontend: cfg.GeniusFrontend}, notice: "Choose YouTube, SoundCloud, or Genius with ↑/↓, then press Enter."}
	app.term = terminal{fd: int(os.Stdin.Fd())}
	if err := app.term.enable(); err != nil {
		fmt.Fprintln(os.Stderr, "jabberwock: cannot enter terminal mode:", err)
		os.Exit(1)
	}
	defer app.term.restore()
	defer fmt.Print("\x1b[0m\x1b[?25h\x1b[2J\x1b[H")

	for !app.quit {
		app.render()
		key := readKey()
		app.handleKey(key)
	}
}

func printHelp() {
	fmt.Println(`jabberwock — lightweight terminal YouTube, SoundCloud, and Genius client

Run: go run .
Build: go build -o jabberwock .

Requires an interactive terminal. Playback is delegated to mpv; for YouTube
URLs, mpv generally needs yt-dlp installed as well. No Go modules are required.
Settings: ~/.config/jabberwock/config.json (or $JABBERWOCK_CONFIG).

At startup, choose YouTube, SoundCloud, or Genius. Ctrl+P opens that chooser again.
Settings > Load through APIs toggles all providers: off scrapes original sites directly; on uses Invidious, Soundcloak, and Dumb/Intellectual instances. Genius displays lyrics in the terminal; press c to copy all lyrics. mpv handles
playback and CAVA is optional for SoundCloud visualizations. Press Esc during playback
to stop media processes and return to the previous page. Ctrl+H returns to Home. No
history or subscriptions are stored.`)
}

func isTerminal(file *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

func rawTerminal(original syscall.Termios) syscall.Termios {
	raw := original
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1 // a 100ms timeout also lets us recognize a lone Esc
	return raw
}

func (t *terminal) enable() error {
	if t.active {
		return nil
	}
	if t.playbackInput {
		t.restorePlaybackInput()
	}
	var original syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(t.fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&original)))
	if errno != 0 {
		return errno
	}
	raw := rawTerminal(original)
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(t.fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&raw)))
	if errno != 0 {
		return errno
	}
	t.original = original
	t.active = true
	fmt.Print("\x1b[?1049h\x1b[?25l")
	return nil
}

// enablePlaybackInput keeps terminal input raw so Esc can be detected while an
// external player owns the visible screen. Unlike enable(), it does not enter
// the alternate-screen buffer; CAVA and terminal output remain visible.
func (t *terminal) enablePlaybackInput() error {
	if t.playbackInput {
		return nil
	}
	if t.active {
		t.restore()
	}
	raw := rawTerminal(t.original)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(t.fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&raw)))
	if errno != 0 {
		return errno
	}
	t.playbackInput = true
	fmt.Print("\x1b[0m\x1b[?25h\x1b[2J\x1b[H")
	return nil
}

func (t *terminal) restorePlaybackInput() {
	if !t.playbackInput {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(t.fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t.original)))
	t.playbackInput = false
	fmt.Print("\x1b[0m\x1b[?25h")
}

func (t *terminal) restore() {
	if t.playbackInput {
		t.restorePlaybackInput()
	}
	if !t.active {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(t.fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t.original)))
	t.active = false
	fmt.Print("\x1b[0m\x1b[?25h\x1b[?1049l")
}

func readKey() string {
	var first [1]byte
	for {
		n, err := os.Stdin.Read(first[:])
		if err != nil {
			return ""
		}
		if n == 1 {
			break
		}
	}
	b := first[0]
	switch b {
	case 0x13:
		return "ctrl+s"
	case 0x11:
		return "ctrl+q"
	case 0x03:
		return "ctrl+c"
	case 0x7f:
		return "backspace"
	case 0x08:
		return "ctrl+h"
	case '\r', '\n':
		return "enter"
	case '\t':
		return "tab"
	case 0x1b:
		var seq [2]byte
		n, _ := os.Stdin.Read(seq[:1])
		if n == 0 {
			return "esc"
		}
		if seq[0] != '[' && seq[0] != 'O' {
			return "esc"
		}
		n, _ = os.Stdin.Read(seq[1:])
		if n == 0 {
			return "esc"
		}
		switch seq[1] {
		case 'A':
			return "up"
		case 'B':
			return "down"
		case 'C':
			return "right"
		case 'D':
			return "left"
		case 'H':
			return "home"
		case 'F':
			return "end"
		default:
			return "esc"
		}
	default:
		if b < 0x20 {
			return fmt.Sprintf("ctrl+%c", b+'a'-1)
		}
		need := 1
		switch {
		case b&0xE0 == 0xC0:
			need = 2
		case b&0xF0 == 0xE0:
			need = 3
		case b&0xF8 == 0xF0:
			need = 4
		}
		buf := []byte{b}
		for len(buf) < need {
			var next [1]byte
			n, _ := os.Stdin.Read(next[:])
			if n == 0 {
				break
			}
			buf = append(buf, next[0])
		}
		r, _ := utf8.DecodeRune(buf)
		if r == utf8.RuneError && len(buf) == 1 {
			return "�"
		}
		return string(r)
	}
}

func (a *App) handleKey(key string) {
	if key == "" {
		return
	}
	if a.lyricsOpen {
		switch key {
		case "esc":
			a.lyricsOpen = false
			a.notice = "Returned to Genius search results."
			return
		case "up", "k":
			if a.lyricsScroll > 0 {
				a.lyricsScroll--
			}
			return
		case "down", "j":
			a.lyricsScroll++
			return
		case "enter", "c", "C":
			a.copyLyrics()
			return
		}
		if key != "ctrl+h" {
			return
		}
	}
	if a.sourcePicker {
		a.handleSourcePickerKey(key)
		return
	}
	if a.inputMode != "" {
		a.handleInputKey(key)
		return
	}
	binds := a.cfg.effectiveKeybinds()
	if action, ok := binds[key]; ok {
		a.dispatchAction(action)
		return
	}
	switch a.view {
	case viewVideos:
		switch key {
		case "up", "k":
			a.moveSelection(-1)
		case "down", "j":
			a.moveSelection(1)
		case "enter":
			a.playSelected()
		case "home":
			a.selected = 0
		case "end":
			if a.hasMore {
				a.selected = len(a.videos)
			} else if len(a.videos) > 0 {
				a.selected = len(a.videos) - 1
			}
		}
	case viewFilters:
		filterCount := len(filterOptions)
		if a.provider == "soundcloud" {
			filterCount = 3
		} else if a.provider == "genius" {
			filterCount = 1
		}
		switch key {
		case "up", "k":
			a.filterRow = (a.filterRow + filterCount - 1) % filterCount
		case "down", "j":
			a.filterRow = (a.filterRow + 1) % filterCount
		case "left":
			a.cycleFilter(-1)
		case "right", "enter":
			a.cycleFilter(1)
		}
	case viewSettings:
		switch key {
		case "up", "k":
			a.settingRow = (a.settingRow + 4) % 5
		case "down", "j":
			a.settingRow = (a.settingRow + 1) % 5
		case "left":
			a.cycleSetting(-1)
		case "right", "enter":
			a.cycleSetting(1)
		}
	}
}

func (a *App) handleSourcePickerKey(key string) {
	switch key {
	case "up", "left":
		a.sourceSelected = (a.sourceSelected + 2) % 3
	case "down", "right":
		a.sourceSelected = (a.sourceSelected + 1) % 3
	case "enter":
		switch a.sourceSelected {
		case 1:
			a.provider = "soundcloud"
			a.notice = "SoundCloud selected. Ctrl+S searches tracks; Enter plays audio. API mode selects Soundcloak/direct scraping."
		case 2:
			a.provider = "genius"
			a.notice = "Genius selected. Ctrl+S searches songs; Enter opens lyrics."
		default:
			a.provider = "youtube"
			a.notice = "YouTube selected. Ctrl+S searches videos; Enter plays with mpv."
		}
		a.sourcePicker = false
		if a.provider == "soundcloud" && a.filterRow > 2 {
			a.filterRow = 2
		}
		if a.provider == "genius" {
			a.filterRow = 0
		}
		a.query = ""
		a.videos = nil
		a.selected = 0
		a.cursor = SearchCursor{}
		a.hasMore = false
	case "ctrl+q", "ctrl+c":
		a.quit = true
	case "ctrl+h":
		if a.provider == "" {
			a.provider = "youtube"
		}
		a.dispatchAction("home")
	case "esc":
		if a.provider != "" {
			a.sourcePicker = false
		}
	}
}

func (a *App) dispatchAction(action string) {
	switch action {
	case "search":
		a.beginInput("search")
	case "quit":
		a.quit = true
	case "next_view":
		a.view = (a.view + 1) % len(viewNames)
	case "switch_source":
		a.sourceSelected = 0
		if a.provider == "soundcloud" {
			a.sourceSelected = 1
		}
		if a.provider == "genius" {
			a.sourceSelected = 2
		}
		a.sourcePicker = true
	case "home":
		if a.provider == "" {
			a.provider = "youtube"
		}
		a.lyricsOpen = false
		a.sourcePicker = false
		a.inputMode = ""
		a.view = viewVideos
		a.selected = 0
		a.lyricsScroll = 0
		a.notice = "Returned to Home. Ctrl+S searches; Ctrl+P switches source."
	case "refresh":
		if strings.TrimSpace(a.query) == "" {
			a.notice = "Search first with Ctrl+S."
		} else {
			a.performSearch()
		}
	case "top":
		a.selected = 0
	case "bottom":
		if a.hasMore {
			a.selected = len(a.videos)
		} else if len(a.videos) > 0 {
			a.selected = len(a.videos) - 1
		}
	default:
		if handler, ok := registeredActions[action]; ok {
			handler(a)
		} else {
			a.notice = "Unknown key action: " + action
		}
	}
}

func (a *App) beginInput(mode string) {
	a.inputMode = mode
	a.input = nil
	if mode == "instance" {
		a.input = []rune(a.cfg.InvidiousURL)
	}
	if mode == "sc_client_id" {
		a.input = []rune(a.cfg.SoundCloudClientID)
	}
	if mode == "soundcloak_instance" {
		a.input = []rune(a.cfg.SoundCloudInstance)
	}
	if mode == "search" {
		a.notice = "Type a query and press Enter; Esc cancels."
	} else if mode == "sc_client_id" {
		a.notice = "Set a SoundCloud client ID, or clear it to auto-detect; press Enter."
	} else if mode == "soundcloak_instance" {
		a.notice = "Optional preferred Soundcloak base URL; empty selects automatically."
	} else {
		a.notice = "Edit the Invidious base URL, then press Enter."
	}
}

func (a *App) handleInputKey(key string) {
	switch key {
	case "esc":
		a.inputMode = ""
		a.notice = "Input cancelled."
	case "ctrl+h":
		a.dispatchAction("home")
	case "enter":
		value := strings.TrimSpace(string(a.input))
		mode := a.inputMode
		a.inputMode = ""
		if mode == "search" {
			if value == "" {
				a.notice = "Search cancelled: query was empty."
				return
			}
			a.query = value
			a.view = viewVideos
			a.selected = 0
			a.performSearch()
		} else if mode == "sc_client_id" {
			a.cfg.SoundCloudClientID = value
			a.saveConfig("Legacy SoundCloud client ID setting updated; Soundcloak is used for searches.")
		} else if mode == "soundcloak_instance" {
			if value != "" && !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
				a.notice = "Soundcloak URL must start with https:// or http://, or be empty for automatic selection."
				return
			}
			a.cfg.SoundCloudInstance = strings.TrimRight(value, "/")
			a.scCloak.PreferredBase = a.cfg.SoundCloudInstance
			a.saveConfig("Soundcloak preferred instance updated.")
		} else if mode == "instance" {
			if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
				a.notice = "URL must start with https:// or http://. Setting was not changed."
				return
			}
			a.cfg.InvidiousURL = strings.TrimRight(value, "/")
			a.saveConfig("Invidious URL updated.")
		}
	case "backspace":
		if len(a.input) > 0 {
			a.input = a.input[:len(a.input)-1]
		}
	case "ctrl+u":
		a.input = nil
	default:
		if len([]rune(key)) == 1 {
			r := []rune(key)[0]
			if !unicode.IsControl(r) {
				a.input = append(a.input, r)
			}
		}
	}
}

func (a *App) moveSelection(delta int) {
	last := len(a.videos) - 1
	if a.hasMore {
		last = len(a.videos) // the final selectable row is Next page
	}
	if last < 0 {
		if a.hasMore {
			a.selected = 0
		}
		return
	}
	a.selected += delta
	if a.selected < 0 {
		a.selected = 0
	}
	if a.selected > last {
		a.selected = last
	}
}

func (a *App) cycleFilter(direction int) {
	if a.provider == "genius" {
		a.notice = "Genius search uses relevance ranking; extra filters are not applicable."
		return
	}
	if direction > 0 {
		a.filters.cycle(a.filterRow)
	} else {
		option := filterOptions[a.filterRow]
		current := a.filters.valueAt(a.filterRow)
		pos := 0
		for i, value := range option.Values {
			if value == current {
				pos = i
				break
			}
		}
		previous := option.Values[(pos+len(option.Values)-1)%len(option.Values)]
		switch a.filterRow {
		case 0:
			a.filters.Sort = previous
		case 1:
			a.filters.Date = previous
		case 2:
			a.filters.Duration = previous
		case 3:
			a.filters.HideShorts = previous == "on"
		}
	}
	if strings.TrimSpace(a.query) != "" {
		a.performSearch()
	} else {
		a.notice = "Filters updated. Search a query to apply them."
	}
}

func (a *App) cycleSetting(direction int) {
	switch a.settingRow {
	case 0:
		a.cfg.UseAPIs = !a.cfg.UseAPIs
		if a.cfg.UseAPIs {
			a.cfg.Backend = "invidious"
		} else {
			a.cfg.Backend = "local"
		}
		a.saveConfig("API mode " + onOff(a.cfg.UseAPIs) + ". YouTube, SoundCloud, and Genius now use the selected loading mode.")
		if a.query != "" {
			a.performSearch()
		}
	case 1:
		a.beginInput("instance")
	case 2:
		names := a.cfg.themeNames()
		if len(names) == 0 {
			return
		}
		current := a.cfg.Theme
		if a.provider == "soundcloud" {
			current = a.cfg.SoundCloudTheme
		}
		pos := 0
		for i, name := range names {
			if name == current {
				pos = i
				break
			}
		}
		if direction >= 0 {
			pos = (pos + 1) % len(names)
		} else {
			pos = (pos + len(names) - 1) % len(names)
		}
		if a.provider == "soundcloud" {
			a.cfg.SoundCloudTheme = names[pos]
			a.saveConfig("SoundCloud theme changed to " + a.cfg.SoundCloudTheme + ".")
		} else {
			a.cfg.Theme = names[pos]
			a.saveConfig("Theme changed to " + a.cfg.Theme + ".")
		}
	case 3:
		a.beginInput("soundcloak_instance")
	case 4:
		frontends := []string{"auto", "dumb", "intellectual"}
		pos := 0
		for i, name := range frontends {
			if name == a.cfg.GeniusFrontend {
				pos = i
				break
			}
		}
		if direction >= 0 {
			pos = (pos + 1) % len(frontends)
		} else {
			pos = (pos + len(frontends) - 1) % len(frontends)
		}
		a.cfg.GeniusFrontend = frontends[pos]
		a.saveConfig("Genius frontend preference changed to " + a.cfg.GeniusFrontend + ".")
	}
}

func (a *App) saveConfig(success string) {
	if err := SaveConfig(a.cfgPath, a.cfg); err != nil {
		a.notice = "Could not save settings: " + err.Error()
		return
	}
	a.notice = success
}

func (a *App) performSearch() {
	if strings.TrimSpace(a.query) == "" {
		a.notice = "Search first with Ctrl+S."
		return
	}
	a.videos = nil
	a.selected = 0
	a.page = 1
	a.cursor = SearchCursor{}
	a.hasMore = false
	a.fetchSearchPage(false)
}

func (a *App) loadNextPage() {
	if !a.hasMore {
		a.notice = "You've reached the end of the search results."
		return
	}
	a.fetchSearchPage(true)
}

func (a *App) fetchSearchPage(next bool) {
	if a.provider == "soundcloud" {
		a.fetchSoundCloudPage(next)
		return
	}
	if a.provider == "genius" {
		a.fetchGeniusPage()
		return
	}
	page := 1
	cursor := SearchCursor{}
	if next {
		page = a.page + 1
		cursor = a.cursor
		a.notice = fmt.Sprintf("Loading search page %d…", page)
	} else {
		a.notice = "Searching…"
	}
	a.render()
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
	defer cancel()
	result, err := SearchVideosPage(ctx, a.cfg, a.query, a.filters, page, cursor)
	if err != nil {
		a.notice = "Search failed: " + err.Error()
		if !next {
			a.videos = nil
			a.selected = 0
			a.hasMore = false
			a.cursor = SearchCursor{}
		}
		return
	}

	if next {
		previousCount := len(a.videos)
		a.videos = appendUniqueVideos(a.videos, result.Videos)
		a.videos = FilterAndSort(a.videos, a.filters)
		a.page = page
		a.cursor = result.Cursor
		a.hasMore = result.HasMore
		if len(a.videos) > previousCount {
			a.selected = min(previousCount, len(a.videos)-1)
			newCount := len(a.videos) - previousCount
			a.notice = fmt.Sprintf("Loaded %d more videos (page %d).", newCount, page)
		} else if a.hasMore {
			a.selected = len(a.videos) // keep the Next page row selected
			a.notice = "No new videos matched on this page. Press Enter on Next page to continue."
		} else {
			a.selected = max(0, len(a.videos)-1)
			a.notice = "You've reached the end of the search results."
		}
		return
	}

	a.page = page
	a.videos = result.Videos
	a.cursor = result.Cursor
	a.hasMore = result.HasMore
	a.selected = 0
	if len(a.videos) == 0 {
		a.notice = "No results matched those filters. Try broader filters."
		return
	}
	a.notice = fmt.Sprintf("Found %d videos on page 1.%s", len(a.videos), nextPageNotice(a.hasMore))
}

func (a *App) fetchSoundCloudPage(next bool) {
	offset := 0
	nextURL := ""
	if next {
		offset = a.cursor.SoundCloudOffset
		nextURL = a.cursor.SoundCloudNextURL
		a.notice = fmt.Sprintf("Loading SoundCloud page %d…", a.page+1)
	} else {
		if a.cfg.UseAPIs {
			a.notice = "Searching SoundCloud through Soundcloak…"
		} else {
			a.notice = "Searching SoundCloud directly…"
		}
	}
	a.render()
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Second)
	defer cancel()
	a.scCloak.InstancesURL = a.cfg.SoundCloudInstancesURL
	a.scCloak.PreferredBase = a.cfg.SoundCloudInstance
	var result soundcloud.SearchPage
	var err error
	if a.cfg.UseAPIs {
		result, err = a.scCloak.Search(ctx, a.query, offset, 20, nextURL)
	} else {
		a.scDirect.ClientID = a.cfg.SoundCloudClientID
		result, err = a.scDirect.Search(ctx, a.query, offset, 20, nextURL)
	}
	if err != nil {
		a.notice = "SoundCloud search failed: " + err.Error()
		if !next {
			a.videos = nil
			a.selected = 0
			a.hasMore = false
			a.cursor = SearchCursor{}
		}
		return
	}
	pageVideos := make([]Video, 0, len(result.Tracks))
	for _, track := range result.Tracks {
		v := Video{
			Title: track.Title, Author: track.Username, VideoID: "sc:" + track.ID,
			WatchURL: track.PermalinkURL, StreamURL: track.StreamURL, IsTrack: true,
			DurationSeconds: track.DurationMS / 1000, DurationKnown: track.DurationMS > 0,
			Views: track.PlaybackCount, ViewsKnown: track.PlaybackCountKnown,
			Published: track.CreatedAt, PublishedKnown: !track.CreatedAt.IsZero(), PublishedText: track.CreatedAtText,
		}
		pageVideos = append(pageVideos, v)
	}
	if next {
		previousCount := len(a.videos)
		a.videos = appendUniqueVideos(a.videos, pageVideos)
		a.videos = FilterAndSort(a.videos, a.filters)
		a.page++
		a.cursor = SearchCursor{SoundCloudOffset: result.NextOffset, SoundCloudNextURL: result.NextURL}
		a.hasMore = result.HasMore
		if len(a.videos) > previousCount {
			a.selected = min(previousCount, len(a.videos)-1)
			a.notice = fmt.Sprintf("Loaded %d more playable tracks (page %d).", len(a.videos)-previousCount, a.page)
		} else if a.hasMore {
			a.selected = len(a.videos)
			a.notice = "No playable tracks on this page matched. Press Enter on Next page to continue."
		} else {
			a.selected = max(0, len(a.videos)-1)
			a.notice = "You've reached the end of playable SoundCloud tracks."
		}
		return
	}
	a.page = 1
	a.videos = FilterAndSort(pageVideos, a.filters)
	a.cursor = SearchCursor{SoundCloudOffset: result.NextOffset, SoundCloudNextURL: result.NextURL}
	a.hasMore = result.HasMore
	a.selected = 0
	if len(a.videos) == 0 && !a.hasMore {
		a.notice = "No playable SoundCloud tracks matched. Go and SoundCloud-only tracks are hidden by default."
		return
	}
	a.notice = fmt.Sprintf("Found %d playable tracks on page 1.%s", len(a.videos), nextPageNotice(a.hasMore))
}

func nextPageNotice(hasMore bool) string {
	if hasMore {
		return " Select Next page at the bottom for more."
	}
	return ""
}

func appendUniqueVideos(existing, incoming []Video) []Video {
	out := make([]Video, 0, len(existing)+len(incoming))
	seen := make(map[string]bool, len(existing)+len(incoming))
	for _, video := range existing {
		if video.VideoID == "" || seen[video.VideoID] {
			continue
		}
		seen[video.VideoID] = true
		out = append(out, video)
	}
	for _, video := range incoming {
		if video.VideoID == "" || seen[video.VideoID] {
			continue
		}
		seen[video.VideoID] = true
		out = append(out, video)
	}
	return out
}

func (a *App) playSelected() {
	if a.hasMore && a.selected >= len(a.videos) {
		a.loadNextPage()
		return
	}
	if len(a.videos) == 0 || a.selected < 0 || a.selected >= len(a.videos) {
		a.notice = "No video selected. Search with Ctrl+S first."
		return
	}
	video := a.videos[a.selected]
	if a.provider == "genius" {
		a.openLyrics(video)
		return
	}
	if video.IsTrack {
		a.playSoundCloudTrack(video)
		return
	}
	if _, err := exec.LookPath("mpv"); err != nil {
		a.notice = "mpv not found. Install mpv to watch videos."
		return
	}
	cmd := exec.Command("mpv", "--input-terminal=no", "--", video.WatchURL)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	stopped, err, _ := a.runManagedPlayback(cmd, nil, "YouTube: "+video.Title)
	switch {
	case stopped:
		a.notice = "Playback stopped. Returned to your results."
	case err != nil:
		a.notice = "mpv exited: " + err.Error()
	default:
		a.notice = "Playback finished."
	}
}

func (a *App) playSoundCloudTrack(track Video) {
	if strings.TrimSpace(track.StreamURL) == "" {
		a.notice = "This SoundCloud track has no supported playable stream. It may be DRM-protected or SoundCloud Go-only."
		return
	}
	if _, err := exec.LookPath("mpv"); err != nil {
		a.notice = "mpv not found. Install mpv to play SoundCloud audio."
		return
	}

	// Soundcloak's restream/progressive endpoint provides a stable proxied
	// stream and renews upstream SoundCloud URLs on the server side.
	var mpvLog bytes.Buffer
	mpv := exec.Command("mpv", "--no-video", "--force-window=no", "--no-terminal", "--input-terminal=no", "--msg-level=all=warn", "--", track.StreamURL)
	mpv.Stdin = nil
	mpv.Stdout, mpv.Stderr = &mpvLog, &mpvLog

	var cavaAttempts []cavaAttempt
	if cavaPath, lookupErr := exec.LookPath("cava"); lookupErr == nil {
		cavaAttempts = newCAVACommands(cavaPath)
		for i := range cavaAttempts {
			cavaAttempts[i].cmd.Stdin = nil
			cavaAttempts[i].cmd.Stdout, cavaAttempts[i].cmd.Stderr = os.Stdout, os.Stderr
			defer cavaAttempts[i].cleanup()
		}
	}
	stopped, playErr, visualErr := a.runManagedPlayback(mpv, cavaAttempts, "SoundCloud: "+track.Title+" — "+track.Author)
	if stopped {
		a.notice = "Playback stopped. Returned to your results."
		return
	}
	if playErr != nil {
		a.notice = "mpv playback failed: " + playErr.Error()
		if logText := strings.TrimSpace(mpvLog.String()); logText != "" {
			// Keep the normal TUI intact; a short detail is enough to distinguish
			// expired streams and decoder failures without leaving CAVA's output up.
			a.notice += " (" + firstLine(logText) + ")"
		}
		return
	}
	if visualErr != nil {
		a.notice = "Playback finished; CAVA stopped: " + visualErr.Error()
	} else {
		a.notice = "Playback finished."
	}
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if i := strings.IndexAny(value, "\r\n"); i >= 0 {
		value = value[:i]
	}
	if len(value) > 120 {
		value = value[:117] + "..."
	}
	return value
}

// cavaAttempt pairs one input backend with its temporary configuration. CAVA
// builds vary in which capture backends are compiled in, so playback can try
// more than one rather than silently giving up on the first unsupported method.
type cavaAttempt struct {
	cmd     *exec.Cmd
	cleanup func()
	method  string
}

// newCAVACommands returns likely working audio-capture backends first. PipeWire
// is preferred when its socket exists; PulseAudio compatibility is tried next.
// If detection is inconclusive, preserve the user's normal CAVA config first.
func newCAVACommands(cavaPath string) []cavaAttempt {
	methods := detectCAVAAudioMethods()
	attempts := make([]cavaAttempt, 0, len(methods))
	seen := map[string]bool{}
	for _, method := range methods {
		if method == "default" {
			attempts = append(attempts, cavaAttempt{cmd: exec.Command(cavaPath), cleanup: func() {}, method: method})
			continue
		}
		if seen[method] {
			continue
		}
		seen[method] = true
		cmd, cleanup := newCAVACommandForMethod(cavaPath, method)
		attempts = append(attempts, cavaAttempt{cmd: cmd, cleanup: cleanup, method: method})
	}
	if len(attempts) == 0 {
		return []cavaAttempt{{cmd: exec.Command(cavaPath), cleanup: func() {}, method: "default"}}
	}
	return attempts
}

func newCAVACommandForMethod(cavaPath, method string) (*exec.Cmd, func()) {
	config := cavaConfig(method)
	f, err := os.CreateTemp("", "jabberwock-cava-*.conf")
	if err != nil {
		return exec.Command(cavaPath), func() {}
	}
	name := f.Name()
	if _, err = f.WriteString(config); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return exec.Command(cavaPath), func() {}
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(name)
		return exec.Command(cavaPath), func() {}
	}
	return exec.Command(cavaPath, "-p", name), func() { _ = os.Remove(name) }
}

func cavaConfig(method string) string {
	active := ""
	if method == "pipewire" {
		active = "active = 1\n"
	}
	return fmt.Sprintf(`[general]
framerate = 60
autosens = 1
sleep_timer = 0

[input]
method = %s
source = auto
%s
[output]
method = noncurses
orientation = bottom
bar_width = 2
bar_spacing = 1
`, method, active)
}

func detectCAVAAudioMethods() []string {
	// Prefer CAVA's own working user configuration first. Previously jabberwock
	// forced an inferred backend and accepted it merely because the process stayed
	// alive, which could leave a silent visualizer and never try the user's config.
	methods := []string{"default"}
	runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	pipewireAvailable := false
	pulseSocket := false
	if runtimeDir != "" {
		if _, err := os.Stat(filepath.Join(runtimeDir, "pipewire-0")); err == nil {
			pipewireAvailable = true
		}
		if _, err := os.Stat(filepath.Join(runtimeDir, "pulse", "native")); err == nil {
			pulseSocket = true
		}
	}
	if pipewireAvailable {
		methods = append(methods, "pipewire")
	}
	pulseAvailable := pulseSocket || strings.TrimSpace(os.Getenv("PULSE_SERVER")) != ""
	if _, err := exec.LookPath("pactl"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		err := exec.CommandContext(ctx, "pactl", "info").Run()
		cancel()
		pulseAvailable = pulseAvailable || err == nil
	}
	if pulseAvailable {
		methods = append(methods, "pulse")
	}
	return methods
}

type playbackProcess struct {
	cmd     *exec.Cmd
	done    chan error
	running bool
	err     error
}

func startPlaybackProcess(cmd *exec.Cmd) (*playbackProcess, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &playbackProcess{cmd: cmd, done: make(chan error, 1), running: true}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

func (p *playbackProcess) collectIfDone() bool {
	if p == nil || !p.running {
		return false
	}
	select {
	case err := <-p.done:
		p.running, p.err = false, err
		return true
	default:
		return false
	}
}

func (p *playbackProcess) stop() {
	if p == nil || !p.running || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	// Kill the entire group, not just the top-level command, so mpv helpers and
	// CAVA are cleaned up when Esc is pressed. Fall back to SIGKILL if needed.
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-p.done:
		p.running, p.err = false, err
	case <-time.After(700 * time.Millisecond):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		err := <-p.done
		p.running, p.err = false, err
	}
}

// pollTerminalKey waits briefly for user input without spawning a reader
// goroutine that could steal a key after playback has ended.
func pollTerminalKey(timeout time.Duration) (string, bool, error) {
	fd := int(os.Stdin.Fd())
	var set syscall.FdSet
	word, bit := fd/64, uint(fd%64)
	if word >= len(set.Bits) {
		return "", false, fmt.Errorf("terminal fd %d is too large for select", fd)
	}
	set.Bits[word] |= int64(1) << bit
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	n, err := syscall.Select(fd+1, &set, nil, nil, &tv)
	if err != nil {
		if err == syscall.EINTR {
			return "", false, nil
		}
		return "", false, err
	}
	if n == 0 {
		return "", false, nil
	}
	return readKey(), true, nil
}

// runManagedPlayback temporarily gives the visible terminal to the player and
// optional visualizer while retaining raw input in jabberwock. Esc terminates
// both child process groups and returns to the existing UI state.
func (a *App) runManagedPlayback(playerCmd *exec.Cmd, visualAttempts []cavaAttempt, title string) (stopped bool, playerErr, visualErr error) {
	a.term.restore()
	if err := a.term.enablePlaybackInput(); err != nil {
		a.notice = "Could not listen for playback controls: " + err.Error()
		if enableErr := a.term.enable(); enableErr != nil {
			a.quit, a.fatalErr = true, enableErr
		}
		return false, err, nil
	}
	fmt.Printf("jabberwock — %s\r\n", title)
	if len(visualAttempts) > 0 {
		fmt.Print("Starting CAVA audio monitor. Press Esc to stop playback and return to jabberwock.\r\n\r\n")
	} else {
		fmt.Print("Press Esc to stop playback and return to jabberwock.\r\n\r\n")
	}

	// Start CAVA first so it can attach to the system-output monitor before mpv
	// begins producing audio. Try alternate capture backends if this CAVA build
	// exits quickly because a requested backend is unavailable.
	var visual *playbackProcess
	var err error
	for _, attempt := range visualAttempts {
		visual, err = startPlaybackProcess(attempt.cmd)
		if err != nil {
			visualErr = err
			fmt.Printf("CAVA (%s) could not start: %v\r\n", attempt.method, err)
			continue
		}
		time.Sleep(450 * time.Millisecond)
		if visual.collectIfDone() {
			if visual.err != nil {
				visualErr = fmt.Errorf("%s backend: %w", attempt.method, visual.err)
				fmt.Printf("CAVA (%s) failed: %v; trying another backend.\r\n", attempt.method, visual.err)
			}
			visual = nil
			continue
		}
		fmt.Printf("CAVA started (%s).\r\n", attempt.method)
		visualErr = nil
		break
	}
	player, err := startPlaybackProcess(playerCmd)
	if err != nil {
		playerErr = err
		if visual != nil {
			visual.stop()
		}
		fmt.Printf("Could not start mpv: %v\r\n", err)
		a.finishPlaybackTerminal()
		return false, playerErr, visualErr
	}

	for player.running {
		player.collectIfDone()
		if !player.running {
			break
		}
		if visual != nil && visual.collectIfDone() {
			if visual.err != nil {
				visualErr = visual.err
				fmt.Printf("\r\nCAVA stopped (%v); audio will continue.\r\n", visual.err)
			}
			visual = nil
		}
		key, ready, keyErr := pollTerminalKey(100 * time.Millisecond)
		if keyErr != nil {
			// A transient terminal read failure should not tear down the player.
			continue
		}
		if ready && key == "esc" {
			stopped = true
			player.stop()
			if visual != nil {
				visual.stop()
			}
			break
		}
	}
	if !stopped && !player.running {
		playerErr = player.err
	}
	if visual != nil && visual.running {
		// The player ended (or Esc was pressed); stopping CAVA here is normal
		// cleanup, not a CAVA failure worth reporting to the user.
		visual.stop()
	}
	a.finishPlaybackTerminal()
	return stopped, playerErr, visualErr
}

func (a *App) finishPlaybackTerminal() {
	a.term.restorePlaybackInput()
	if enableErr := a.term.enable(); enableErr != nil {
		a.quit = true
		a.fatalErr = enableErr
	}
}

func (a *App) render() {
	if a.lyricsOpen {
		a.renderLyrics()
		return
	}
	if a.sourcePicker {
		a.renderSourcePicker()
		return
	}
	width, height := terminalSize()
	if width < 48 {
		width = 48
	}
	if height < 12 {
		height = 12
	}
	theme := a.cfg.selectedThemeFor(a.provider)
	reset := "\x1b[0m"
	paint := func(code, text string) string {
		if code == "" {
			return text
		}
		return "\x1b[" + code + "m" + text + reset
	}
	clip := func(s string) string {
		r := []rune(s)
		if len(r) > width-2 {
			return string(r[:max(0, width-5)]) + "…"
		}
		return s
	}
	lines := make([]string, 0, height)
	backendLabel := "LOCAL SCRAPE"
	productLabel := "YouTube TUI"
	if a.provider == "soundcloud" {
		backendLabel = "SOUNDCLOUD TRACKS / SOUNDCLOAK"
		productLabel = "YouTube + SoundCloud + Genius TUI"
	} else if a.provider == "genius" {
		backendLabel = "GENIUS LYRICS"
		productLabel = "YouTube + SoundCloud + Genius TUI"
	} else if a.cfg.Backend == "invidious" {
		backendLabel = "INVIDIOUS API"
	}
	lines = append(lines, "  "+paint(theme.Title, "JABBERWOCK")+"  "+paint(theme.Muted, "minimal "+productLabel)+"  "+paint(theme.Accent, backendLabel))
	var tabs []string
	for i, name := range viewNames {
		if i == a.view {
			tabs = append(tabs, paint(theme.Selected, " "+name+" "))
		} else {
			tabs = append(tabs, paint(theme.Muted, " "+name+" "))
		}
	}
	lines = append(lines, "  "+strings.Join(tabs, "  "))
	lines = append(lines, paint(theme.Border, strings.Repeat("─", width-2)))

	switch a.view {
	case viewVideos:
		if a.query == "" {
			if a.provider == "genius" {
				lines = append(lines, "  Welcome to jabberwock Genius mode — search songs and read lyrics in the terminal.")
				lines = append(lines, "  Ctrl+S searches songs; Enter opens the selected song's lyrics.")
				lines = append(lines, "  Esc returns from lyrics to results; Ctrl+H returns Home without quitting.")
			} else if a.provider == "soundcloud" {
				lines = append(lines, "  Welcome to jabberwock SoundCloud mode — search, filter, and play tracks.")
				lines = append(lines, "  Ctrl+S searches tracks; Enter plays with mpv, optionally alongside CAVA visuals.")
				lines = append(lines, "  Press Esc during playback to stop mpv/CAVA and return here.")
				lines = append(lines, "  SoundCloud uses its orange-accent theme by default; change it in Settings.")
				lines = append(lines, "  Go / subscriber-only, blocked, and unplayable tracks are hidden.")
			} else {
				lines = append(lines, "  Welcome to jabberwock YouTube mode — search, filter, and watch videos.")
				lines = append(lines, "  Ctrl+S searches; Enter plays a video in mpv. At the end, select Next page.")
				lines = append(lines, "  Press Esc during playback to stop mpv and return here.")
			}
			lines = append(lines, "  Tab switches Home / Filters / Settings; ↑/↓ navigate results and rows inside a section.")
			lines = append(lines, "  ←/→ change the selected filter or setting. Press Ctrl+P to switch source.")
			lines = append(lines, "  No watch history or local subscriptions are stored.")
		} else {
			lines = append(lines, "  Query: "+clip(a.query))
			if a.provider == "genius" {
				lines = append(lines, "  Search results from Genius. Select a song and press Enter to read lyrics.")
			} else if a.provider == "soundcloud" {
				lines = append(lines, "  Filters: sort="+a.filters.Sort+"  uploaded="+a.filters.Date+"  duration="+a.filters.Duration+"  playable tracks only")
			} else {
				lines = append(lines, "  Filters: sort="+a.filters.Sort+"  uploaded="+a.filters.Date+"  duration="+a.filters.Duration+"  hide-shorts="+onOff(a.filters.HideShorts))
			}
			lines = append(lines, "")
			available := height - 10
			if available < 1 {
				available = 1
			}
			listCount := len(a.videos)
			if a.hasMore {
				listCount++ // one additional, selectable Next page row
			}
			start := 0
			if a.selected >= available {
				start = a.selected - available + 1
			}
			end := min(listCount, start+available)
			if len(a.videos) == 0 && !a.hasMore {
				lines = append(lines, paint(theme.Muted, "  No videos to display. Press Ctrl+S to search or visit Filters."))
			} else {
				for i := start; i < end; i++ {
					marker := "  "
					if i == a.selected {
						marker = "› "
					}
					if i == len(a.videos) && a.hasMore {
						row := clip("  " + marker + "Next page  —  load more search results")
						if i == a.selected {
							lines = append(lines, paint(theme.Selected, row))
						} else {
							lines = append(lines, paint(theme.Accent, row))
						}
						continue
					}
					video := a.videos[i]
					meta := fmt.Sprintf("[%s] %s", formatDuration(video), video.Title)
					if video.Author != "" {
						meta += "  —  " + video.Author
					}
					if video.ViewsKnown {
						meta += "  ·  " + compactCount(video.Views) + " views"
					}
					if video.PublishedText != "" {
						meta += "  ·  " + video.PublishedText
					}
					row := clip("  " + marker + meta)
					if i == a.selected {
						lines = append(lines, paint(theme.Selected, row))
					} else {
						lines = append(lines, paint(theme.Text, row))
					}
				}
			}
		}
	case viewFilters:
		lines = append(lines, "  Select a row with ↑/↓. Press Enter (or →) to cycle; ← cycles backwards.")
		lines = append(lines, "")
		options := filterOptions
		if a.provider == "soundcloud" {
			options = filterOptions[:3]
		} else if a.provider == "genius" {
			options = nil
		}
		for i, option := range options {
			row := fmt.Sprintf("  %-14s  %s", option.Name, a.filters.labelAt(i))
			if i == a.filterRow {
				lines = append(lines, paint(theme.Selected, clip("› "+row)))
			} else {
				lines = append(lines, paint(theme.Text, clip("  "+row)))
			}
		}
		lines = append(lines, "")
		if a.provider == "soundcloud" {
			lines = append(lines, paint(theme.Muted, "  SoundCloud mode: API toggle selects Soundcloak or direct scraping; Go-only and blocked tracks are hidden."))
		} else if a.provider == "genius" {
			lines = append(lines, paint(theme.Muted, "  Genius searches songs by relevance. Select a result and press Enter to read lyrics."))
		} else {
			lines = append(lines, paint(theme.Muted, "  No Shorts: Shorts-marked results are hidden. Older Invidious instances may not label every Short."))
		}
	case viewSettings:
		lines = append(lines, "  Settings are saved to: "+clip(a.cfgPath))
		lines = append(lines, "  Select with ↑/↓; Enter or → changes the selected setting.")
		lines = append(lines, "")
		apiMode := "Disabled — scrape original sites directly"
		if a.cfg.UseAPIs {
			apiMode = "Enabled — use public APIs/frontends"
		}
		scInstance := strings.TrimSpace(a.cfg.SoundCloudInstance)
		if scInstance == "" {
			scInstance = "Auto (instances.json)"
		}
		themeSetting := a.cfg.Theme
		if a.provider == "soundcloud" {
			themeSetting = a.cfg.SoundCloudTheme + " (SoundCloud)"
		}
		settings := []string{
			"Load through APIs     " + apiMode,
			"Invidious base URL    " + a.cfg.InvidiousURL,
			"Theme                 " + themeSetting,
			"Soundcloak instance   " + scInstance,
			"Genius frontend       " + a.cfg.GeniusFrontend + " (Dumb / Intellectual)",
		}
		for i, setting := range settings {
			row := clip("  " + setting)
			if i == a.settingRow {
				lines = append(lines, paint(theme.Selected, "› "+row))
			} else {
				lines = append(lines, paint(theme.Text, "  "+row))
			}
		}
		lines = append(lines, "")
		lines = append(lines, paint(theme.Muted, "  API mode applies to all sources: disabled scrapes the original sites directly; enabled uses Invidious, Soundcloak, and Dumb/Intellectual."))
		lines = append(lines, paint(theme.Muted, "  Keybinds: key -> action in config.json; Ctrl+P opens the source picker; Ctrl+H returns Home."))
		lines = append(lines, paint(theme.Muted, "  Genius frontend preference cycles Auto / Dumb / Intellectual when API mode is enabled."))
	}

	for len(lines) < height-3 {
		lines = append(lines, "")
	}
	lines = append(lines, paint(theme.Border, strings.Repeat("─", width-2)))
	if a.inputMode != "" {
		prompt := "Search> "
		if a.inputMode == "instance" {
			prompt = "Invidious URL> "
		} else if a.inputMode == "sc_client_id" {
			prompt = "SoundCloud client ID (legacy)> "
		} else if a.inputMode == "soundcloak_instance" {
			prompt = "Preferred Soundcloak URL (empty = auto)> "
		}
		lines = append(lines, paint(theme.Accent, "  "+prompt)+clip(string(a.input))+"█")
	} else {
		lines = append(lines, paint(theme.Muted, "  Tab: sections  ↑/↓: items  ←/→: options  Ctrl+S: search  Enter: select/play  Ctrl+H: Home  Ctrl+Q: quit"))
	}
	statusCode := theme.Muted
	if strings.HasPrefix(a.notice, "Search failed:") || strings.HasPrefix(a.notice, "SoundCloud search failed:") || strings.HasPrefix(a.notice, "Genius search failed:") || strings.HasPrefix(a.notice, "Genius lyrics failed:") || strings.HasPrefix(a.notice, "Could not") || strings.Contains(a.notice, "not found") {
		statusCode = theme.Error
	}
	status := "  " + a.notice
	if a.fatalErr != nil {
		status = "  Error: " + a.fatalErr.Error()
	}
	lines = append(lines, paint(statusCode, clip(status)))
	if len(lines) > height {
		lines = lines[:height]
	}
	fmt.Print("\x1b[?25l\x1b[H\x1b[2J")
	for _, line := range lines {
		fmt.Print(line, "\r\n")
	}
	if a.inputMode != "" {
		fmt.Print("\x1b[?25h")
	}
}

func (a *App) renderSourcePicker() {
	width, height := terminalSize()
	if width < 48 {
		width = 48
	}
	if height < 12 {
		height = 12
	}
	theme := a.cfg.selectedTheme()
	paint := func(code, value string) string {
		if code == "" {
			return value
		}
		return "\x1b[" + code + "m" + value + "\x1b[0m"
	}
	lines := []string{
		"  " + paint(theme.Title, "JABBERWOCK") + "  " + paint(theme.Muted, "lightweight terminal media client"),
		"  " + strings.Repeat("─", width-4),
		"",
		"  Choose a source to open:",
		"",
	}
	choices := []string{"YouTube", "SoundCloud", "Genius (lyrics)"}
	for i, choice := range choices {
		row := "    " + choice
		if i == a.sourceSelected {
			lines = append(lines, paint(theme.Selected, "  › "+row))
		} else {
			lines = append(lines, paint(theme.Text, "    "+row))
		}
	}
	lines = append(lines, "", paint(theme.Muted, "  ↑/↓ choose, Enter open, Ctrl+Q quit"), paint(theme.Muted, "  YouTube: videos. SoundCloud: tracks via Soundcloak. Genius: search/read lyrics."), paint(theme.Muted, "  Tab switches Home / Filters / Settings; arrows move inside a section."), paint(theme.Muted, "  Ctrl+H returns Home without quitting jabberwock."))
	for len(lines) < height-2 {
		lines = append(lines, "")
	}
	fmt.Print("\x1b[?25l\x1b[H\x1b[2J")
	for _, line := range lines {
		fmt.Print(line, "\r\n")
	}
}

func (a *App) fetchGeniusPage() {
	a.notice = "Searching Genius songs…"
	a.render()
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
	defer cancel()
	a.geniusClient.UseAPI = a.cfg.UseAPIs
	a.geniusClient.Frontend = a.cfg.GeniusFrontend
	results, err := a.geniusClient.Search(ctx, a.query)
	if err != nil {
		a.notice = "Genius search failed: " + err.Error()
		a.videos = nil
		a.selected = 0
		a.hasMore = false
		return
	}
	videos := make([]Video, 0, len(results))
	for _, song := range results {
		videos = append(videos, Video{Title: song.Title, Author: song.Artist, VideoID: "genius:" + song.ID, WatchURL: song.URL, GeniusURL: song.URL, GeniusPath: song.Path, GeniusMirrorURL: song.MirrorURL, GeniusID: song.ID})
	}
	a.videos = videos
	a.page = 1
	a.cursor = SearchCursor{}
	a.hasMore = false
	a.selected = 0
	if len(videos) == 0 {
		a.notice = "No Genius song results found. Try another query."
	} else {
		a.notice = fmt.Sprintf("Found %d Genius song results. Press Enter to read lyrics.", len(videos))
	}
}

func (a *App) openLyrics(video Video) {
	a.notice = "Fetching lyrics for " + video.Title + "…"
	a.render()
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Second)
	defer cancel()
	a.geniusClient.UseAPI = a.cfg.UseAPIs
	a.geniusClient.Frontend = a.cfg.GeniusFrontend
	lyrics, err := a.geniusClient.Lyrics(ctx, genius.Song{ID: video.GeniusID, Title: video.Title, Artist: video.Author, URL: video.GeniusURL, Path: video.GeniusPath, MirrorURL: video.GeniusMirrorURL})
	if err != nil {
		a.notice = "Genius lyrics failed: " + err.Error()
		return
	}
	a.lyricsSong = video
	a.lyrics = lyrics
	a.lyricsScroll = 0
	a.lyricsOpen = true
	a.notice = "Lyrics loaded. ↑/↓ scroll; Enter or c copies all lyrics; Esc returns to results; Ctrl+H returns Home."
}

func (a *App) copyLyrics() {
	if strings.TrimSpace(a.lyrics) == "" {
		a.notice = "No lyrics are loaded to copy."
		return
	}
	method, err := copyTextToClipboard(a.lyrics)
	if err != nil {
		a.notice = "Could not copy lyrics: " + err.Error()
		return
	}
	a.notice = "Full lyrics copied to clipboard via " + method + "."
}

func (a *App) renderLyrics() {
	width, height := terminalSize()
	if width < 48 {
		width = 48
	}
	if height < 12 {
		height = 12
	}
	theme := a.cfg.selectedThemeFor("genius")
	paint := func(code, text string) string {
		if code == "" {
			return text
		}
		return "\x1b[" + code + "m" + text + "\x1b[0m"
	}
	clip := func(s string) string {
		r := []rune(s)
		if len(r) > width-4 {
			return string(r[:max(0, width-7)]) + "…"
		}
		return s
	}
	lines := []string{
		"  " + paint(theme.Title, "JABBERWOCK") + "  " + paint(theme.Accent, "GENIUS LYRICS"),
		"  " + strings.Repeat("─", width-2),
		"  " + paint(theme.Title, a.lyricsSong.Title),
		"  " + paint(theme.Muted, a.lyricsSong.Author),
		"  " + paint(theme.Selected, "[ Copy lyrics ]") + "  " + paint(theme.Muted, "press Enter or c to copy everything"),
		"",
	}
	lyricLines := strings.Split(a.lyrics, "\n")
	available := height - len(lines) - 3
	if available < 1 {
		available = 1
	}
	maxScroll := max(0, len(lyricLines)-available)
	if a.lyricsScroll > maxScroll {
		a.lyricsScroll = maxScroll
	}
	if a.lyricsScroll < 0 {
		a.lyricsScroll = 0
	}
	end := min(len(lyricLines), a.lyricsScroll+available)
	for i := a.lyricsScroll; i < end; i++ {
		lines = append(lines, "  "+paint(theme.Text, clip(lyricLines[i])))
	}
	for len(lines) < height-2 {
		lines = append(lines, "")
	}
	lines = append(lines, paint(theme.Border, strings.Repeat("─", width-2)))
	lines = append(lines, paint(theme.Muted, "  ↑/↓ scroll lyrics  Enter/c: Copy lyrics  Esc: results  Ctrl+H: Home  Ctrl+Q: quit"))
	if len(lines) > height {
		lines = lines[:height]
	}
	fmt.Print("\x1b[?25l\x1b[H\x1b[2J")
	for _, line := range lines {
		fmt.Print(line, "\r\n")
	}
}

func terminalSize() (int, int) {
	var size struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 || size.Col == 0 || size.Row == 0 {
		return 80, 24
	}
	return int(size.Col), int(size.Row)
}

func formatDuration(video Video) string {
	if !video.DurationKnown {
		if video.IsTrack {
			return "--:--"
		}
		return "LIVE"
	}
	seconds := video.DurationSeconds
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, secs)
	}
	return fmt.Sprintf("%d:%02d", minutes, secs)
}

func compactCount(n int64) string {
	if n >= 1_000_000_000 {
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprint(n)
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

// Avoid a shadowed future toolchain function if building with Go versions before 1.21.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
