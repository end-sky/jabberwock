# Fix for duplicate scraper declarations

The errors described in the report are caused by an obsolete **root-level** `scrape.go` being present alongside the split architecture.

From the project root:

```sh
rm ./scrape.go        # only remove the root-level duplicate
 gofmt -w media.go yt_adapter.go main.go
 go test ./...
 go vet ./...
 go build -o jabberwock .
```

Keep `yt/scrape.go`. It is the YouTube scraper implementation in the `yt` package. The intended root-level definitions are `Video` in `media.go` and `SearchCursor`, `SearchPage`, and the `SearchVideosPage` adapter in `yt_adapter.go`. The UI adapter's `SearchCursor` includes `SoundCloudOffset` and `SoundCloudNextURL` for SoundCloud pagination.

If your root `scrape.go` contains local changes not present in `yt/scrape.go`, back it up before removing it; do not flatten both implementations into the root `main` package.
