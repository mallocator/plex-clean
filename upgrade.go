package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Upgrade search: Sonarr and Radarr only see new releases through RSS, and the sources with the best releases
// (aggregators like Knaben) have none. So curator searches for new episodes and movies itself, on a schedule that
// follows release patterns, and uses what it knows that the apps don't (what was watched, what is playing):
//   - candidates: episodes aired and movies added/available within UPGRADE_WINDOW that are missing or whose file is
//     below the profile's cutoff (format score or quality), plus older missing ones (searched daily);
//   - searched every 30 min for the first 6 h, hourly until 2 days, every 6 h until UPGRADE_WINDOW;
//   - the app decides: curator grabs the first approved release of an interactive search (the app's own order);
//   - fallback: with no file and nothing approved after UPGRADE_FALLBACK, the best release rejected only for the size
//     floor is grabbed, else the best 720p one. Upgrades continue after that until the window ends;
//   - watched items (grace queue, Jellyfin played marks) and files being played are skipped; at most
//     UPGRADE_MAX_SEARCHES searches per run;
//   - groups whose imports measured starved (imports.go) are passed over when the app approved another release.

type arrRelease struct {
	GUID              string   `json:"guid"`
	IndexerID         int      `json:"indexerId"`
	Indexer           string   `json:"indexer"`
	Title             string   `json:"title"`
	Size              int64    `json:"size"`
	Approved          bool     `json:"approved"`
	Rejections        []string `json:"rejections"`
	CustomFormatScore int      `json:"customFormatScore"`
	Seeders           int      `json:"seeders"`
	ReleaseGroup      string   `json:"releaseGroup"`
	Quality           struct {
		Quality struct {
			Name       string `json:"name"`
			Resolution int    `json:"resolution"`
		} `json:"quality"`
	} `json:"quality"`
}

// searchClient: interactive searches wait for every indexer (FlareSolverr challenges take tens of seconds).
var searchClient = &http.Client{Timeout: 4 * time.Minute}

// Releases runs an interactive search ("episodeId=1" or "movieId=2"). The app returns its preferred release first.
func (a *Arr) Releases(query string) ([]arrRelease, error) {
	c := *a
	c.Client = searchClient
	var rs []arrRelease
	err := c.do("GET", "/api/v3/release?"+query, nil, &rs)
	return rs, err
}

// Grab sends a release from the last search to the download client, whatever the app's verdict on it.
func (a *Arr) Grab(r arrRelease) error {
	c := *a
	c.Client = searchClient
	return c.do("POST", "/api/v3/release", map[string]any{"guid": r.GUID, "indexerId": r.IndexerID}, nil)
}

// cutoffScores returns each quality profile's cutoff format score.
func (a *Arr) cutoffScores() (map[int]int, error) {
	var ps []struct {
		ID                int `json:"id"`
		CutoffFormatScore int `json:"cutoffFormatScore"`
	}
	if err := a.do("GET", "/api/v3/qualityprofile", nil, &ps); err != nil {
		return nil, err
	}
	m := map[int]int{}
	for _, p := range ps {
		m[p.ID] = p.CutoffFormatScore
	}
	return m, nil
}

func sizeFloor(m string) bool { return strings.Contains(m, "is smaller than minimum allowed") }
func notWanted(m string) bool { return strings.Contains(m, "is not wanted in profile") }

// fallbackRelease picks what to take when nothing is acceptable: the best release whose only problem is the size
// floor, else the best 720p one rejected only for its quality (and size). Best = format score, then size.
func fallbackRelease(rs []arrRelease) (arrRelease, string, bool) {
	tiers := []struct {
		reason string
		ok     func(r arrRelease, m string) bool
	}{
		{"under the size floor", func(r arrRelease, m string) bool { return sizeFloor(m) }},
		{"720p", func(r arrRelease, m string) bool {
			return r.Quality.Quality.Resolution == 720 && (sizeFloor(m) || notWanted(m))
		}},
	}
	for _, tier := range tiers {
		var cands []arrRelease
		for _, r := range rs {
			if r.Approved || len(r.Rejections) == 0 || (tier.reason == "720p" && r.Quality.Quality.Resolution != 720) {
				continue
			}
			ok := true
			for _, m := range r.Rejections {
				ok = ok && tier.ok(r, m)
			}
			if ok {
				cands = append(cands, r)
			}
		}
		if len(cands) > 0 {
			sort.SliceStable(cands, func(i, j int) bool {
				if cands[i].CustomFormatScore != cands[j].CustomFormatScore {
					return cands[i].CustomFormatScore > cands[j].CustomFormatScore
				}
				return cands[i].Size > cands[j].Size
			})
			return cands[0], tier.reason, true
		}
	}
	return arrRelease{}, "", false
}

// upgradeItem is an episode or movie the upgrade search looks after.
type upgradeItem struct {
	Key     string // "sonarr:<episode id>", "radarr:<movie id>"
	App     string
	Query   string // interactive search query
	Name    string
	Since   time.Time // aired, or added/available
	HasFile bool
	File    string // base name of the current file
	Score   int    // the current file's format score
	Watched bool
}

func (it upgradeItem) state() string {
	if !it.HasFile {
		return "missing"
	}
	return fmt.Sprintf("score %d", it.Score)
}

// searchEvery follows release patterns: WEB-DLs within hours, repacks and good encodes within two days.
func searchEvery(age, window time.Duration) time.Duration {
	switch {
	case age < 6*time.Hour:
		return 30 * time.Minute
	case age < 48*time.Hour:
		return time.Hour
	case age < window:
		return 6 * time.Hour
	}
	return 24 * time.Hour
}

type upgradeRecord struct {
	FirstSeen  time.Time `json:"first_seen"`
	LastSearch time.Time `json:"last_search,omitzero"`
	Searches   int       `json:"searches,omitempty"`
}

// upgradeLog persists when each item was first seen and last searched (UPGRADE_STATE_FILE).
type upgradeLog struct {
	mu    sync.Mutex
	path  string
	items map[string]upgradeRecord
}

func loadUpgradeLog(path string) *upgradeLog {
	l := &upgradeLog{path: path, items: map[string]upgradeRecord{}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &l.items); err != nil {
			log.Printf("Upgrades: reading %s: %v", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("Upgrades: reading %s: %v", path, err)
	}
	return l
}

func (l *upgradeLog) save() {
	if l.path == "" {
		return
	}
	data, err := json.MarshalIndent(l.items, "", " ")
	if err == nil {
		tmp := l.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, l.path)
		}
	}
	if err != nil {
		log.Printf("Upgrades: saving %s: %v", l.path, err)
	}
}

func (a *App) upgradeLog() *upgradeLog {
	a.upgradesOnce.Do(func() { a.upgrades = loadUpgradeLog(a.Config.UpgradeStateFile) })
	return a.upgrades
}

// UpgradeSearch runs one round of the upgrade search over Sonarr and Radarr.
func (a *App) UpgradeSearch() {
	now := a.now()
	playing := a.playingFiles()
	var items []upgradeItem
	apps := map[string]*Arr{}
	if a.Sonarr != nil {
		apps["Sonarr"] = a.Sonarr.arr()
		eps, err := a.sonarrUpgradeItems(now)
		if err != nil {
			log.Printf("Upgrades (Sonarr): %v", err)
		}
		items = append(items, eps...)
	}
	if a.Radarr != nil {
		apps["Radarr"] = a.Radarr
		mvs, err := a.radarrUpgradeItems(now)
		if err != nil {
			log.Printf("Upgrades (Radarr): %v", err)
		}
		items = append(items, mvs...)
	}
	queued := map[string]bool{}
	for name, app := range apps {
		q, err := app.Queue()
		if err != nil {
			log.Printf("Upgrades (%s): %v", name, err)
			return // without the queue, a search could grab a second copy of something downloading
		}
		for _, it := range q {
			if it.EpisodeID > 0 {
				queued[fmt.Sprintf("sonarr:%d", it.EpisodeID)] = true
			}
			if it.MovieID > 0 && name == "Radarr" {
				queued[fmt.Sprintf("radarr:%d", it.MovieID)] = true
			}
		}
	}

	ul := a.upgradeLog()
	ul.mu.Lock()
	defer ul.mu.Unlock()
	keep := map[string]bool{}
	var due []upgradeItem
	for _, it := range items {
		keep[it.Key] = true
		rec, ok := ul.items[it.Key]
		if !ok {
			rec = upgradeRecord{FirstSeen: now}
			ul.items[it.Key] = rec
		}
		switch {
		case it.Watched, queued[it.Key], it.File != "" && playing[strings.ToLower(it.File)]:
			continue
		case !rec.LastSearch.IsZero() && now.Sub(rec.LastSearch) < searchEvery(now.Sub(it.Since), a.Config.UpgradeWindow):
			continue
		}
		due = append(due, it)
	}
	for k := range ul.items {
		if !keep[k] {
			delete(ul.items, k) // done (good file, watched and deleted, or out of the window)
		}
	}
	// newest first: they are what's about to be watched, and their releases change fastest
	sort.SliceStable(due, func(i, j int) bool { return due[i].Since.After(due[j].Since) })
	metrics.SetGroup("upgrades", map[string]float64{
		series("curator_upgrade_candidates", "state", "tracked"): float64(len(items)),
		series("curator_upgrade_candidates", "state", "due"):     float64(len(due)),
	})
	a.debugf("Upgrades: %d tracked, %d due", len(items), len(due))
	for i, it := range due {
		if i >= a.Config.UpgradeMaxSearches {
			break
		}
		rec := ul.items[it.Key]
		rec.LastSearch, rec.Searches = now, rec.Searches+1
		ul.items[it.Key] = rec
		fallback := !it.HasFile && now.Sub(later(it.Since, rec.FirstSeen)) >= a.Config.UpgradeFallback
		a.upgradeOne(apps[it.App], it, fallback)
	}
	ul.save()
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// upgradeOne searches for one item and grabs what the app approves, or the fallback.
func (a *App) upgradeOne(app *Arr, it upgradeItem, fallback bool) {
	metrics.Inc("curator_upgrade_searches_total", "app", strings.ToLower(it.App))
	rs, err := app.Releases(it.Query)
	if err != nil {
		log.Printf("Upgrades (%s): searching %s: %v", it.App, it.Name, err)
		return
	}
	var pick arrRelease
	reason := ""
	approved := 0
	for _, r := range rs {
		if r.Approved {
			approved++
		}
	}
	g := a.groupStats()
	for _, r := range rs {
		if !r.Approved {
			continue
		}
		if approved > 1 && g.starved(r.ReleaseGroup, a.Config.ImportMinMBPerMin) {
			a.debugf("Upgrades (%s): passing over %s for %s: %s's files measured under %.0f MB/min", it.App, r.Title, it.Name,
				r.ReleaseGroup, a.Config.ImportMinMBPerMin)
			approved--
			continue
		}
		pick, reason = r, "upgrade"
		if !it.HasFile {
			reason = "new"
		}
		break
	}
	if reason == "" && fallback {
		if r, why, ok := fallbackRelease(rs); ok {
			pick, reason = r, "fallback"
			log.Printf("Upgrades (%s): nothing acceptable for %s after %s; falling back to a release %s", it.App, it.Name,
				a.Config.UpgradeFallback, why)
		}
	}
	if reason == "" {
		a.debugf("Upgrades (%s): %s (%s): nothing better among %d releases", it.App, it.Name, it.state(), len(rs))
		return
	}
	if a.Config.DryRun {
		log.Printf("[dry run] Upgrades (%s): would grab %s for %s (%s, score %d)", it.App, pick.Title, it.Name, it.state(), pick.CustomFormatScore)
		return
	}
	if err := app.Grab(pick); err != nil {
		log.Printf("Upgrades (%s): grabbing %s for %s: %v", it.App, pick.Title, it.Name, err)
		return
	}
	log.Printf("Upgrades (%s): grabbed %s for %s (%s -> score %d, %s, %d MB, %s)", it.App, pick.Title, it.Name, it.state(),
		pick.CustomFormatScore, pick.Quality.Quality.Name, pick.Size>>20, pick.Indexer)
	metrics.Inc("curator_actions_total", "action", "upgrade_grabbed", "reason", reason)
}

type sonarrCalendarEpisode struct {
	ID            int    `json:"id"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	AirDateUtc    string `json:"airDateUtc"`
	Monitored     bool   `json:"monitored"`
	HasFile       bool   `json:"hasFile"`
	Series        struct {
		Title            string `json:"title"`
		QualityProfileID int    `json:"qualityProfileId"`
		Monitored        bool   `json:"monitored"`
	} `json:"series"`
	EpisodeFile *struct {
		Path                string `json:"path"`
		CustomFormatScore   int    `json:"customFormatScore"`
		QualityCutoffNotMet bool   `json:"qualityCutoffNotMet"`
	} `json:"episodeFile"`
}

// sonarrUpgradeItems: monitored episodes aired within the window that are missing or below cutoff, and older
// missing ones.
func (a *App) sonarrUpgradeItems(now time.Time) ([]upgradeItem, error) {
	cutoffs, err := a.Sonarr.cutoffScores()
	if err != nil {
		return nil, err
	}
	q := url.Values{"start": {now.Add(-a.Config.UpgradeWindow).UTC().Format(time.RFC3339)}, "end": {now.UTC().Format(time.RFC3339)},
		"unmonitored": {"false"}, "includeSeries": {"true"}, "includeEpisodeFile": {"true"}}
	var eps []sonarrCalendarEpisode
	if err := a.Sonarr.do("GET", "/api/v3/calendar?"+q.Encode(), nil, &eps); err != nil {
		return nil, err
	}
	var missing struct {
		Records []sonarrCalendarEpisode `json:"records"`
	}
	if err := a.Sonarr.do("GET", "/api/v3/wanted/missing?pageSize=200&monitored=true&includeSeries=true&sortKey=airDateUtc&sortDirection=descending", nil, &missing); err != nil {
		return nil, err
	}
	watched := map[string]bool{} // the grace queue, plus anything any Jellyfin user marked played (WatchState mirrors Plex)
	for _, p := range a.Queue.Items() {
		watched[p.Key()] = true
	}
	if a.Jellyfin != nil {
		played, err := a.Jellyfin.PlayedEpisodeKeys()
		if err != nil {
			return nil, err // without it, a watched and deleted episode could be fetched again
		}
		for k := range played {
			watched[k] = true
		}
	}
	seen := map[int]bool{}
	var items []upgradeItem
	for _, e := range append(eps, missing.Records...) {
		aired, err := time.Parse(time.RFC3339, e.AirDateUtc)
		if seen[e.ID] || err != nil || aired.After(now) || !e.Monitored {
			continue
		}
		seen[e.ID] = true
		it := upgradeItem{Key: fmt.Sprintf("sonarr:%d", e.ID), App: "Sonarr", Query: fmt.Sprintf("episodeId=%d", e.ID),
			Name: fmt.Sprintf("%s S%02dE%02d", e.Series.Title, e.SeasonNumber, e.EpisodeNumber), Since: aired}
		if e.HasFile && e.EpisodeFile != nil {
			if !e.EpisodeFile.QualityCutoffNotMet && e.EpisodeFile.CustomFormatScore >= cutoffs[e.Series.QualityProfileID] {
				continue
			}
			if now.Sub(aired) > a.Config.UpgradeWindow {
				continue // old files are upgraded by the app's RSS sync only
			}
			it.HasFile, it.Score, it.File = true, e.EpisodeFile.CustomFormatScore, filepath.Base(e.EpisodeFile.Path)
		} else if e.HasFile {
			continue
		}
		it.Watched = watched[PendingItem{Series: e.Series.Title, Season: e.SeasonNumber, Episode: e.EpisodeNumber}.Key()]
		items = append(items, it)
	}
	return items, nil
}

type radarrUpgradeMovie struct {
	ID               int    `json:"id"`
	TmdbID           int    `json:"tmdbId"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	Monitored        bool   `json:"monitored"`
	IsAvailable      bool   `json:"isAvailable"`
	HasFile          bool   `json:"hasFile"`
	Added            string `json:"added"`
	DigitalRelease   string `json:"digitalRelease"`
	PhysicalRelease  string `json:"physicalRelease"`
	QualityProfileID int    `json:"qualityProfileId"`
}

type radarrMovieFile struct {
	Path                string `json:"path"`
	CustomFormatScore   int    `json:"customFormatScore"`
	QualityCutoffNotMet bool   `json:"qualityCutoffNotMet"`
}

// radarrUpgradeItems: monitored, available movies that are missing, or that became available (or were added)
// within the window and are below cutoff.
func (a *App) radarrUpgradeItems(now time.Time) ([]upgradeItem, error) {
	cutoffs, err := a.Radarr.cutoffScores()
	if err != nil {
		return nil, err
	}
	var movies []radarrUpgradeMovie
	if err := a.Radarr.do("GET", "/api/v3/movie", nil, &movies); err != nil {
		return nil, err
	}
	var watched map[int]bool
	if a.Jellyfin != nil {
		if watched, err = a.Jellyfin.PlayedMovies(); err != nil {
			log.Printf("Upgrades (Radarr): %v", err) // without it, watched movies may be upgraded too
		}
	}
	var items []upgradeItem
	for _, m := range movies {
		if !m.Monitored || !m.IsAvailable {
			continue
		}
		since := time.Time{}
		for _, s := range []string{m.Added, m.DigitalRelease, m.PhysicalRelease} {
			if t, err := time.Parse(time.RFC3339, s); err == nil && !t.After(now) {
				since = later(since, t)
			}
		}
		if since.IsZero() {
			continue
		}
		it := upgradeItem{Key: fmt.Sprintf("radarr:%d", m.ID), App: "Radarr", Query: fmt.Sprintf("movieId=%d", m.ID),
			Name: fmt.Sprintf("%s (%d)", m.Title, m.Year), Since: since, Watched: watched[m.TmdbID]}
		if m.HasFile {
			if now.Sub(since) > a.Config.UpgradeWindow {
				continue
			}
			var files []radarrMovieFile
			if err := a.Radarr.do("GET", fmt.Sprintf("/api/v3/moviefile?movieId=%d", m.ID), nil, &files); err != nil || len(files) == 0 {
				continue
			}
			f := files[0]
			if !f.QualityCutoffNotMet && f.CustomFormatScore >= cutoffs[m.QualityProfileID] {
				continue
			}
			it.HasFile, it.Score, it.File = true, f.CustomFormatScore, filepath.Base(f.Path)
		}
		items = append(items, it)
	}
	return items, nil
}

// PlayedEpisodeKeys returns the episodes any Jellyfin user has played, as queue keys (show|season|episode).
func (j *Jellyfin) PlayedEpisodeKeys() (map[string]bool, error) {
	var users []struct {
		ID string `json:"Id"`
	}
	if err := j.get("/Users", &users); err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, u := range users {
		var items struct {
			Items []struct {
				SeriesName  string `json:"SeriesName"`
				ParentIndex int    `json:"ParentIndexNumber"`
				Index       int    `json:"IndexNumber"`
			} `json:"Items"`
		}
		q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Episode"}, "IsPlayed": {"true"}}
		if err := j.get("/Users/"+u.ID+"/Items?"+q.Encode(), &items); err != nil {
			return nil, err
		}
		for _, it := range items.Items {
			keys[PendingItem{Series: it.SeriesName, Season: it.ParentIndex, Episode: it.Index}.Key()] = true
		}
	}
	return keys, nil
}

// PlayedMovies returns the TMDB IDs of movies any Jellyfin user has played.
func (j *Jellyfin) PlayedMovies() (map[int]bool, error) {
	var users []struct {
		ID string `json:"Id"`
	}
	if err := j.get("/Users", &users); err != nil {
		return nil, err
	}
	ids := map[int]bool{}
	for _, u := range users {
		var items struct {
			Items []struct {
				ProviderIds map[string]string `json:"ProviderIds"`
			} `json:"Items"`
		}
		q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie"}, "IsPlayed": {"true"}, "Fields": {"ProviderIds"}}
		if err := j.get("/Users/"+u.ID+"/Items?"+q.Encode(), &items); err != nil {
			return nil, err
		}
		for _, it := range items.Items {
			for k, v := range it.ProviderIds {
				var id int
				if strings.EqualFold(k, "tmdb") {
					if _, err := fmt.Sscan(v, &id); err == nil {
						ids[id] = true
					}
				}
			}
		}
	}
	return ids, nil
}

// playingFiles returns the base names (lower case) of the files Jellyfin and Plex are playing right now.
func (a *App) playingFiles() map[string]bool {
	files := map[string]bool{}
	if a.Jellyfin != nil {
		var sessions []struct {
			NowPlaying *struct {
				Path string `json:"Path"`
			} `json:"NowPlayingItem"`
		}
		if err := a.Jellyfin.get("/Sessions?activeWithinSeconds=300", &sessions); err != nil {
			log.Printf("Upgrades: %v", err)
		}
		for _, s := range sessions {
			if s.NowPlaying != nil && s.NowPlaying.Path != "" {
				files[strings.ToLower(filepath.Base(s.NowPlaying.Path))] = true
			}
		}
	}
	if a.Plex != nil {
		var sessions struct {
			MediaContainer struct {
				Metadata []struct {
					Media []struct {
						Part []struct {
							File string `json:"file"`
						} `json:"Part"`
					} `json:"Media"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := a.Plex.get("/status/sessions", &sessions); err != nil {
			log.Printf("Upgrades: %v", err)
		}
		for _, m := range sessions.MediaContainer.Metadata {
			for _, md := range m.Media {
				for _, p := range md.Part {
					files[strings.ToLower(filepath.Base(p.File))] = true
				}
			}
		}
	}
	return files
}
