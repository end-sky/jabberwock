# Genius backend

This package provides standard-library-only Genius song search and lyrics extraction for the TUI. Search uses the public `genius.com/api/search/multi` endpoint; lyrics are extracted from song-page HTML. If direct Genius requests are blocked, the scraper tries public Dumb frontend instances (including the current Dumb instances list). HTML and upstream instance contracts may change. Run tests with `go test ./genius`.
