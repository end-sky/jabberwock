package genius

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearchAndLyricsThroughPublicGeniusPages(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search/multi":
			if r.URL.Query().Get("q") != "test artist song" {
				t.Errorf("unexpected search query %q", r.URL.Query().Get("q"))
			}
			w.Header().Set("Content-Type", "application/json")
			payload := map[string]any{"response": map[string]any{"sections": []any{map[string]any{
				"type": "song", "hits": []any{map[string]any{
					"type": "song", "result": map[string]any{
						"id": 42, "title": "Example Song", "full_title": "Example Song by Example Artist",
						"url": server.URL + "/Example-artist-example-song-lyrics", "path": "/Example-artist-example-song-lyrics",
						"artist_names": "Example Artist", "song_art_image_thumbnail_url": "https://images.example/art.jpg",
					},
				}},
			}}}}
			_ = json.NewEncoder(w).Encode(payload)
		case "/Example-artist-example-song-lyrics":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><div data-lyrics-container="true">Line one<br/>Line &amp; two<div>Line three</div></div></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{HTTPClient: server.Client(), SearchURL: server.URL + "/api/search/multi", DumbInstances: []string{}}
	songs, err := client.Search(context.Background(), "test artist song")
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != 1 || songs[0].Title != "Example Song" || songs[0].Artist != "Example Artist" {
		t.Fatalf("unexpected search result: %#v", songs)
	}
	lyrics, err := client.Lyrics(context.Background(), songs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Line one", "Line & two", "Line three"} {
		if !strings.Contains(lyrics, expected) {
			t.Fatalf("lyrics %q do not contain %q", lyrics, expected)
		}
	}
}

func TestParseDumbSearchIgnoresNavigationLinks(t *testing.T) {
	got := parseDumbSearch([]byte(`<a href="/search?q=foo">Search</a><a href="/Artist-Song-lyrics">Artist - Song</a><a href="/about">About</a>`), "https://dumb.example")
	if len(got) != 1 || got[0].Title != "Artist - Song" || got[0].Path != "Artist-Song-lyrics" {
		t.Fatalf("unexpected Dumb parse results: %#v", got)
	}
}

func TestAPIModeSearchesConfiguredDumbAndUsesMirrorForLyrics(t *testing.T) {
	var listCalls, searchCalls, lyricsCalls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dumb-instances.json":
			listCalls++
			_, _ = w.Write([]byte(`[{"clearnet":"` + server.URL + `"}]`))
		case "/search":
			searchCalls++
			if r.URL.Query().Get("q") != "example song" {
				t.Errorf("search query = %q", r.URL.Query().Get("q"))
			}
			_, _ = w.Write([]byte(`<a href="/Artist-Song-lyrics">Artist - Song</a>`))
		case "/Artist-Song-lyrics":
			lyricsCalls++
			_, _ = w.Write([]byte(`<main><div data-lyrics-container="true">Full<br/>lyrics</div></main>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{
		HTTPClient: server.Client(), UseAPI: true, Frontend: "dumb",
		DumbInstancesURL:         server.URL + "/dumb-instances.json",
		IntellectualInstancesURL: server.URL + "/intellectual-instances.json",
	}
	songs, err := client.Search(context.Background(), "example song")
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != 1 || songs[0].MirrorURL != server.URL+"/Artist-Song-lyrics" {
		t.Fatalf("unexpected API search results: %#v", songs)
	}
	lyrics, err := client.Lyrics(context.Background(), songs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lyrics, "Full") || !strings.Contains(lyrics, "lyrics") {
		t.Fatalf("unexpected lyrics: %q", lyrics)
	}
	if listCalls < 1 || searchCalls != 1 || lyricsCalls != 1 {
		t.Fatalf("calls: list=%d search=%d lyrics=%d", listCalls, searchCalls, lyricsCalls)
	}
}

func TestIntellectualFrontendInstanceListIsSupported(t *testing.T) {
	var searched bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/intellectual-instances.json":
			_, _ = w.Write([]byte(`[{"clearnet":"` + server.URL + `"}]`))
		case "/search":
			searched = true
			_, _ = w.Write([]byte(`<a href="/song/artist-title">Artist — Title</a>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{HTTPClient: server.Client(), UseAPI: true, Frontend: "intellectual", IntellectualInstancesURL: server.URL + "/intellectual-instances.json"}
	results, err := client.Search(context.Background(), "artist title")
	if err != nil {
		t.Fatal(err)
	}
	if !searched || len(results) != 1 || !strings.Contains(results[0].MirrorURL, "/song/artist-title") {
		t.Fatalf("Intellectual search did not parse expected result: searched=%v results=%#v", searched, results)
	}
}

func TestDirectModeDoesNotQueryPublicFrontendInstances(t *testing.T) {
	var listCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/instances.json" {
			listCalls++
			http.Error(w, "should not be requested", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/search/multi" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"response":{"sections":[]}}`))
	}))
	defer server.Close()
	client := &Client{HTTPClient: server.Client(), SearchURL: server.URL + "/search/multi", UseAPI: false, DumbInstancesURL: server.URL + "/instances.json", IntellectualInstancesURL: server.URL + "/instances.json"}
	results, err := client.Search(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 || listCalls != 0 {
		t.Fatalf("direct mode touched public instances: results=%#v listCalls=%d", results, listCalls)
	}
}
