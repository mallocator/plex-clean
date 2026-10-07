package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// App wires the webhook server, the pending queue and the external services.
type App struct {
	Config Config
	Queue  *Queue
	Sonarr *Sonarr
	Qbt    *Qbittorrent
	Radarr *Arr
	Simkl  *Simkl
	Seerr  *Seerr
	Plex   *Plex
	Now    func() time.Time // for tests
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// WatchEvent is a finished viewing reported by Plex or Jellyfin.
type WatchEvent struct {
	Source  string // "plex" or "jellyfin"
	Type    string // "episode" or "movie"
	Series  string // episodes only
	Season  int
	Episode int
	Title   string // episode or movie title
}

// PlexWebhookPayload holds the fields of a Plex webhook we use.
type PlexWebhookPayload struct {
	Event    string `json:"event"`
	Metadata struct {
		Type             string `json:"type"`
		Title            string `json:"title"`
		GrandparentTitle string `json:"grandparentTitle"`
		ParentIndex      int    `json:"parentIndex"`
		Index            int    `json:"index"`
	} `json:"Metadata"`
}

// JellyfinWebhookPayload holds the fields of the Jellyfin Webhook plugin's default template we use.
type JellyfinWebhookPayload struct {
	Event       string `json:"Event"`
	ItemType    string `json:"ItemType"`
	MediaStatus struct {
		PlayedToCompletion bool `json:"PlayedToCompletion"`
	} `json:"MediaStatus"`
	PlayedToCompletion bool   `json:"PlayedToCompletion"` // top level in the plugin's default template
	NotificationType   string `json:"NotificationType"`
	Title              string `json:"Name"`
	SeriesName         string `json:"SeriesName"`
	SeasonNumber       int    `json:"SeasonNumber"`
	EpisodeNumber      int    `json:"EpisodeNumber"`
}

// MarkerData is the marker file format (kept compatible with the Tautulli-based version).
type MarkerData struct {
	FullTitle        string  `json:"full_title"`
	ParentMediaIndex int     `json:"parent_media_index"`
	MediaIndex       int     `json:"media_index"`
	WatchedStatus    float64 `json:"watched_status"`
	PercentComplete  int     `json:"percent_complete"`
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/plex", a.handlePlex)
	mux.HandleFunc("/jellyfin", a.handleJellyfin)
	// Sonarr/Radarr "Connect" webhooks (On Series/Movie Add): route new items right away.
	arrHook := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		go a.Route()
		okResponse(w)
	}
	mux.HandleFunc("/sonarr", arrHook)
	mux.HandleFunc("/radarr", arrHook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/pending", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.Queue.Items())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// Older setups post both kinds to "/": tell them apart by content type.
		ct := r.Header.Get("Content-Type")
		switch {
		case strings.Contains(ct, "multipart/form-data"):
			a.handlePlex(w, r)
		case strings.Contains(ct, "application/json"):
			a.handleJellyfin(w, r)
		default:
			http.Error(w, "Unable to determine webhook type", http.StatusBadRequest)
		}
	})
	return mux
}

func (a *App) handlePlex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}
	var p PlexWebhookPayload
	if err := json.Unmarshal([]byte(r.FormValue("payload")), &p); err != nil {
		http.Error(w, "Error parsing payload", http.StatusBadRequest)
		return
	}
	// media.scrobble is Plex's "watched" event (sent once when playback passes the watched threshold).
	if p.Event != "media.scrobble" {
		a.debugf("Ignoring Plex event %s", p.Event)
		okResponse(w)
		return
	}
	switch {
	case p.Metadata.Type == "episode":
		a.Watched(WatchEvent{Source: "plex", Type: "episode", Series: p.Metadata.GrandparentTitle,
			Season: p.Metadata.ParentIndex, Episode: p.Metadata.Index, Title: p.Metadata.Title})
	case p.Metadata.Type == "movie":
		a.Watched(movieOrEpisode("plex", p.Metadata.Title))
	default:
		log.Printf("Ignoring Plex scrobble of %s (type %s)", p.Metadata.Title, p.Metadata.Type)
	}
	okResponse(w)
}

// movieOrEpisode turns a watched "movie" into an episode when its title is a release name ("Show S17E01 ..."):
// loose episode files in a download folder show up as movies in Jellyfin's mixed libraries and Plex movie libraries.
func movieOrEpisode(source, title string) WatchEvent {
	if show, season, episode, ok := parseRelease(title); ok && show != "" {
		return WatchEvent{Source: source, Type: "episode", Series: releaseShowName(title), Season: season, Episode: episode, Title: title}
	}
	return WatchEvent{Source: source, Type: "movie", Title: title}
}

func (a *App) handleJellyfin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	var p JellyfinWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "Error parsing payload", http.StatusBadRequest)
		return
	}
	if p.Event != "playback.stop" && p.NotificationType != "PlaybackStop" {
		a.debugf("Ignoring Jellyfin event %s/%s", p.Event, p.NotificationType)
		okResponse(w)
		return
	}
	if !p.PlayedToCompletion && !p.MediaStatus.PlayedToCompletion {
		log.Printf("Jellyfin: playback of %s stopped before the end", p.Title)
		okResponse(w)
		return
	}
	switch {
	case p.ItemType == "Episode" && p.SeriesName != "":
		a.Watched(WatchEvent{Source: "jellyfin", Type: "episode", Series: p.SeriesName,
			Season: p.SeasonNumber, Episode: p.EpisodeNumber, Title: p.Title})
	case p.ItemType == "Movie" || p.ItemType == "Video" || p.ItemType == "Episode":
		a.Watched(movieOrEpisode("jellyfin", p.Title))
	default:
		log.Printf("Jellyfin: ignoring %s (item type %s)", p.Title, p.ItemType)
	}
	okResponse(w)
}

// Watched records a finished viewing: marker file, and episodes go into the grace-period queue.
func (a *App) Watched(e WatchEvent) {
	a.writeMarker(e)
	if e.Type == "episode" {
		if a.Queue.Add(e, a.now()) {
			log.Printf("Queued %s S%02dE%02d (watched in %s), due in %s", e.Series, e.Season, e.Episode, e.Source, a.Config.GracePeriod)
		}
	}
}

func (a *App) writeMarker(e WatchEvent) {
	if a.Config.OutputDir == "" {
		if e.Type == "episode" {
			log.Printf("Watched in %s: %s S%02dE%02d", e.Source, e.Series, e.Season, e.Episode)
		} else {
			log.Printf("Watched in %s: %s", e.Source, e.Title)
		}
		return
	}
	var name string
	m := MarkerData{WatchedStatus: 1, PercentComplete: 100}
	if e.Type == "episode" {
		m.FullTitle = e.Series + " - " + e.Title
		m.ParentMediaIndex, m.MediaIndex = e.Season, e.Episode
		name = fmt.Sprintf("%s - S%dE%d.json", m.FullTitle, e.Season, e.Episode)
	} else {
		m.FullTitle = e.Title
		name = e.Title + ".json"
	}
	name = strings.ReplaceAll(name, string(os.PathSeparator), "-")
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(a.Config.OutputDir, 0755); err != nil {
		log.Printf("Error creating output directory: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(a.Config.OutputDir, name), data, 0644); err != nil {
		log.Printf("Error writing marker %s: %v", name, err)
		return
	}
	log.Printf("Watched in %s: %s", e.Source, name)
}

func (a *App) debugf(format string, args ...any) {
	if a.Config.Debug {
		log.Printf(format, args...)
	}
}

func okResponse(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
