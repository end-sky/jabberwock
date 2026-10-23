// Package genius provides a small, standard-library-only Genius song search
// and lyrics scraper. Search follows the public endpoint used by lightweight
// frontends such as dumb; lyric text is extracted from the song page, with a
// Dumb instance fallback when Genius blocks the client directly.
package genius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultSearchURL           = "https://genius.com/api/search/multi"
	defaultDumbListURL         = "https://raw.githubusercontent.com/rramiachraf/dumb/main/instances.json"
	defaultIntellectualListURL = "https://raw.githubusercontent.com/Insprill/intellectual/master/instances.json"
	maxResponseBytes           = 12 << 20
)

var (
	lyricsContainerRE = regexp.MustCompile(`(?is)<div\b[^>]*data-lyrics-container\s*=\s*["']true["'][^>]*>`)
	divTagRE          = regexp.MustCompile(`(?is)</?div\b[^>]*>`)
	stripTagRE        = regexp.MustCompile(`(?s)<[^>]+>`)
	brTagRE           = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockEndRE        = regexp.MustCompile(`(?i)</(?:p|div|li|h[1-6])\s*>`)
	scriptStyleRE     = regexp.MustCompile(`(?is)<(?:script|style)\b[^>]*>.*?</(?:script|style)\s*>`)
	linkRE            = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a\s*>`)
)

type Client struct {
	HTTPClient               *http.Client
	SearchURL                string
	UseAPI                   bool
	Frontend                 string // auto, dumb, intellectual
	DumbInstancesURL         string
	IntellectualInstancesURL string
	DumbInstances            []string
	IntellectualInstances    []string
}

type Song struct {
	ID        string
	MirrorURL string
	Title     string
	Artist    string
	FullTitle string
	URL       string
	Path      string // Genius path, retained for Dumb's matching route.
	Thumbnail string
}

type searchPayload struct {
	Response struct {
		Sections []struct {
			Type string `json:"type"`
			Hits []struct {
				Type   string `json:"type"`
				Result struct {
					ID                       int64  `json:"id"`
					Title                    string `json:"title"`
					FullTitle                string `json:"full_title"`
					URL                      string `json:"url"`
					Path                     string `json:"path"`
					ArtistNames              string `json:"artist_names"`
					SongArtImageThumbnailURL string `json:"song_art_image_thumbnail_url"`
					PrimaryArtist            struct {
						Name string `json:"name"`
					} `json:"primary_artist"`
				} `json:"result"`
			} `json:"hits"`
		} `json:"sections"`
	} `json:"response"`
}

type dumbInstance struct {
	Clearnet string `json:"clearnet"`
}

func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 16 * time.Second}
}

func (c *Client) searchURL() string {
	if strings.TrimSpace(c.SearchURL) != "" {
		return strings.TrimSpace(c.SearchURL)
	}
	return defaultSearchURL
}

func (c *Client) dumbListURL() string {
	if strings.TrimSpace(c.DumbInstancesURL) != "" {
		return strings.TrimSpace(c.DumbInstancesURL)
	}
	return defaultDumbListURL
}

func (c *Client) get(ctx context.Context, target string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/125 Safari/537.36 jabberwock/0.1")
	req.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header.Get("Content-Type"), fmt.Errorf("HTTP %s from %s", resp.Status, req.URL.Host)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxResponseBytes {
		return nil, "", errors.New("response exceeds size limit")
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// Search switches between direct Genius scraping and API-backed privacy frontends.
// With UseAPI disabled it only talks to Genius itself. With UseAPI enabled it
// searches Dumb and/or Intellectual according to Frontend, trying healthy
// instances from their live instances.json lists and falling over to another.
func (c *Client) Search(ctx context.Context, query string) ([]Song, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("enter a search query")
	}
	if c.UseAPI {
		return c.searchViaFrontends(ctx, query)
	}
	return c.searchDirect(ctx, query)
}

func (c *Client) searchDirect(ctx context.Context, query string) ([]Song, error) {
	u, err := url.Parse(c.searchURL())
	if err != nil {
		return nil, err
	}
	params := u.Query()
	params.Set("q", query)
	u.RawQuery = params.Encode()
	body, contentType, err := c.get(ctx, u.String())
	if err != nil {
		return nil, err
	}
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		return nil, errors.New("Genius search was intercepted by an HTML challenge; enable API mode to use Dumb/Intellectual frontends")
	}
	var payload searchPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Genius search response: %w", err)
	}
	return parseSearch(payload), nil
}

func parseSearch(payload searchPayload) []Song {
	out := make([]Song, 0, 20)
	seen := map[string]bool{}
	for _, section := range payload.Response.Sections {
		for _, hit := range section.Hits {
			if hit.Type != "" && hit.Type != "song" {
				continue
			}
			if section.Type != "" && section.Type != "song" && hit.Type == "" {
				continue
			}
			item := hit.Result
			if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.URL) == "" {
				continue
			}
			id := fmt.Sprint(item.ID)
			if item.ID == 0 {
				id = item.URL
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			artist := strings.TrimSpace(item.ArtistNames)
			if artist == "" {
				artist = strings.TrimSpace(item.PrimaryArtist.Name)
			}
			p := strings.TrimSpace(item.Path)
			if p == "" {
				if u, err := url.Parse(item.URL); err == nil {
					p = strings.TrimPrefix(u.Path, "/")
				}
			}
			out = append(out, Song{ID: id, Title: strings.TrimSpace(item.Title), Artist: artist, FullTitle: strings.TrimSpace(item.FullTitle), URL: strings.TrimSpace(item.URL), Path: p, Thumbnail: strings.TrimSpace(item.SongArtImageThumbnailURL)})
		}
	}
	return out
}

type frontendInstance struct {
	Base string
	Kind string
}

func (c *Client) instanceListURL(kind string) string {
	if kind == "intellectual" {
		if strings.TrimSpace(c.IntellectualInstancesURL) != "" {
			return c.IntellectualInstancesURL
		}
		return defaultIntellectualListURL
	}
	if strings.TrimSpace(c.DumbInstancesURL) != "" {
		return c.DumbInstancesURL
	}
	return defaultDumbListURL
}

func (c *Client) frontendInstances(ctx context.Context) []frontendInstance {
	wanted := []string{"dumb", "intellectual"}
	switch strings.ToLower(strings.TrimSpace(c.Frontend)) {
	case "dumb":
		wanted = []string{"dumb"}
	case "intellectual":
		wanted = []string{"intellectual"}
	}
	out := make([]frontendInstance, 0, 12)
	seen := map[string]bool{}
	for _, kind := range wanted {
		configured := c.DumbInstances
		if kind == "intellectual" {
			configured = c.IntellectualInstances
		}
		list := validateInstances(configured)
		if len(list) == 0 {
			body, _, err := c.get(ctx, c.instanceListURL(kind))
			if err == nil {
				var payload []dumbInstance
				if json.Unmarshal(body, &payload) == nil {
					var raw []string
					for _, item := range payload {
						if strings.TrimSpace(item.Clearnet) != "" {
							raw = append(raw, item.Clearnet)
						}
					}
					list = validateInstances(raw)
				}
			}
		}
		if len(list) == 0 {
			if kind == "intellectual" {
				list = validateInstances([]string{"https://intellectual.insprill.net", "https://intellectual.catsarch.com", "https://intellectual.ducks.party", "https://in.bloat.cat", "https://int.dc09.xyz"})
			} else {
				list = validateInstances([]string{"https://dumb.ducks.party", "https://dumb.privacydev.net", "https://dumb.bloat.cat", "https://dumb.jeikobu.net", "https://dumb.canine.tools"})
			}
		}
		for _, base := range list {
			if !seen[base] {
				seen[base] = true
				out = append(out, frontendInstance{Base: base, Kind: kind})
			}
		}
	}
	return out
}

func validateInstances(list []string) []string {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, candidate := range list {
		u, err := url.Parse(strings.TrimSpace(candidate))
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			continue
		}
		base := strings.TrimRight(u.String(), "/")
		if !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	return out
}

func (c *Client) searchViaFrontends(ctx context.Context, query string) ([]Song, error) {
	instances := c.frontendInstances(ctx)
	var lastErr error
	for _, instance := range instances {
		u, err := url.Parse(strings.TrimRight(instance.Base, "/") + "/search")
		if err != nil {
			continue
		}
		q := u.Query()
		q.Set("q", query)
		u.RawQuery = q.Encode()
		body, _, err := c.get(ctx, u.String())
		if err != nil {
			lastErr = err
			continue
		}
		results := parseFrontendSearch(body, instance.Base)
		if len(results) > 0 {
			return results, nil
		}
		lastErr = fmt.Errorf("%s instance returned no parseable song links", instance.Kind)
	}
	if lastErr == nil {
		lastErr = errors.New("no public Dumb/Intellectual instances are available")
	}
	return nil, lastErr
}

// Keep the legacy unexported method for older local forks and focused tests.
func (c *Client) dumbInstances(ctx context.Context) []string {
	var out []string
	for _, instance := range c.frontendInstances(ctx) {
		if instance.Kind == "dumb" {
			out = append(out, instance.Base)
		}
	}
	return out
}

func parseDumbSearch(body []byte, base string) []Song {
	return parseFrontendSearch(body, base)
}

func parseFrontendSearch(body []byte, base string) []Song {
	out := make([]Song, 0, 20)
	seen := map[string]bool{}
	baseURL, err := url.Parse(base)
	if err != nil {
		return out
	}
	for _, match := range linkRE.FindAllSubmatch(body, -1) {
		if len(match) != 3 {
			continue
		}
		href := html.UnescapeString(strings.TrimSpace(string(match[1])))
		label := strings.TrimSpace(html.UnescapeString(stripTagRE.ReplaceAllString(string(match[2]), " ")))
		if label == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			continue
		}
		p, err := url.Parse(href)
		if err != nil {
			continue
		}
		if p.IsAbs() {
			if !strings.EqualFold(p.Host, baseURL.Host) {
				continue
			}
		} else if !strings.HasPrefix(p.Path, "/") {
			continue
		}
		pathPart := strings.Trim(p.Path, "/")
		if pathPart == "" || strings.HasPrefix(pathPart, "search") || strings.HasPrefix(pathPart, "static") || strings.HasPrefix(pathPart, "about") || strings.HasPrefix(pathPart, "assets") || strings.HasPrefix(pathPart, "favicon") {
			continue
		}
		mirror := baseURL.ResolveReference(p).String()
		key := mirror
		if seen[key] {
			continue
		}
		seen[key] = true
		canonicalPath := strings.TrimSuffix(pathPart, "-lyrics")
		out = append(out, Song{ID: key, MirrorURL: mirror, Title: label, FullTitle: label, URL: "https://genius.com/" + pathPart, Path: pathPart, Artist: ""})
		_ = canonicalPath
		if len(out) == 20 {
			break
		}
	}
	return out
}

// Lyrics retrieves the Genius song page and extracts text from its lyrics
// containers. If Genius blocks direct requests, it asks a Dumb frontend for the
// corresponding song page. The scraper returns text only and does not execute JS.
func (c *Client) Lyrics(ctx context.Context, song Song) (string, error) {
	if strings.TrimSpace(song.URL) == "" && strings.TrimSpace(song.MirrorURL) == "" {
		return "", errors.New("song has no lyrics URL")
	}
	if c.UseAPI {
		var lastErr error
		if strings.TrimSpace(song.MirrorURL) != "" {
			if body, _, err := c.get(ctx, song.MirrorURL); err == nil {
				if lyrics := extractLyrics(body); lyrics != "" {
					return lyrics, nil
				}
				if lyrics := extractDumbLyrics(body); lyrics != "" {
					return lyrics, nil
				}
				lastErr = errors.New("selected frontend returned no extractable lyrics")
			} else {
				lastErr = err
			}
		}
		for _, instance := range c.frontendInstances(ctx) {
			pathPart := strings.Trim(strings.TrimSpace(song.Path), "/")
			if pathPart == "" {
				if u, err := url.Parse(song.URL); err == nil {
					pathPart = strings.Trim(u.Path, "/")
				}
			}
			candidates := []string{pathPart, strings.TrimSuffix(pathPart, "-lyrics")}
			for _, pathCandidate := range candidates {
				if pathCandidate == "" {
					continue
				}
				target := strings.TrimRight(instance.Base, "/") + "/" + strings.TrimLeft(pathCandidate, "/")
				body, _, err := c.get(ctx, target)
				if err != nil {
					lastErr = err
					continue
				}
				if lyrics := extractLyrics(body); lyrics != "" {
					return lyrics, nil
				}
				if lyrics := extractDumbLyrics(body); lyrics != "" {
					return lyrics, nil
				}
				lastErr = errors.New("frontend returned a page without extractable lyrics")
			}
		}
		if lastErr == nil {
			lastErr = errors.New("no public lyric frontend was available")
		}
		return "", fmt.Errorf("lyrics via Dumb/Intellectual failed: %w", lastErr)
	}
	body, _, err := c.get(ctx, song.URL)
	if err != nil {
		return "", fmt.Errorf("fetch Genius lyrics: %w", err)
	}
	if lyrics := extractLyrics(body); lyrics != "" {
		return lyrics, nil
	}
	return "", errors.New("Genius page did not contain readable lyrics markup; enable API mode to use Dumb/Intellectual frontends")
}

func extractLyrics(body []byte) string {
	starts := lyricsContainerRE.FindAllIndex(body, -1)
	if len(starts) == 0 {
		return ""
	}
	var chunks []string
	for _, bounds := range starts {
		start := bounds[1]
		depth := 1
		end := start
		for _, tag := range divTagRE.FindAllIndex(body[start:], -1) {
			tagStart := start + tag[0]
			tagEnd := start + tag[1]
			t := strings.ToLower(string(body[tagStart:tagEnd]))
			if strings.HasPrefix(t, "</div") {
				depth--
			} else if !strings.HasSuffix(strings.TrimSpace(t), "/>") {
				depth++
			}
			if depth == 0 {
				end = tagStart
				break
			}
		}
		if end > start {
			if text := cleanHTML(string(body[start:end])); text != "" {
				chunks = append(chunks, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(chunks, "\n\n"))
}

func extractDumbLyrics(body []byte) string {
	text := string(body)
	// The text-only Dumb template commonly wraps lyrics in an article/main or
	// element with a lyrics-related class; prefer that section over page chrome.
	classSectionRE := regexp.MustCompile(`(?is)<(?:main|article|section|div)\b[^>]*(?:class|id)=["'][^"']*(?:lyrics|song-text|song_content)[^"']*["'][^>]*>(.*?)</(?:main|article|section|div)\s*>`)
	if match := classSectionRE.FindStringSubmatch(text); len(match) == 2 {
		if result := cleanHTML(match[1]); len(result) > 80 {
			return result
		}
	}
	return ""
}

func cleanHTML(value string) string {
	value = scriptStyleRE.ReplaceAllString(value, " ")
	value = brTagRE.ReplaceAllString(value, "\n")
	value = blockEndRE.ReplaceAllString(value, "\n")
	value = stripTagRE.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	lines := strings.Split(strings.ReplaceAll(value, "\r", ""), "\n")
	clean := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if len(clean) > 0 && !blank {
				clean = append(clean, "")
				blank = true
			}
			continue
		}
		clean = append(clean, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(clean, "\n"))
}
