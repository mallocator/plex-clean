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

	SonarrURL    string
	SonarrAPIKey string
	DeleteTag    string
	ArchiveTag   string
	ArchiveDir   string
	ArchiveShows []string // shows not in Sonarr, handled by file name (see files.go)
	DeleteShows  []string
	SearchDirs   []string

	RadarrURL    string
	RadarrAPIKey string
	UserFolders  map[string]string // Seerr user name -> base folder (USER_FOLDERS)
	TVSubdir     string
	MovieSubdir  string

	SimklClientID  string
	SimklTokenFile string
	SimklInterval  time.Duration // 0 disables the Simkl sync
	SeerrURL       string
	SeerrAPIKey    string
	SeerrUserID    int // Seerr user the blocklist entries are attributed to

	QbtURL         string
	QbtUser        string
	QbtPass        string
	SweepInterval  time.Duration // 0 disables the torrent sweep
	SweepRoot      string        // only torrents saved below this path are swept
	SweepSkipCats  []string      // categories managed elsewhere (Sonarr/Radarr remove their own torrents)
	SweepMaxRemove int           // refuse a sweep removing more than this many torrents and over half of them (missing mount)
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

	go loop(config.CheckInterval, func() { app.Route(); app.ProcessDue() })
	if app.Qbt != nil && config.SweepInterval > 0 {
		go loop(config.SweepInterval, app.Sweep)
	}
	if app.Simkl != nil && config.SimklInterval > 0 {
		go loop(config.SimklInterval, app.SimklSync)
	}

	log.Printf("plex-clean listening on :%d (grace %s, dry run %v, sonarr %v, radarr %v, sweep %v, user folders %d, simkl %v)",
		config.Port, config.GracePeriod, config.DryRun, app.Sonarr != nil, app.Radarr != nil,
		app.Qbt != nil && config.SweepInterval > 0, len(config.UserFolders), app.Simkl != nil)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), app.Routes()))
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
		OutputDir:     getEnv("OUTPUT_DIR", "/output"),
		StateFile:     getEnv("STATE_FILE", "/data/pending.json"),
		GracePeriod:   getDuration("GRACE_PERIOD", 24*time.Hour),
		CheckInterval: getDuration("CHECK_INTERVAL", 5*time.Minute),
		DryRun:        getEnv("DRY_RUN", "false") == "true",
		Debug:         getEnv("DEBUG", "false") == "true",

		SonarrURL:    strings.TrimRight(getEnv("SONARR_URL", ""), "/"),
		SonarrAPIKey: getEnv("SONARR_API_KEY", ""),
		DeleteTag:    getEnv("DELETE_TAG", "delete-after-watch"),
		ArchiveTag:   getEnv("ARCHIVE_TAG", "archive"),
		ArchiveDir:   getEnv("ARCHIVE_DIR", "/archive"),
		ArchiveShows: splitList(getEnv("ARCHIVE_SHOWS", "")),
		DeleteShows:  splitList(getEnv("DELETE_SHOWS", "")),
		SearchDirs:   splitList(getEnv("SEARCH_DIRS", "/downloads/ravi,/downloads/daniela")),

		RadarrURL:    strings.TrimRight(getEnv("RADARR_URL", ""), "/"),
		RadarrAPIKey: getEnv("RADARR_API_KEY", ""),
		UserFolders:  parseMap(getEnv("USER_FOLDERS", "")),
		TVSubdir:     getEnv("TV_SUBDIR", "tv"),
		MovieSubdir:  getEnv("MOVIE_SUBDIR", "movies"),

		SimklClientID:  getEnv("SIMKL_CLIENT_ID", ""),
		SimklTokenFile: getEnv("SIMKL_TOKEN_FILE", "/data/simkl.json"),
		SimklInterval:  getDuration("SIMKL_INTERVAL", 6*time.Hour),
		SeerrURL:       strings.TrimRight(getEnv("SEERR_URL", ""), "/"),
		SeerrAPIKey:    getEnv("SEERR_API_KEY", ""),
		SeerrUserID:    getInt("SEERR_USER_ID", 1),

		QbtURL:         strings.TrimRight(getEnv("QBT_URL", ""), "/"),
		QbtUser:        getEnv("QBT_USER", ""),
		QbtPass:        getEnv("QBT_PASS", ""),
		SweepInterval:  getDuration("SWEEP_INTERVAL", 15*time.Minute),
		SweepRoot:      getEnv("SWEEP_ROOT", "/downloads"),
		SweepSkipCats:  splitList(getEnv("SWEEP_SKIP_CATEGORIES", "sonarr,radarr")),
		SweepMaxRemove: getInt("SWEEP_MAX_REMOVE", 5),
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
