package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Prometheus metrics at /metrics, for the home monitoring stack (VictoriaMetrics/Grafana). Counters record what
// plex-clean did since it started; gauges describe the media stack it already talks to and are refreshed every
// STATS_INTERVAL by CollectStats. Plain text exposition format, no client library.

type metricSet struct {
	mu       sync.Mutex
	counters map[string]float64            // series ("name{labels}") -> value
	gauges   map[string]map[string]float64 // group -> series -> value; a group is replaced as a whole
	help     map[string]string             // metric name -> help text
}

var metrics = &metricSet{counters: map[string]float64{}, gauges: map[string]map[string]float64{}, help: map[string]string{}}

// series renders name{k="v",...} from alternating label names and values.
func series(name string, labels ...string) string {
	if len(labels) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(labels[i+1])
		fmt.Fprintf(&b, `%s="%s"`, labels[i], v)
	}
	b.WriteByte('}')
	return b.String()
}

// Add increases a counter.
func (m *metricSet) Add(name string, v float64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[series(name, labels...)] += v
}

// Inc increases a counter by one.
func (m *metricSet) Inc(name string, labels ...string) { m.Add(name, 1, labels...) }

// SetGroup replaces all gauge series of a group, so label sets that disappeared (e.g. a torrent state) vanish too.
func (m *metricSet) SetGroup(group string, values map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[group] = values
}

func (m *metricSet) Help(name, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.help[name] = text
}

func (m *metricSet) Write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type sample struct {
		series string
		value  float64
		kind   string
	}
	var all []sample
	for s, v := range m.counters {
		all = append(all, sample{s, v, "counter"})
	}
	for _, g := range m.gauges {
		for s, v := range g {
			all = append(all, sample{s, v, "gauge"})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].series < all[j].series })
	seen := map[string]bool{}
	for _, s := range all {
		name, _, _ := strings.Cut(s.series, "{")
		if !seen[name] {
			seen[name] = true
			if h := m.help[name]; h != "" {
				fmt.Fprintf(w, "# HELP %s %s\n", name, h)
			}
			fmt.Fprintf(w, "# TYPE %s %s\n", name, s.kind)
		}
		fmt.Fprintf(w, "%s %g\n", s.series, s.value)
	}
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	metrics.Write(w)
}

func init() {
	metrics.Help("plexclean_watched_total", "Finished viewings received, by source (plex, jellyfin).")
	metrics.Help("plexclean_actions_total", "Actions taken: episode_deleted, episode_archived, file_deleted, file_archived, movie_archived, torrent_swept, download_rejected, routed, tagged.")
	metrics.Help("plexclean_freed_bytes_total", "Bytes deleted from the downloads share.")
	metrics.Help("plexclean_pending_items", "Watched episodes waiting for their grace period.")
	metrics.Help("plexclean_qbittorrent_torrents", "Torrents by qBittorrent state.")
	metrics.Help("plexclean_qbittorrent_speed_bytes", "Current qBittorrent transfer rate in bytes per second.")
	metrics.Help("plexclean_arr_queue_items", "Items in the Sonarr/Radarr download queue by tracked state.")
	metrics.Help("plexclean_arr_missing", "Monitored episodes/movies without a file (Sonarr/Radarr wanted list).")
	metrics.Help("plexclean_arr_library_bytes", "Size on disk of everything Sonarr/Radarr manage.")
	metrics.Help("plexclean_arr_library_files", "Episode files (Sonarr) or movies with a file (Radarr).")
	metrics.Help("plexclean_streams", "Active playback sessions by server and kind (direct, transcode_hw, transcode_sw).")
	metrics.Help("plexclean_stats_last_success_timestamp_seconds", "Unix time each stats source last answered.")
}

// CollectStats refreshes the gauges. Each source is independent: one failing doesn't hide the others.
func (a *App) CollectStats() {
	now := float64(a.now().Unix())
	ok := map[string]float64{}
	metrics.SetGroup("queue", map[string]float64{series("plexclean_pending_items"): float64(len(a.Queue.Items()))})

	if a.Qbt != nil {
		if err := a.collectQbt(); err != nil {
			log.Printf("Stats: %v", err)
		} else {
			ok[series("plexclean_stats_last_success_timestamp_seconds", "source", "qbittorrent")] = now
		}
	}
	apps := map[string]*Arr{}
	if a.Sonarr != nil {
		apps["sonarr"] = a.Sonarr.arr()
	}
	if a.Radarr != nil {
		apps["radarr"] = a.Radarr
	}
	for name, app := range apps {
		if err := collectArr(name, app); err != nil {
			log.Printf("Stats (%s): %v", name, err)
		} else {
			ok[series("plexclean_stats_last_success_timestamp_seconds", "source", name)] = now
		}
	}
	if a.Plex != nil {
		if err := a.collectPlexStreams(); err != nil {
			log.Printf("Stats: %v", err)
		} else {
			ok[series("plexclean_stats_last_success_timestamp_seconds", "source", "plex")] = now
		}
	}
	if a.Jellyfin != nil {
		if err := a.collectJellyfinStreams(); err != nil {
			log.Printf("Stats: %v", err)
		} else {
			ok[series("plexclean_stats_last_success_timestamp_seconds", "source", "jellyfin")] = now
		}
	}
	metrics.mu.Lock()
	prev := metrics.gauges["last_success"]
	metrics.mu.Unlock()
	for k, v := range prev { // keep the last success of a source that failed this time
		if _, done := ok[k]; !done {
			ok[k] = v
		}
	}
	metrics.SetGroup("last_success", ok)
}

func (a *App) collectQbt() error {
	if err := a.Qbt.Login(); err != nil {
		return err
	}
	torrents, err := a.Qbt.Torrents()
	if err != nil {
		return err
	}
	states := map[string]float64{}
	for _, t := range torrents {
		states[series("plexclean_qbittorrent_torrents", "state", t.State)]++
	}
	metrics.SetGroup("qbt_torrents", states)
	var info struct {
		Down int64 `json:"dl_info_speed"`
		Up   int64 `json:"up_info_speed"`
	}
	if err := a.Qbt.get("/api/v2/transfer/info", &info); err != nil {
		return err
	}
	metrics.SetGroup("qbt_speed", map[string]float64{
		series("plexclean_qbittorrent_speed_bytes", "direction", "down"): float64(info.Down),
		series("plexclean_qbittorrent_speed_bytes", "direction", "up"):   float64(info.Up),
	})
	return nil
}

func collectArr(name string, app *Arr) error {
	items, err := app.Queue()
	if err != nil {
		return err
	}
	queue := map[string]float64{}
	for _, q := range items {
		state := q.TrackedState
		if state == "" {
			state = "unknown"
		}
		queue[series("plexclean_arr_queue_items", "app", name, "state", state)]++
	}
	metrics.SetGroup(name+"_queue", queue)

	var missing struct {
		Total int `json:"totalRecords"`
	}
	if err := app.do("GET", "/api/v3/wanted/missing?pageSize=1&monitored=true", nil, &missing); err != nil {
		return err
	}
	var size, files float64
	if name == "sonarr" {
		var all []struct {
			Statistics struct {
				SizeOnDisk       int64 `json:"sizeOnDisk"`
				EpisodeFileCount int   `json:"episodeFileCount"`
			} `json:"statistics"`
		}
		if err := app.do("GET", "/api/v3/series", nil, &all); err != nil {
			return err
		}
		for _, s := range all {
			size += float64(s.Statistics.SizeOnDisk)
			files += float64(s.Statistics.EpisodeFileCount)
		}
	} else {
		var all []struct {
			SizeOnDisk int64 `json:"sizeOnDisk"`
			HasFile    bool  `json:"hasFile"`
		}
		if err := app.do("GET", "/api/v3/movie", nil, &all); err != nil {
			return err
		}
		for _, m := range all {
			size += float64(m.SizeOnDisk)
			if m.HasFile {
				files++
			}
		}
	}
	metrics.SetGroup(name+"_library", map[string]float64{
		series("plexclean_arr_missing", "app", name):       float64(missing.Total),
		series("plexclean_arr_library_bytes", "app", name): size,
		series("plexclean_arr_library_files", "app", name): files,
	})
	return nil
}

// streamKinds starts every kind at zero so dashboards show 0 instead of no data.
func streamKinds(server string) map[string]float64 {
	m := map[string]float64{}
	for _, k := range []string{"direct", "transcode_hw", "transcode_sw"} {
		m[series("plexclean_streams", "server", server, "kind", k)] = 0
	}
	return m
}

func (a *App) collectPlexStreams() error {
	var sessions struct {
		MediaContainer struct {
			Metadata []struct {
				TranscodeSession *struct {
					VideoDecision string `json:"videoDecision"`
					HwRequested   bool   `json:"transcodeHwRequested"`
				} `json:"TranscodeSession"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := a.Plex.get("/status/sessions", &sessions); err != nil {
		return err
	}
	m := streamKinds("plex")
	for _, s := range sessions.MediaContainer.Metadata {
		kind := "direct"
		if t := s.TranscodeSession; t != nil && t.VideoDecision == "transcode" {
			kind = "transcode_sw"
			if t.HwRequested {
				kind = "transcode_hw"
			}
		}
		m[series("plexclean_streams", "server", "plex", "kind", kind)]++
	}
	metrics.SetGroup("plex_streams", m)
	return nil
}

func (a *App) collectJellyfinStreams() error {
	var sessions []struct {
		NowPlaying *struct {
			Name string `json:"Name"`
		} `json:"NowPlayingItem"`
		PlayState struct {
			PlayMethod string `json:"PlayMethod"`
		} `json:"PlayState"`
		Transcoding *struct {
			IsVideoDirect bool   `json:"IsVideoDirect"`
			HwAccel       string `json:"HardwareAccelerationType"`
		} `json:"TranscodingInfo"`
	}
	if err := a.Jellyfin.get("/Sessions?activeWithinSeconds=120", &sessions); err != nil {
		return err
	}
	m := streamKinds("jellyfin")
	for _, s := range sessions {
		if s.NowPlaying == nil {
			continue
		}
		kind := "direct"
		if s.PlayState.PlayMethod == "Transcode" && s.Transcoding != nil && !s.Transcoding.IsVideoDirect {
			kind = "transcode_sw"
			if hw := strings.ToLower(s.Transcoding.HwAccel); hw != "" && hw != "none" {
				kind = "transcode_hw"
			}
		}
		m[series("plexclean_streams", "server", "jellyfin", "kind", kind)]++
	}
	metrics.SetGroup("jellyfin_streams", m)
	return nil
}

// statsLoop runs CollectStats now and then every interval.
func (a *App) statsLoop(interval time.Duration) {
	for {
		a.CollectStats()
		time.Sleep(interval)
	}
}
