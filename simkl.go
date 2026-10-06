package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Simkl sync: teaches Seerr what has been seen, from the owner's Simkl lists.
//   - Seerr blocklist (hides titles from discovery): movies completed or dropped; shows/anime watching, completed,
//     dropped or on hold. Titles Sonarr/Radarr manage are skipped. Movies back on "plan to watch" or "watching"
//     (the owner wants a copy) are taken off the blocklist again.
//   - Cinema rule: a movie completed in Simkl before its home release (watch date earlier than Radarr's digital or
//     physical release date, or no home release yet) was seen in the cinema; if Radarr waits for it (monitored, no
//     file), it is unmonitored so it doesn't download. Movies watched after their home release (e.g. on a watchlist to
//     get a copy) are left alone.
//   - Collection rule: a movie on "plan to watch"/"watching" that Seerr already reports as available (it is in the
//     Jellyfin movie library) gets a Radarr exclusion, so Radarr's Simkl list doesn't download a second copy, and is
//     unmonitored if Radarr already waits for it.
// Downloading from Simkl lists is done by Sonarr/Radarr's own Simkl import lists.
// Auth: Simkl OAuth2 token file (device login once), renewed with its refresh token before it expires.

var (
	simklAPI   = "https://api.simkl.com"
	hideMovies = map[string]bool{"completed": true, "dropped": true}
	hideShows  = map[string]bool{"watching": true, "completed": true, "dropped": true, "hold": true}
	wantMovies = map[string]bool{"plantowatch": true, "watching": true}
)

type simklToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ObtainedAt   int64  `json:"obtained_at"`
	TokenType    string `json:"token_type,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type simklIDs struct {
	TMDB json.Number `json:"tmdb"`
}

type simklItem struct {
	Status        string `json:"status"`
	LastWatchedAt string `json:"last_watched_at"`
	Movie         *struct {
		Title string   `json:"title"`
		IDs   simklIDs `json:"ids"`
	} `json:"movie"`
	Show *struct {
		Title string   `json:"title"`
		IDs   simklIDs `json:"ids"`
	} `json:"show"`
}

type simklAll struct {
	Movies []simklItem `json:"movies"`
	Shows  []simklItem `json:"shows"`
	Anime  []simklItem `json:"anime"`
}

type mediaKey struct {
	Type string // "movie" or "tv"
	TMDB int
}

// Simkl is a minimal client for the Simkl API with a self-renewing OAuth2 token.
type Simkl struct {
	ClientID  string
	TokenFile string
	Client    *http.Client
	Now       func() time.Time
}

func NewSimkl(clientID, tokenFile string) *Simkl {
	return &Simkl{ClientID: clientID, TokenFile: tokenFile, Client: &http.Client{Timeout: 120 * time.Second}, Now: time.Now}
}

// token returns a valid access token, renewing it when less than a day is left.
func (s *Simkl) token() (string, error) {
	data, err := os.ReadFile(s.TokenFile)
	if err != nil {
		return "", fmt.Errorf("simkl token file: %w", err)
	}
	var t simklToken
	if err := json.Unmarshal(data, &t); err != nil {
		return "", fmt.Errorf("simkl token file: %w", err)
	}
	expires := time.Unix(t.ObtainedAt+t.ExpiresIn, 0)
	if t.AccessToken != "" && s.Now().Before(expires.Add(-24*time.Hour)) {
		return t.AccessToken, nil
	}
	if t.RefreshToken == "" {
		return "", errors.New("simkl token expired and no refresh token: sign in again")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}, "client_id": {s.ClientID}}
	req, _ := http.NewRequest("POST", simklAPI+"/oauth2/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("simkl-api-key", s.ClientID)
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("simkl token refresh: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return "", fmt.Errorf("simkl token refresh: %s", resp.Status)
	}
	var nt simklToken
	if err := json.NewDecoder(resp.Body).Decode(&nt); err != nil || nt.AccessToken == "" {
		return "", fmt.Errorf("simkl token refresh: bad response")
	}
	if nt.RefreshToken == "" {
		nt.RefreshToken = t.RefreshToken
	}
	nt.ObtainedAt = s.Now().Unix()
	out, _ := json.Marshal(nt)
	tmp := s.TokenFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, s.TokenFile); err != nil {
		return "", err
	}
	log.Printf("Simkl: access token renewed")
	return nt.AccessToken, nil
}

func (s *Simkl) AllItems() (*simklAll, error) {
	tok, err := s.token()
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", simklAPI+"/sync/all-items/?extended=full", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("simkl-api-key", s.ClientID)
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("simkl all-items: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("simkl all-items: %s", resp.Status)
	}
	var all simklAll
	return &all, json.NewDecoder(resp.Body).Decode(&all)
}

// Seerr is a minimal client for the Seerr API.
type Seerr struct {
	BaseURL string
	APIKey  string
	UserID  int
	Client  *http.Client
}

func NewSeerr(baseURL, apiKey string, userID int) *Seerr {
	return &Seerr{BaseURL: baseURL, APIKey: apiKey, UserID: userID, Client: &http.Client{Timeout: 60 * time.Second}}
}

func (s *Seerr) do(method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, s.BaseURL+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Api-Key", s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("seerr %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return resp.StatusCode, fmt.Errorf("seerr %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// Blocklist returns all blocklisted titles.
func (s *Seerr) Blocklist() (map[mediaKey]bool, error) {
	set := map[mediaKey]bool{}
	for skip := 0; ; skip += 100 {
		var page struct {
			PageInfo struct {
				Results int `json:"results"`
			} `json:"pageInfo"`
			Results []struct {
				TMDB      int    `json:"tmdbId"`
				MediaType string `json:"mediaType"`
			} `json:"results"`
		}
		if _, err := s.do("GET", fmt.Sprintf("/api/v1/blocklist?take=100&skip=%d", skip), nil, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Results {
			set[mediaKey{r.MediaType, r.TMDB}] = true
		}
		if len(page.Results) == 0 || skip+100 >= page.PageInfo.Results {
			return set, nil
		}
	}
}

func (s *Seerr) Block(k mediaKey, title string) error {
	code, err := s.do("POST", "/api/v1/blocklist", map[string]any{"tmdbId": k.TMDB, "mediaType": k.Type, "title": title, "user": s.UserID}, nil)
	if code == http.StatusPreconditionFailed { // already blocklisted
		return nil
	}
	return err
}

// MovieAvailable reports whether Seerr knows the movie as available (Jellyfin library), with title and year.
func (s *Seerr) MovieAvailable(tmdb int) (bool, string, int, error) {
	var m struct {
		Title       string `json:"title"`
		ReleaseDate string `json:"releaseDate"`
		MediaInfo   *struct {
			Status int `json:"status"`
		} `json:"mediaInfo"`
	}
	if _, err := s.do("GET", fmt.Sprintf("/api/v1/movie/%d", tmdb), nil, &m); err != nil {
		return false, "", 0, err
	}
	year := 0
	if len(m.ReleaseDate) >= 4 {
		fmt.Sscan(m.ReleaseDate[:4], &year)
	}
	return m.MediaInfo != nil && m.MediaInfo.Status == 5, m.Title, year, nil // 5 = AVAILABLE
}

func (s *Seerr) Unblock(k mediaKey) error {
	_, err := s.do("DELETE", fmt.Sprintf("/api/v1/blocklist/%d?mediaType=%s", k.TMDB, k.Type), nil, nil)
	return err
}

// SimklSync applies the Simkl lists to Seerr's blocklist and Radarr (see the comment at the top). It returns false
// if the sync couldn't run (e.g. a service still starting), so the caller retries soon instead of a full interval later.
func (a *App) SimklSync() bool {
	if a.Simkl == nil || a.Seerr == nil {
		return true
	}
	all, err := a.Simkl.AllItems()
	if err != nil {
		log.Printf("Simkl: %v", err)
		return false
	}
	hide := map[mediaKey]string{}
	want := map[mediaKey]bool{}
	completedMovies := map[int]time.Time{} // tmdb -> when it was watched (zero if unknown)
	for _, it := range all.Movies {
		if it.Movie == nil {
			continue
		}
		id, err := it.Movie.IDs.TMDB.Int64()
		if err != nil || id == 0 {
			continue
		}
		k := mediaKey{"movie", int(id)}
		switch {
		case hideMovies[it.Status]:
			hide[k] = it.Movie.Title
			if it.Status == "completed" {
				watched, _ := time.Parse(time.RFC3339, it.LastWatchedAt)
				completedMovies[int(id)] = watched
			}
		case wantMovies[it.Status]:
			want[k] = true
		}
	}
	for _, it := range append(all.Shows, all.Anime...) {
		if it.Show == nil || !hideShows[it.Status] {
			continue
		}
		if id, err := it.Show.IDs.TMDB.Int64(); err == nil && id != 0 {
			hide[mediaKey{"tv", int(id)}] = it.Show.Title
		}
	}

	managed, radarrMovies, err := a.managedMedia()
	if err != nil {
		log.Printf("Simkl: %v (retrying soon)", err)
		return false
	}
	current, err := a.Seerr.Blocklist()
	if err != nil {
		log.Printf("Simkl: %v (retrying soon)", err)
		return false
	}

	added, removed, unmonitored, excluded := 0, 0, 0, 0
	for k, title := range hide {
		if current[k] || managed[k] {
			continue
		}
		if a.Config.DryRun {
			log.Printf("[dry run] Simkl: would blocklist %s %q", k.Type, title)
			continue
		}
		if err := a.Seerr.Block(k, title); err != nil {
			log.Printf("Simkl: blocklist %q: %v", title, err)
			continue
		}
		added++
	}
	for k := range want {
		if !current[k] {
			continue
		}
		if a.Config.DryRun {
			log.Printf("[dry run] Simkl: would take movie %d off the blocklist (wanted again)", k.TMDB)
			continue
		}
		if err := a.Seerr.Unblock(k); err != nil {
			log.Printf("Simkl: unblock movie %d: %v", k.TMDB, err)
			continue
		}
		removed++
	}
	// collection rule: wanted movies that are already in the library
	haveCopy := map[int]bool{}
	if a.Radarr != nil {
		var excl []map[string]any
		if err := a.Radarr.do("GET", "/api/v3/exclusions", nil, &excl); err != nil {
			log.Printf("Simkl: radarr exclusions: %v", err)
		}
		isExcluded := map[int]bool{}
		for _, e := range excl {
			if id := toInts([]any{e["tmdbId"]}); len(id) == 1 {
				isExcluded[id[0]] = true
			}
		}
		for k := range want {
			avail, title, year, err := a.Seerr.MovieAvailable(k.TMDB)
			if err != nil || !avail {
				continue
			}
			haveCopy[k.TMDB] = true
			if isExcluded[k.TMDB] {
				continue
			}
			if a.Config.DryRun {
				log.Printf("[dry run] Simkl: would exclude %q in Radarr (already in the collection)", title)
				continue
			}
			if err := a.Radarr.do("POST", "/api/v3/exclusions", map[string]any{"tmdbId": k.TMDB, "movieTitle": title, "movieYear": year}, nil); err != nil {
				log.Printf("Simkl: exclude %q: %v", title, err)
				continue
			}
			log.Printf("Simkl: excluded %q in Radarr (on the watchlist, but already in the collection)", title)
			excluded++
		}
	}
	for _, m := range radarrMovies {
		tmdb := toInts([]any{m["tmdbId"]})
		if len(tmdb) == 0 || m["monitored"] != true || m["hasFile"] == true {
			continue
		}
		reason := ""
		switch {
		case seenInCinema(completedMovies, tmdb[0], m):
			reason = "completed in Simkl before its home release, seen in the cinema"
		case haveCopy[tmdb[0]]:
			reason = "already in the collection"
		default:
			continue
		}
		title, _ := m["title"].(string)
		if a.Config.DryRun {
			log.Printf("[dry run] Simkl: would unmonitor %q in Radarr (%s)", title, reason)
			continue
		}
		m["monitored"] = false
		id := toInts([]any{m["id"]})[0]
		if err := a.Radarr.do("PUT", fmt.Sprintf("/api/v3/movie/%d", id), m, nil); err != nil {
			log.Printf("Simkl: unmonitor %q: %v", title, err)
			continue
		}
		log.Printf("Simkl: unmonitored %q in Radarr (%s)", title, reason)
		unmonitored++
	}
	if added+removed+unmonitored+excluded > 0 || a.Config.Debug {
		log.Printf("Simkl: %d titles to hide; blocklisted %d, unblocked %d, excluded %d, unmonitored %d", len(hide), added, removed, excluded, unmonitored)
	}
	return true
}

// managedMedia returns the titles Sonarr/Radarr manage and Radarr's movies (raw, for updates).
func (a *App) managedMedia() (map[mediaKey]bool, []map[string]any, error) {
	managed := map[mediaKey]bool{}
	var movies []map[string]any
	if a.Sonarr != nil {
		var series []map[string]any
		if err := a.Sonarr.do("GET", "/api/v3/series", nil, &series); err != nil {
			return nil, nil, err
		}
		for _, s := range series {
			if id := toInts([]any{s["tmdbId"]}); len(id) == 1 && id[0] != 0 {
				managed[mediaKey{"tv", id[0]}] = true
			}
		}
	}
	if a.Radarr != nil {
		if err := a.Radarr.do("GET", "/api/v3/movie", nil, &movies); err != nil {
			return nil, nil, err
		}
		for _, m := range movies {
			if id := toInts([]any{m["tmdbId"]}); len(id) == 1 && id[0] != 0 {
				managed[mediaKey{"movie", id[0]}] = true
			}
		}
	}
	return managed, movies, nil
}

// seenInCinema reports whether a Simkl-completed movie was watched before its home release (Radarr's earliest digital
// or physical release date). Without a known watch date it counts only if there is no home release yet.
func seenInCinema(completed map[int]time.Time, tmdb int, radarrMovie map[string]any) bool {
	watched, ok := completed[tmdb]
	if !ok {
		return false
	}
	var home time.Time
	for _, f := range []string{"digitalRelease", "physicalRelease"} {
		if v, _ := radarrMovie[f].(string); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil && (home.IsZero() || t.Before(home)) {
				home = t
			}
		}
	}
	if home.IsZero() {
		return true // no home release yet: it can only have been the cinema
	}
	return !watched.IsZero() && watched.Before(home)
}
