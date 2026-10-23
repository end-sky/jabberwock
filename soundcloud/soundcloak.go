package soundcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const defaultInstancesURL = "https://maid.zone/soundcloak/instances.json"

// CloakClient uses Soundcloak's public proxy instead of SoundCloud's private
// client ID. Instance data is cached briefly and healthy API-enabled instances
// are tried in order, with failover if one goes offline.
type CloakClient struct {
	HTTPClient    *http.Client
	InstancesURL  string
	PreferredBase string

	mu          sync.Mutex
	instances   []cloakInstance
	loadedAt    time.Time
	selectedURL string
}

type cloakInstance struct {
	URL      string `json:"URL"`
	Settings struct {
		EnableAPI    bool `json:"EnableAPI"`
		Restream     bool `json:"Restream"`
		ProxyStreams bool `json:"ProxyStreams"`
	} `json:"Settings"`
	Status struct {
		Error string `json:"Error"`
	} `json:"Status"`
}

func (c *CloakClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 18 * time.Second}
}

func (c *CloakClient) instancesURL() string {
	if strings.TrimSpace(c.InstancesURL) != "" {
		return strings.TrimSpace(c.InstancesURL)
	}
	return defaultInstancesURL
}

func (c *CloakClient) get(ctx context.Context, target string, maxBytes int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "jabberwock/0.1 (+https://github.com/jabberwock)")
	req.Header.Set("Accept", "application/json, text/html, audio/*, */*")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header.Get("Content-Type"), fmt.Errorf("HTTP %s from %s", resp.Status, req.URL.Host)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > maxBytes {
		return nil, "", errors.New("response exceeds size limit")
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func (c *CloakClient) healthyInstances(ctx context.Context, refresh bool) ([]cloakInstance, error) {
	c.mu.Lock()
	if !refresh && len(c.instances) != 0 && time.Since(c.loadedAt) < 10*time.Minute {
		cached := append([]cloakInstance(nil), c.instances...)
		c.mu.Unlock()
		return prioritizeInstance(c.PreferredBase, cached), nil
	}
	c.mu.Unlock()

	body, _, err := c.get(ctx, c.instancesURL(), 2<<20)
	if err != nil {
		return nil, fmt.Errorf("load Soundcloak instances list: %w", err)
	}
	var entries []cloakInstance
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("decode Soundcloak instances list: %w", err)
	}
	healthy := make([]cloakInstance, 0, len(entries))
	seen := map[string]bool{}
	for _, item := range entries {
		u, parseErr := url.Parse(strings.TrimSpace(item.URL))
		if parseErr != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			continue
		}
		if !item.Settings.EnableAPI || strings.TrimSpace(item.Status.Error) != "" {
			continue
		}
		base := strings.TrimRight(u.String(), "/")
		if seen[base] {
			continue
		}
		seen[base] = true
		item.URL = base
		healthy = append(healthy, item)
	}
	if len(healthy) == 0 {
		return nil, errors.New("no healthy Soundcloak instances with API support are listed")
	}
	c.mu.Lock()
	c.instances = append([]cloakInstance(nil), healthy...)
	c.loadedAt = time.Now()
	c.mu.Unlock()
	return prioritizeInstance(c.PreferredBase, healthy), nil
}

func prioritizeInstance(preferred string, instances []cloakInstance) []cloakInstance {
	preferred = strings.TrimRight(strings.TrimSpace(preferred), "/")
	if preferred == "" {
		return instances
	}
	out := make([]cloakInstance, 0, len(instances))
	for _, instance := range instances {
		if instance.URL == preferred {
			out = append(out, instance)
			break
		}
	}
	for _, instance := range instances {
		if instance.URL != preferred {
			out = append(out, instance)
		}
	}
	return out
}

// Search uses Soundcloak's /_/api/v2/search/tracks proxy. Pagination is driven
// by offset rather than forwarding SoundCloud's upstream next_href to another
// host. The track's play URL is a Soundcloak restream/progressive endpoint.
func (c *CloakClient) Search(ctx context.Context, query string, offset, limit int, _ string) (SearchPage, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchPage{}, errors.New("enter a search query")
	}
	if offset < 0 {
		offset = 0
	}
	if limit < 1 || limit > 50 {
		limit = defaultLimit
	}

	instances, err := c.healthyInstances(ctx, false)
	if err != nil {
		return SearchPage{}, err
	}
	var lastErr error
	for _, instance := range instances {
		endpoint, err := url.Parse(instance.URL + "/_/api/v2/search/tracks")
		if err != nil {
			continue
		}
		q := endpoint.Query()
		q.Set("q", query)
		q.Set("limit", fmt.Sprint(limit))
		q.Set("offset", fmt.Sprint(offset))
		q.Set("linked_partitioning", "1")
		endpoint.RawQuery = q.Encode()

		body, _, requestErr := c.get(ctx, endpoint.String(), maxBodyBytes)
		if requestErr != nil {
			lastErr = requestErr
			continue
		}
		var response apiSearchResponse
		if err := json.Unmarshal(body, &response); err != nil {
			lastErr = fmt.Errorf("decode Soundcloak search response from %s: %w", instance.URL, err)
			continue
		}
		parser := &Client{}
		tracks := make([]Track, 0, len(response.Collection))
		for _, item := range response.Collection {
			track, ok := parser.parseTrack(item, "")
			if !ok || track.StreamURL == "" || !Playable(track) {
				continue
			}
			streamURL, err := soundcloakStreamURL(instance, track.PermalinkURL)
			if err != nil {
				continue
			}
			track.StreamURL = streamURL
			if !Playable(track) {
				continue
			}
			tracks = append(tracks, track)
		}
		nextOffset := offset + len(response.Collection)
		hasMore := strings.TrimSpace(response.NextHref) != "" || len(response.Collection) >= limit
		c.mu.Lock()
		c.selectedURL = instance.URL
		c.mu.Unlock()
		return SearchPage{Tracks: tracks, HasMore: hasMore, NextOffset: nextOffset}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("all listed Soundcloak instances failed")
	}
	// Refresh instance metadata once in case the list changed since its cache
	// was populated, then retry once via the newest list.
	fresh, refreshErr := c.healthyInstances(ctx, true)
	if refreshErr == nil {
		for _, instance := range fresh {
			endpoint, _ := url.Parse(instance.URL + "/_/api/v2/search/tracks")
			q := endpoint.Query()
			q.Set("q", query)
			q.Set("limit", fmt.Sprint(limit))
			q.Set("offset", fmt.Sprint(offset))
			q.Set("linked_partitioning", "1")
			endpoint.RawQuery = q.Encode()
			body, _, requestErr := c.get(ctx, endpoint.String(), maxBodyBytes)
			if requestErr != nil {
				lastErr = requestErr
				continue
			}
			var response apiSearchResponse
			if err := json.Unmarshal(body, &response); err != nil {
				lastErr = err
				continue
			}
			parser := &Client{}
			tracks := make([]Track, 0, len(response.Collection))
			for _, item := range response.Collection {
				track, ok := parser.parseTrack(item, "")
				if !ok || track.StreamURL == "" || !Playable(track) {
					continue
				}
				streamURL, err := soundcloakStreamURL(instance, track.PermalinkURL)
				if err != nil {
					continue
				}
				track.StreamURL = streamURL
				if Playable(track) {
					tracks = append(tracks, track)
				}
			}
			c.mu.Lock()
			c.selectedURL = instance.URL
			c.mu.Unlock()
			return SearchPage{Tracks: tracks, HasMore: strings.TrimSpace(response.NextHref) != "" || len(response.Collection) >= limit, NextOffset: offset + len(response.Collection)}, nil
		}
	}
	return SearchPage{}, fmt.Errorf("SoundCloud search via Soundcloak failed: %w", lastErr)
}

func soundcloakStreamURL(instance cloakInstance, permalink string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(permalink))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.EqualFold(u.Hostname(), "soundcloud.com") {
		return "", errors.New("track has an invalid SoundCloud permalink")
	}
	parts := strings.Split(strings.Trim(path.Clean(u.Path), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." {
		return "", errors.New("track permalink is not a regular user/track URL")
	}
	mode := "progressive"
	if instance.Settings.Restream {
		mode = "restream"
	}
	endpoint, err := url.Parse(instance.URL + "/_/api/" + mode + "/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]))
	if err != nil {
		return "", err
	}
	if mode == "restream" {
		q := endpoint.Query()
		q.Set("audio", "mpeg")
		endpoint.RawQuery = q.Encode()
	}
	return endpoint.String(), nil
}

// PreferredInstance reports the base URL that answered most recently. It is
// useful for diagnostics and intentionally does not persist any account data.
func (c *CloakClient) PreferredInstance() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedURL
}
