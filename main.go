// plex-clean applies house rules to media after it has been watched.
//
// It receives "watched" events from Plex (media.scrobble webhooks) and Jellyfin (Webhook plugin,
// PlaybackStop with PlayedToCompletion), writes a marker file per watched item, and queues episodes.
// After a grace period, episodes of shows that Sonarr manages are handled according to the show's tags:
//   - DELETE_TAG (default "delete-after-watch"): the episode file is deleted through Sonarr.
//   - ARCHIVE_TAG (default "archive"): the file is copied to ARCHIVE_DIR/<show>/Season NN/, then deleted through Sonarr.
//
// Both unmonitor the episode so Sonarr doesn't fetch it again. Shows and movies requested through Seerr are moved into
// their requester's folder (USER_FOLDERS, see routing.go). With Simkl configured, Seerr's blocklist and Radarr follow
// the owner's Simkl lists (simkl.go). Shows Sonarr doesn't manage can be listed in
// ARCHIVE_SHOWS / DELETE_SHOWS instead; their files are found in SEARCH_DIRS by release name. Separately, a
// periodic sweep removes completed qBittorrent torrents whose files are gone (formerly qbittorrent-cleaner).
// /metrics serves Prometheus metrics (metrics.go).
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port          int
	OutputDir     string // marker files; empty disables
	StateFile     string // pending queue
	GracePeriod   time.Duration
	CheckInterval time.Duration
	DryRun        bool
	Debug         bool

	SonarrURL            string
	SonarrAPIKey         string
	DeleteTag            string
	ArchiveTag           string
	ArchiveDir           string
	AnimeArchiveDir      string // archive for Sonarr series of type anime; empty uses ArchiveDir
	MovieArchiveDir      string // Radarr movies tagged MovieArchiveTag are moved here (movies.go); empty disables
	MovieArchiveTag      string
	JellyfinURL          string
	JellyfinAPIKey       string
	JellyfinArchiveUsers []string // their Jellyfin favourites (movies) are archived like the Radarr tag
	ArchiveShows         []string // shows not in Sonarr, handled by file name (see files.go)
	DeleteShows          []string
	SearchDirs           []string

	RadarrURL    string
	RadarrAPIKey string
	UserFolders  map[string]string // Seerr user name -> base folder (USER_FOLDERS)
	TVSubdir     string
	MovieSubdir  string

	SimklClientID  string
	SimklTokenFile string
	SimklInterval  time.Duration // 0 disables the Simkl sync
	SimklRulesFile string        // movies the cinema/collection rules handled (each rule acts once per movie)
	SeerrURL       string
	SeerrAPIKey    string
	SeerrUserID    int // Seerr user the blocklist entries are attributed to
	PlexURL        string
	PlexCollection string // Plex movie sections below this path are the collection (collection rule)

	QbtURL         string
	QbtUser        string
	QbtPass        string
	SweepInterval  time.Duration // 0 disables the torrent sweep
	SweepRoot      string        // only torrents saved below this path are swept
	SweepSkipCats  []string      // categories managed elsewhere (Sonarr/Radarr remove their own torrents)
	SweepMaxRemove int           // refuse a sweep removing more than this many torrents and over half of them (missing mount)
	StallTimeout   time.Duration // Sonarr/Radarr torrents without data for this long are rejected; 0 disables
	StatsInterval  time.Duration // refresh of the /metrics gauges (metrics.go); 0 disables

	DeleteRoots   []string // deletions only below these (the downloads share); see guard.go
	ProtectedDirs []string // never deleted in, in addition to the archive directories
}

func main() {
	config := loadConfig()
	queue, err := LoadQueue(config.StateFile)
	if err != nil {
		log.Fatalf("Loading queue %s: %v", config.StateFile, err)
	}

	app := &App{Config: config, Queue: queue}
	if config.SonarrURL != "" {
		app.Sonarr = NewSonarr(config.SonarrURL, config.SonarrAPIKey)
	}
	if config.QbtURL != "" {
		app.Qbt = NewQbittorrent(config.QbtURL, config.QbtUser, config.QbtPass)
	}
	if config.RadarrURL != "" {
		app.Radarr = NewArr(config.RadarrURL, config.RadarrAPIKey)
	}
	if config.SimklClientID != "" && config.SeerrURL != "" {
		app.Simkl = NewSimkl(config.SimklClientID, config.SimklTokenFile)
		app.Seerr = NewSeerr(config.SeerrURL, config.SeerrAPIKey, config.SeerrUserID)
	}
	if config.JellyfinURL != "" && config.JellyfinAPIKey != "" {
		app.Jellyfin = NewJellyfin(config.JellyfinURL, config.JellyfinAPIKey)
	}
	if config.PlexURL != "" {
		app.Plex = NewPlex(config.PlexURL, config.PlexCollection)
	}

	go loop(config.CheckInterval, func() { app.Route(); app.ProcessDue(); app.ArchiveMovies() })
	if app.Qbt != nil && config.SweepInterval > 0 {
		go loop(config.SweepInterval, func() { app.Sweep(); app.CheckDownloads() })
	}
	if config.StatsInterval > 0 {
		go app.statsLoop(config.StatsInterval)
	}
	if app.Simkl != nil && config.SimklInterval > 0 {
		go loopRetry(config.SimklInterval, 10*time.Minute, app.SimklSync)
	}

	log.Printf("plex-clean listening on :%d (grace %s, dry run %v, sonarr %v, radarr %v, sweep %v, user folders %d, simkl %v)",
		config.Port, config.GracePeriod, config.DryRun, app.Sonarr != nil, app.Radarr != nil,
		app.Qbt != nil && config.SweepInterval > 0, len(config.UserFolders), app.Simkl != nil)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), app.Routes()))
}

// loopRetry runs fn now and then every interval, or after retry if fn reports failure.
func loopRetry(interval, retry time.Duration, fn func() bool) {
	for {
		wait := interval
		if !fn() {
			wait = retry
		}
		time.Sleep(wait)
	}
}

// loop runs fn now and then every interval.
func loop(interval time.Duration, fn func()) {
	for {
		fn()
		time.Sleep(interval)
	}
}

func loadConfig() Config {
	return Config{
		Port:          getInt("PORT", 3333),
		OutputDir:     getEnv("OUTPUT_DIR", ""), // marker files are optional (the old Tautulli-era "watched" folder)
		StateFile:     getEnv("STATE_FILE", "/data/pending.json"),
		GracePeriod:   getDuration("GRACE_PERIOD", 24*time.Hour),
		CheckInterval: getDuration("CHECK_INTERVAL", 5*time.Minute),
		DryRun:        getEnv("DRY_RUN", "false") == "true",
		Debug:         getEnv("DEBUG", "false") == "true",

		SonarrURL:            strings.TrimRight(getEnv("SONARR_URL", ""), "/"),
		SonarrAPIKey:         getEnv("SONARR_API_KEY", ""),
		DeleteTag:            getEnv("DELETE_TAG", "delete-after-watch"),
		ArchiveTag:           getEnv("ARCHIVE_TAG", "archive"),
		ArchiveDir:           getEnv("ARCHIVE_DIR", "/archive"),
		AnimeArchiveDir:      getEnv("ANIME_ARCHIVE_DIR", ""),
		MovieArchiveDir:      getEnv("MOVIE_ARCHIVE_DIR", ""),
		MovieArchiveTag:      getEnv("MOVIE_ARCHIVE_TAG", "archive"),
		JellyfinURL:          strings.TrimRight(getEnv("JELLYFIN_URL", ""), "/"),
		JellyfinAPIKey:       getEnv("JELLYFIN_API_KEY", ""),
		JellyfinArchiveUsers: splitList(getEnv("JELLYFIN_ARCHIVE_USERS", "")),
		ArchiveShows:         splitList(getEnv("ARCHIVE_SHOWS", "")),
		DeleteShows:          splitList(getEnv("DELETE_SHOWS", "")),
		SearchDirs:           splitList(getEnv("SEARCH_DIRS", "/downloads/ravi,/downloads/daniela")),

		RadarrURL:    strings.TrimRight(getEnv("RADARR_URL", ""), "/"),
		RadarrAPIKey: getEnv("RADARR_API_KEY", ""),
		UserFolders:  parseMap(getEnv("USER_FOLDERS", "")),
		TVSubdir:     getEnv("TV_SUBDIR", "tv"),
		MovieSubdir:  getEnv("MOVIE_SUBDIR", "movies"),

		SimklClientID:  getEnv("SIMKL_CLIENT_ID", ""),
		SimklTokenFile: getEnv("SIMKL_TOKEN_FILE", "/data/simkl.json"),
		SimklInterval:  getDuration("SIMKL_INTERVAL", 6*time.Hour),
		SimklRulesFile: getEnv("SIMKL_RULES_FILE", "/data/simkl-rules.json"),
		SeerrURL:       strings.TrimRight(getEnv("SEERR_URL", ""), "/"),
		SeerrAPIKey:    getEnv("SEERR_API_KEY", ""),
		SeerrUserID:    getInt("SEERR_USER_ID", 1),
		PlexURL:        strings.TrimRight(getEnv("PLEX_URL", ""), "/"),
		PlexCollection: getEnv("PLEX_COLLECTION_ROOT", "/volume1/Video"),

		QbtURL:         strings.TrimRight(getEnv("QBT_URL", ""), "/"),
		QbtUser:        getEnv("QBT_USER", ""),
		QbtPass:        getEnv("QBT_PASS", ""),
		SweepInterval:  getDuration("SWEEP_INTERVAL", 15*time.Minute),
		SweepRoot:      getEnv("SWEEP_ROOT", "/downloads"),
		SweepSkipCats:  splitList(getEnv("SWEEP_SKIP_CATEGORIES", "sonarr,radarr")),
		SweepMaxRemove: getInt("SWEEP_MAX_REMOVE", 5),
		StallTimeout:   getDuration("STALL_TIMEOUT", 12*time.Hour),
		StatsInterval:  getDuration("STATS_INTERVAL", 2*time.Minute),

		DeleteRoots:   splitList(getEnv("DELETE_ROOTS", "/downloads")),
		ProtectedDirs: splitList(getEnv("PROTECTED_DIRS", "")),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getInt(key string, defaultValue int) int {
	v, err := strconv.Atoi(getEnv(key, strconv.Itoa(defaultValue)))
	if err != nil {
		log.Printf("Invalid %s, using %d", key, defaultValue)
		return defaultValue
	}
	return v
}

func getDuration(key string, defaultValue time.Duration) time.Duration {
	v, err := time.ParseDuration(getEnv(key, defaultValue.String()))
	if err != nil {
		log.Printf("Invalid %s, using %s", key, defaultValue)
		return defaultValue
	}
	return v
}

// parseMap reads "a=x,b=y".
func parseMap(s string) map[string]string {
	m := map[string]string{}
	for _, p := range splitList(s) {
		if k, v, ok := strings.Cut(p, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimRight(strings.TrimSpace(v), "/")
		}
	}
	return m
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
