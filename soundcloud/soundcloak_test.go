package soundcloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSoundcloakSearchUsesHealthyInstanceAndRestreamURL(t *testing.T) {
	var instanceCalls, searchCalls int
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/instances.json":
			instanceCalls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]cloakInstance{
				{URL: "https://offline.invalid", Settings: struct {
					EnableAPI    bool `json:"EnableAPI"`
					Restream     bool `json:"Restream"`
					ProxyStreams bool `json:"ProxyStreams"`
				}{EnableAPI: true, Restream: true}, Status: struct {
					Error string `json:"Error"`
				}{Error: "offline"}},
				{URL: testInstanceURLPlaceholder, Settings: struct {
					EnableAPI    bool `json:"EnableAPI"`
					Restream     bool `json:"Restream"`
					ProxyStreams bool `json:"ProxyStreams"`
				}{EnableAPI: true, Restream: true}},
			})
		case "/_/api/v2/search/tracks":
			searchCalls++
			gotQuery = r.URL.Query().Get("q")
			if r.URL.Query().Get("offset") != "40" || r.URL.Query().Get("limit") != "20" {
				t.Errorf("unexpected paging query: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"next_href":"https://api-v2.soundcloud.com/search/tracks?offset=60","collection":[
				{"id":123,"title":"Playable song","permalink_url":"https://soundcloud.com/artist/ambient-song","duration":123000,"policy":"ALLOW","access":"playable","monetization_model":"AD_SUPPORTED","media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/123/stream/hls","preset":"aac_160k","format":{"protocol":"hls","mime_type":"application/vnd.apple.mpegurl"}}]}},
				{"id":124,"title":"Go-only song","permalink_url":"https://soundcloud.com/artist/go-song","duration":123000,"policy":"ALLOW","access":"blocked","monetization_model":"SUB_HIGH_TIER","media":{"transcodings":[{"url":"https://api-v2.soundcloud.com/media/124/stream/hls","preset":"aac_160k","format":{"protocol":"hls","mime_type":"application/vnd.apple.mpegurl"}}]}}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	testInstanceURLPlaceholder = server.URL
	defer func() { testInstanceURLPlaceholder = "" }()

	client := &CloakClient{HTTPClient: server.Client(), InstancesURL: server.URL + "/instances.json"}
	page, err := client.Search(context.Background(), "ambient jazz", 40, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if instanceCalls != 1 || searchCalls != 1 {
		t.Fatalf("instance calls=%d search calls=%d, want 1 each", instanceCalls, searchCalls)
	}
	if gotQuery != "ambient jazz" {
		t.Fatalf("query = %q", gotQuery)
	}
	if len(page.Tracks) != 1 {
		t.Fatalf("got %d tracks, want one playable track: %#v", len(page.Tracks), page.Tracks)
	}
	if !page.HasMore || page.NextOffset != 42 {
		t.Fatalf("unexpected pagination metadata: %#v", page)
	}
	wantPrefix := server.URL + "/_/api/restream/artist/ambient-song?audio=mpeg"
	if page.Tracks[0].StreamURL != wantPrefix {
		t.Fatalf("stream URL = %q, want %q", page.Tracks[0].StreamURL, wantPrefix)
	}
	if client.PreferredInstance() != server.URL {
		t.Fatalf("selected instance = %q, want %q", client.PreferredInstance(), server.URL)
	}
}

var testInstanceURLPlaceholder string

func TestSoundcloakSkipsInstancesWithoutAPISupport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instances.json" {
			t.Fatal("search should not be sent because there are no API-enabled instances")
		}
		_, _ = w.Write([]byte(`[{"URL":"https://no-api.invalid","Settings":{"EnableAPI":false,"Restream":false},"Status":{"Error":""}}]`))
	}))
	defer server.Close()
	client := &CloakClient{HTTPClient: server.Client(), InstancesURL: server.URL + "/instances.json"}
	_, err := client.Search(context.Background(), "test", 0, 20, "")
	if err == nil || !strings.Contains(err.Error(), "no healthy Soundcloak instances") {
		t.Fatalf("expected helpful no-healthy-instances error, got %v", err)
	}
}

func TestSoundcloakSearchFailsOverToAnotherHealthyInstance(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_/api/v2/search/tracks" {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_/api/v2/search/tracks" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"collection":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer second.Close()
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"URL":"` + first.URL + `","Settings":{"EnableAPI":true,"Restream":false},"Status":{"Error":""}},
			{"URL":"` + second.URL + `","Settings":{"EnableAPI":true,"Restream":false},"Status":{"Error":""}}
		]`))
	}))
	defer list.Close()

	client := &CloakClient{HTTPClient: list.Client(), InstancesURL: list.URL + "/instances.json"}
	page, err := client.Search(context.Background(), "ambient", 0, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tracks) != 0 {
		t.Fatalf("expected empty second-instance result, got %#v", page.Tracks)
	}
	if client.PreferredInstance() != second.URL {
		t.Fatalf("selected instance = %q, want failover instance %q", client.PreferredInstance(), second.URL)
	}
}
