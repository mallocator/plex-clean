package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Import checks: what actually arrived, measured on the file instead of the release name.
//   - every file imported within UPGRADE_WINDOW is measured (MB per minute of runtime) and exported as a metric;
//   - a file whose picture is smaller than its quality claims (a "1080p" release that is 720p) is mislabeled: its
//     grab is marked failed in the app, which blocklists the release and searches again (the file stays until the
//     replacement arrives). Once per file;
//   - each release group's measured MB/min is kept (last 20 files). The upgrade search passes over approved releases
//     of groups that reliably deliver less than IMPORT_MIN_MB_PER_MIN when the app approved something else too, which
//     catches re-encoders the format lists don't know yet.

type importFile struct {
	Key        string // "sonarr:<file id>", "radarr:<file id>"
	App        string
	Name       string
	Group      string
	Size       int64
	Added      time.Time
	Resolution int // the quality's claimed vertical resolution
	MediaInfo  struct {
		RunTime    string `json:"runTime"`
		Resolution string `json:"resolution"` // "1920x1080"
	}
	HistoryQuery string // Sonarr "history?episodeId=1&pageSize=50" (paged), Radarr "history/movie?movieId=2" (a list)
}

// minutes parses Sonarr/Radarr runtimes ("1:04:32", "44:10").
func minutes(rt string) float64 {
	parts := strings.Split(rt, ":")
	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		total = total*60 + v
	}
	if len(parts) == 3 || len(parts) == 2 {
		return total / 60 // seconds -> minutes
	}
	return 0
}

func (f importFile) mbPerMin() float64 {
	m := minutes(f.MediaInfo.RunTime)
	if m < 5 {
		return 0
	}
	return float64(f.Size) / (1 << 20) / m
}

// mislabeled: the picture is much smaller than the quality's resolution in both dimensions (wide and 4:3 crops
// keep one full dimension).
func (f importFile) mislabeled() bool {
	w, h, ok := strings.Cut(f.MediaInfo.Resolution, "x")
	width, err1 := strconv.Atoi(w)
	height, err2 := strconv.Atoi(h)
	if !ok || err1 != nil || err2 != nil || width == 0 || f.Resolution == 0 {
		return false
	}
	switch f.Resolution {
	case 1080:
		return width < 1600 && height < 900
	case 2160:
		return width < 3000 && height < 1600
	}
	return false
}

type groupStats struct {
	mu      sync.Mutex
	path    string
	Groups  map[string][]float64 `json:"groups"` // lower-case group -> MB/min of its last files
	Files   map[string]int64     `json:"files"`  // measured files (unix time first seen), so each counts once
	Flagged map[string]int64     `json:"flagged"`
}

func loadGroupStats(path string) *groupStats {
	g := &groupStats{path: path, Groups: map[string][]float64{}, Files: map[string]int64{}, Flagged: map[string]int64{}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, g); err != nil {
			log.Printf("Imports: reading %s: %v", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("Imports: reading %s: %v", path, err)
	}
	for _, m := range []*map[string]int64{&g.Files, &g.Flagged} {
		if *m == nil {
			*m = map[string]int64{}
		}
	}
	if g.Groups == nil {
		g.Groups = map[string][]float64{}
	}
	return g
}

func (g *groupStats) saveLocked() {
	if g.path == "" {
		return
	}
	data, err := json.MarshalIndent(g, "", " ")
	if err == nil {
		tmp := g.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, g.path)
		}
	}
	if err != nil {
		log.Printf("Imports: saving %s: %v", g.path, err)
	}
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// starved reports a group whose measured files (at least 3) are mostly under min MB/min.
func (g *groupStats) starved(group string, min float64) bool {
	if g == nil || group == "" || min <= 0 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	v := g.Groups[strings.ToLower(group)]
	return len(v) >= 3 && median(v) < min
}

func (a *App) groupStats() *groupStats {
	a.groupsOnce.Do(func() { a.groups = loadGroupStats(a.Config.GroupStatsFile) })
	return a.groups
}

// CheckImports measures recent imports, flags mislabeled ones and updates the group statistics.
func (a *App) CheckImports() {
	now := a.now()
	var files []importFile
	type source struct {
		name string
		app  *Arr
		get  func(time.Time) ([]importFile, error)
	}
	var sources []source
	if a.Sonarr != nil {
		sources = append(sources, source{"Sonarr", a.Sonarr.arr(), a.sonarrImports})
	}
	if a.Radarr != nil {
		sources = append(sources, source{"Radarr", a.Radarr, a.radarrImports})
	}
	apps := map[string]*Arr{}
	for _, s := range sources {
		apps[s.name] = s.app
		fs, err := s.get(now)
		if err != nil {
			log.Printf("Imports (%s): %v", s.name, err)
			continue
		}
		files = append(files, fs...)
	}
	g := a.groupStats()
	g.mu.Lock()
	defer g.mu.Unlock()
	gauges := map[string]float64{}
	low := map[string]float64{"sonarr": 0, "radarr": 0}
	for _, f := range files {
		mbm := f.mbPerMin()
		app := strings.ToLower(f.App)
		if mbm > 0 {
			gauges[series("curator_recent_import_mb_per_minute", "app", app, "item", f.Name, "group", f.Group)] = mbm
			if a.Config.ImportMinMBPerMin > 0 && mbm < a.Config.ImportMinMBPerMin {
				low[app]++
			}
			if _, done := g.Files[f.Key]; !done && f.Group != "" {
				k := strings.ToLower(f.Group)
				g.Groups[k] = append(g.Groups[k], mbm)
				if n := len(g.Groups[k]); n > 20 {
					g.Groups[k] = g.Groups[k][n-20:]
				}
			}
		}
		if _, done := g.Files[f.Key]; !done {
			g.Files[f.Key] = now.Unix()
		}
		if _, done := g.Flagged[f.Key]; f.mislabeled() && !done && f.HistoryQuery != "" {
			if a.flagMislabeled(apps[f.App], f) {
				g.Flagged[f.Key] = now.Unix()
			}
		}
	}
	for k, v := range low {
		gauges[series("curator_recent_imports_below_floor", "app", k)] = v
	}
	for grp, v := range g.Groups {
		if len(v) >= 3 {
			gauges[series("curator_group_mb_per_minute", "group", grp)] = median(v)
		}
	}
	metrics.SetGroup("imports", gauges)
	for _, m := range []map[string]int64{g.Files, g.Flagged} {
		for k, t := range m {
			if now.Sub(time.Unix(t, 0)) > 90*24*time.Hour {
				delete(m, k)
			}
		}
	}
	g.saveLocked()
}

// flagMislabeled marks the file's last grab as failed: the app blocklists that release and searches again.
func (a *App) flagMislabeled(app *Arr, f importFile) bool {
	type event struct {
		ID        int    `json:"id"`
		EventType string `json:"eventType"`
		Date      string `json:"date"`
	}
	var raw json.RawMessage
	if err := app.do("GET", "/api/v3/"+f.HistoryQuery, nil, &raw); err != nil {
		log.Printf("Imports (%s): history of %s: %v", f.App, f.Name, err)
		return false
	}
	var hist []event
	if err := json.Unmarshal(raw, &hist); err != nil { // Sonarr pages it
		var page struct {
			Records []event `json:"records"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			log.Printf("Imports (%s): history of %s: %v", f.App, f.Name, err)
			return false
		}
		hist = page.Records
	}
	id := 0
	latest := ""
	for _, h := range hist {
		if h.EventType == "grabbed" && h.Date > latest {
			id, latest = h.ID, h.Date
		}
	}
	if id == 0 {
		log.Printf("Imports (%s): %s is mislabeled (%s for %dp) but has no grab to mark failed", f.App, f.Name, f.MediaInfo.Resolution, f.Resolution)
		return true
	}
	if a.Config.DryRun {
		log.Printf("[dry run] Imports (%s): would mark %s failed (picture %s, released as %dp)", f.App, f.Name, f.MediaInfo.Resolution, f.Resolution)
		return false // not recorded: acted on once dry run is off
	}
	if err := app.do("POST", fmt.Sprintf("/api/v3/history/failed/%d", id), nil, nil); err != nil {
		log.Printf("Imports (%s): marking %s failed: %v", f.App, f.Name, err)
		return false
	}
	log.Printf("Imports (%s): %s is mislabeled (picture %s, released as %dp); marked failed: blocklisted, searching again",
		f.App, f.Name, f.MediaInfo.Resolution, f.Resolution)
	metrics.Inc("curator_actions_total", "action", "import_mislabeled", "reason", strings.ToLower(f.App))
	return true
}

type arrFileResource struct {
	ID           int    `json:"id"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	DateAdded    string `json:"dateAdded"`
	ReleaseGroup string `json:"releaseGroup"`
	Quality      struct {
		Quality struct {
			Resolution int `json:"resolution"`
		} `json:"quality"`
	} `json:"quality"`
	MediaInfo struct {
		RunTime    string `json:"runTime"`
		Resolution string `json:"resolution"`
	} `json:"mediaInfo"`
}

func (r arrFileResource) importFile(key, app, name, history string) (importFile, bool) {
	added, err := time.Parse(time.RFC3339, r.DateAdded)
	if err != nil {
		return importFile{}, false
	}
	f := importFile{Key: key, App: app, Name: name, Group: r.ReleaseGroup, Size: r.Size, Added: added,
		Resolution: r.Quality.Quality.Resolution, HistoryQuery: history}
	f.MediaInfo.RunTime, f.MediaInfo.Resolution = r.MediaInfo.RunTime, r.MediaInfo.Resolution
	return f, true
}

func (a *App) sonarrImports(now time.Time) ([]importFile, error) {
	q := url.Values{"start": {now.Add(-a.Config.UpgradeWindow).UTC().Format(time.RFC3339)}, "end": {now.UTC().Format(time.RFC3339)},
		"includeSeries": {"true"}, "includeEpisodeFile": {"true"}}
	var eps []struct {
		ID            int                    `json:"id"`
		SeasonNumber  int                    `json:"seasonNumber"`
		EpisodeNumber int                    `json:"episodeNumber"`
		Series        struct{ Title string } `json:"series"`
		EpisodeFile   *arrFileResource       `json:"episodeFile"`
	}
	if err := a.Sonarr.do("GET", "/api/v3/calendar?"+q.Encode(), nil, &eps); err != nil {
		return nil, err
	}
	var out []importFile
	for _, e := range eps {
		if e.EpisodeFile == nil {
			continue
		}
		name := fmt.Sprintf("%s S%02dE%02d", e.Series.Title, e.SeasonNumber, e.EpisodeNumber)
		if f, ok := e.EpisodeFile.importFile(fmt.Sprintf("sonarr:%d", e.EpisodeFile.ID), "Sonarr", name,
			fmt.Sprintf("history?episodeId=%d&pageSize=50", e.ID)); ok && now.Sub(f.Added) <= a.Config.UpgradeWindow {
			out = append(out, f)
		}
	}
	return out, nil
}

func (a *App) radarrImports(now time.Time) ([]importFile, error) {
	var movies []struct {
		ID        int              `json:"id"`
		Title     string           `json:"title"`
		Year      int              `json:"year"`
		MovieFile *arrFileResource `json:"movieFile"`
	}
	if err := a.Radarr.do("GET", "/api/v3/movie", nil, &movies); err != nil {
		return nil, err
	}
	var out []importFile
	for _, m := range movies {
		if m.MovieFile == nil {
			continue
		}
		if f, ok := m.MovieFile.importFile(fmt.Sprintf("radarr:%d", m.MovieFile.ID), "Radarr", fmt.Sprintf("%s (%d)", m.Title, m.Year),
			fmt.Sprintf("history/movie?movieId=%d", m.ID)); ok && now.Sub(f.Added) <= a.Config.UpgradeWindow {
			out = append(out, f)
		}
	}
	return out, nil
}
