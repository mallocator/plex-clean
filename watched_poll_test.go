package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServers serves Jellyfin's played episodes and Plex's watched episodes from editable lists.
type fakeServers struct {
	mu       sync.Mutex
	jellyfin []map[string]any // Items for user "mallox"
	plex     []map[string]any // Metadata of the one show section
}

func (f *fakeServers) start(t *testing.T) (jf, px *httptest.Server) {
	jf = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			json.NewEncoder(w).Encode([]map[string]string{{"Id": "u1", "Name": "mallox"}})
		case strings.HasSuffix(r.URL.Path, "/Items"):
			json.NewEncoder(w).Encode(map[string]any{"Items": f.jellyfin})
		default:
			http.NotFound(w, r)
		}
	}))
	px = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/library/sections" {
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Directory": []map[string]string{{"key": "2", "type": "show"}, {"key": "1", "type": "movie"}}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": f.plex}})
	}))
	t.Cleanup(jf.Close)
	t.Cleanup(px.Close)
	return jf, px
}

func jfEpisode(id, series string, s, e int, at time.Time) map[string]any {
	return map[string]any{"Id": id, "Name": "Ep", "SeriesName": series, "ParentIndexNumber": s, "IndexNumber": e,
		"UserData": map[string]any{"LastPlayedDate": at.UTC().Format("2006-01-02T15:04:05.0000000Z")}}
}

func plexEpisode(rk, series string, s, e int, at time.Time) map[string]any {
	return map[string]any{"ratingKey": rk, "title": "Ep", "grandparentTitle": series, "parentIndex": s, "index": e, "lastViewedAt": at.Unix()}
}

func pollApp(t *testing.T, jf, px *httptest.Server, now time.Time) *App {
	dir := t.TempDir()
	q, err := LoadQueue(filepath.Join(dir, "pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &App{
		Config: Config{WatchedLookback: 48 * time.Hour, WatchedBurstMax: 3, WatchedSeenFile: filepath.Join(dir, "seen.json")},
		Queue:  q, Jellyfin: NewJellyfin(jf.URL, "k"), Plex: NewPlex(px.URL, "/volume1/Video"),
		Now: func() time.Time { return now },
	}
}

func queued(a *App) []string {
	var out []string
	for _, it := range a.Queue.Items() {
		out = append(out, fmt.Sprintf("%s S%02dE%02d %s", it.Series, it.Season, it.Episode, it.WatchedAt.UTC().Format("15:04")))
	}
	return out
}

func TestWatchedPoll(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f := &fakeServers{
		jellyfin: []map[string]any{jfEpisode("j1", "Bake Off", 17, 1, now.Add(-30*time.Hour))},
		plex:     []map[string]any{plexEpisode("p1", "Scrubs", 2, 3, now.Add(-2*time.Hour))},
	}
	jf, px := f.start(t)
	a := pollApp(t, jf, px, now)

	// first run: baseline only
	a.WatchedPoll()
	if got := queued(a); len(got) != 0 {
		t.Fatalf("first run queued %v, want nothing", got)
	}

	// a new Jellyfin mark (the missed webhook) and a re-report of the known ones
	f.mu.Lock()
	f.jellyfin = append([]map[string]any{jfEpisode("j3", "Bake Off", 17, 3, now.Add(-time.Hour))}, f.jellyfin...)
	f.mu.Unlock()
	a.WatchedPoll()
	if got := queued(a); len(got) != 1 || got[0] != "Bake Off S17E03 11:00" {
		t.Fatalf("queued %v, want only Bake Off S17E03 with its watch time", got)
	}

	// a mass sync: 4 new marks at once (burst max 3) are held, and stay held on the next poll
	f.mu.Lock()
	for i := 1; i <= 4; i++ {
		f.plex = append(f.plex, plexEpisode(fmt.Sprintf("f%d", i), "Futurama", 1, i, now.Add(-time.Minute)))
	}
	f.mu.Unlock()
	a.WatchedPoll()
	a.WatchedPoll()
	if got := queued(a); len(got) != 1 {
		t.Fatalf("burst queued episodes: %v", got)
	}

	// marks older than the lookback never count
	f.mu.Lock()
	f.jellyfin = append(f.jellyfin, jfEpisode("j0", "Old Show", 1, 1, now.Add(-72*time.Hour)))
	f.mu.Unlock()
	a.WatchedPoll()
	if got := queued(a); len(got) != 1 {
		t.Fatalf("old mark queued: %v", got)
	}

	// a second viewing of an episode is a new mark (date changed) and queues it again once handled
	a.Queue.Remove(a.Queue.Items()[0])
	f.mu.Lock()
	f.jellyfin[0] = jfEpisode("j3", "Bake Off", 17, 3, now.Add(-10*time.Minute))
	f.mu.Unlock()
	a.WatchedPoll()
	if got := queued(a); len(got) != 1 || got[0] != "Bake Off S17E03 11:50" {
		t.Fatalf("rewatch: queued %v", got)
	}
}
