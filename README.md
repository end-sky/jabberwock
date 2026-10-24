# jabberwock

A deliberately small, native terminal media client written in Go. It combines **YouTube**, **SoundCloud**, and **Genius** in one TUI, keeps no watch history or local subscriptions, and uses only the Go standard library. Playback is delegated to `mpv`.

## Build and run

```sh
go build -ldflags='-s -w' -o jabberwock .
./jabberwock
```

You need an interactive Linux terminal. Install `mpv` to play videos and audio. YouTube playback generally also needs `yt-dlp` available on `PATH`. SoundCloud terminal visualizations are optional: if `cava` is installed, jabberwock runs `mpv` audio-only and quietly in the background while CAVA takes over the terminal; without CAVA, audio plays through `mpv` without visuals. During either YouTube or SoundCloud playback, jabberwock keeps control of terminal input so `Esc` stops playback and returns to the same results page.

There are no third-party Go modules. Runtime media tools are external programs, not Go dependencies.

## Choose a source

At startup, select **YouTube**, **SoundCloud**, or **Genius (lyrics)** with `↑` / `↓` and press `Enter`. Press `Ctrl+P` at any time to switch source. Each scraper lives in its own directory (`yt/`, `soundcloud/`, and `genius/`) so problems can be diagnosed independently. Genius results open a scrollable lyrics view rather than launching a browser.

There are no third-party Go modules. Runtime media tools are external programs, not Go dependencies.


## Controls

| Key | Action |
| --- | --- |
| `Ctrl+S` or `/` | Search the selected source |
| `Tab` | Switch section: Home → Filters → Settings |
| `↑` / `↓` | Navigate results or the rows in the current section |
| `←` / `→` | Change the selected filter or setting |
| `Enter` | Play a result, load **Next page**, or apply an option |
| `Ctrl+P` | Reopen the YouTube / SoundCloud / Genius source chooser |
| `Ctrl+H` | Return Home without quitting or restarting jabberwock |
| `r` | Refresh the current search |
| `g` / `G` | Jump to first / last result |
| `Ctrl+Q` / `Ctrl+C` | Quit |
| `Esc` | Cancel an input prompt; during playback, stop `mpv` and CAVA and return to the previous page |

**Navigation rule:** `Tab` switches between Home, Filters, and Settings. Arrow keys navigate the results and rows *inside* the current section.

Search results are paginated. When more results are available, a selectable **Next page** row appears after the current results. The current list stays in place while the next page is appended, and overlapping results are deduplicated.

## API mode and direct scraping

**Settings → Load through APIs** is a global toggle for all three sources. When enabled, YouTube uses the configured Invidious API, SoundCloud uses Soundcloak public instances, and Genius searches/loads lyrics through public Dumb and Intellectual instances. The **Genius frontend** setting lets you pick `auto`, `dumb`, or `intellectual`; `auto` tries both instance lists. Instance lists are fetched from their upstream `instances.json` files and have a small hard-coded fallback list if a list cannot be reached.

When disabled, jabberwock scrapes each original site directly: YouTube's own search HTML/continuation flow, SoundCloud's web assets and API, and Genius's own search/song pages. This is not offline mode; direct scraping still requires an internet connection and upstream sites can change their markup or access policies. API mode defaults to enabled; the setting is saved in `use_apis`.

Filters include relevance/views/date sorting, upload time, duration, and a default-on **Hide Shorts** option. YouTube's HTML does not have a stable public scraping contract, and some Invidious instances do not identify every Short, so the backend may need adjustments if its upstream responses change.

## SoundCloud

SoundCloud mode searches tracks through the **Soundcloak API**, using the public instances list at `https://maid.zone/soundcloak/instances.json`. Jabberwock filters the list to instances whose API is enabled and whose status does not report an error, caches it for ten minutes, and fails over to another listed instance when requests fail. You may set `soundcloud_instance` in the config to prefer a particular base URL; leave it empty for automatic selection. SoundCloud requests normally do not require a local SoundCloud client ID because Soundcloak handles upstream API requests server-side.

Soundcloak's `/_/api/v2/search/tracks` endpoint provides search results. For playback, jabberwock uses the instance's restream endpoint when enabled (or its progressive endpoint otherwise). Tracks are filtered by default: blocked or preview-only tracks, SoundCloud Go/subscriber-only tracks, DRM/encrypted streams, and tracks without a supported audio stream are excluded.

When CAVA is installed, jabberwock starts the visualizer before `mpv`. It now tries the user's normal CAVA configuration first rather than forcing a backend that might remain alive but capture nothing; if that process exits during startup, it tries detected PipeWire and PulseAudio-compatible configurations. CAVA runs in noncurses terminal-output mode so jabberwock can continue polling for `Esc`. If all attempts exit during startup, audio continues with plain `mpv`. Press `Esc` to stop audio and CAVA and return to the same results page.

## Genius

Genius mode presents song title/artist results. Press `Enter` to open a scrollable lyrics view; press `Enter` on the **Copy lyrics** action (or use `c`) to copy the entire lyrics text to the clipboard (using `wl-copy`, `xclip`, `xsel`, `pbcopy`, `clip.exe`, or OSC 52 when supported). With API mode enabled, searches and lyrics use public instances from the Dumb and Intellectual `instances.json` lists; with API mode disabled, jabberwock talks directly to Genius. The scraper is written with the Go standard library, following the lightweight public-endpoint and HTML-parsing approach of [dumb](https://github.com/rramiachraf/dumb); [intellectual](https://github.com/Insprill/intellectual) is another related project. These upstream services can change markup, rate-limit requests, or restrict automated access, so a failed search or lyrics fetch may require scraper updates. Use lyrics for personal, lawful purposes and respect Genius and instance terms.

## Settings and customization

By default, settings are stored at `~/.config/jabberwock/config.json`. Override the path with `JABBERWOCK_CONFIG=/path/to/config.json`.

Example config:

```json
{
  "use_apis": true,
  "backend": "invidious",
  "genius_frontend": "auto",
  "invidious_url": "https://yewtu.be",
  "soundcloud_instances_url": "https://maid.zone/soundcloak/instances.json",
  "soundcloud_instance": "",
  "theme": "wock",
  "soundcloud_theme": "soundcloud",
  "themes": {
    "midnight": {
      "name": "midnight",
      "title": "1;35",
      "accent": "36",
      "text": "37",
      "muted": "90",
      "selected": "1;30;45",
      "error": "1;31",
      "border": "35"
    }
  },
  "keybinds": {
    "ctrl+r": "refresh",
    "home": "top",
    "ctrl+p": "switch_source"
  }
}
```

Theme values are ANSI SGR parameter strings (e.g. `36`, `1;35`, `38;5;110`). YouTube and Genius use `theme`; SoundCloud defaults to the built-in orange-accent `soundcloud` theme via `soundcloud_theme`. Change the active source's theme in **Settings → Theme**, or add a theme under `themes`. `use_apis` selects public proxy/frontends versus direct scraping for all sources, and `genius_frontend` selects `auto`, `dumb`, or `intellectual` while API mode is enabled. `keybinds` maps keys to built-in action names: `search`, `quit`, `next_view`, `switch_source`, `refresh`, `home`, `top`, or `bottom`. Go forks can add callbacks with `RegisterAction` and bindings with `RegisterKeybind`; themes can be registered with `RegisterTheme` in `config.go`.

The client writes its configuration file only. Search results stay in memory; no playback history or subscriptions are stored locally.

## Project layout

- `main.go` — raw terminal handling, unified UI, navigation, and playback orchestration
- `config.go` — settings, theme registry, and keybind/action extension points
- `media.go` / `yt_adapter.go` — common search-result model and YouTube adapter
- `filters.go` — common date/duration sorting and filter UI
- `yt/` — YouTube HTML / Invidious search implementation and parser tests
- `soundcloud/` — Soundcloak instance selection/failover, SoundCloud search/pagination, track filters, and audio URLs
- `genius/` — direct Genius scraping plus Dumb/Intellectual instance discovery, search, and lyrics extraction
- `clipboard.go` — clipboard helper/OSC 52 fallback for the Copy lyrics action

## Related projects

The SoundCloud client uses Soundcloak’s public API and instance list. The Genius scraper follows public-search and HTML-extraction ideas used by [dumb](https://github.com/rramiachraf/dumb) and [intellectual](https://github.com/Insprill/intellectual); no source files were copied from those projects. See their repositories and instance terms for details.

## Repairing duplicate declarations after a manual merge

If `go build` reports that `Video` is redeclared in `scrape.go` and `media.go`, or that `SearchCursor`, `SearchPage`, or `SearchVideosPage` are redeclared in `scrape.go` and `yt_adapter.go`, the checkout contains an obsolete duplicate file at the project root. Delete **only the root-level `scrape.go`**; do not delete `yt/scrape.go`.

The current layout deliberately keeps each definition in one place:

- `media.go`: the unified UI `Video` model
- `yt_adapter.go`: UI-facing `SearchCursor`, `SearchPage`, and `SearchVideosPage` wrapper; `SearchCursor` includes SoundCloud pagination fields
- `yt/scrape.go`: YouTube-specific scraping implementation (`package yt`)

Then run:

```sh
rm ./scrape.go        # only if this obsolete root-level file exists
 gofmt -w media.go yt_adapter.go main.go
 go test ./...
 go vet ./...
 go build -o jabberwock .
```

Do not copy `yt/scrape.go` into the project root. The root-level scraper file belongs to an older layout and conflicts with the unified YouTube/SoundCloud adapter.

The SoundCloud scraper was implemented specifically for jabberwock using Go's standard library. Its design was informed by the public API/streaming approaches described by [soundcloak](https://github.com/maid-zone/soundcloak) and [wisp](https://github.com/end-sky/wisp); Soundcloak's source code was not copied into this project. See each repository for its respective license and implementation details.
