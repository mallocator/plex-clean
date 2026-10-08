package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSeriesEscapesLabels(t *testing.T) {
	got := series("m", "a", `x"y`, "b", `c\d`)
	if want := `m{a="x\"y",b="c\\d"}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if series("m") != "m" {
		t.Fatal("no labels should render the bare name")
	}
}

func TestMetricsWrite(t *testing.T) {
	m := &metricSet{counters: map[string]float64{}, gauges: map[string]map[string]float64{}, help: map[string]string{}}
	m.Help("x_total", "help text")
	m.Inc("x_total", "action", "a")
	m.Inc("x_total", "action", "a")
	m.SetGroup("g", map[string]float64{series("y", "state", "old"): 3})
	m.SetGroup("g", map[string]float64{series("y", "state", "new"): 1}) // replaces the old label set
	var b strings.Builder
	m.Write(&b)
	out := b.String()
	for _, want := range []string{"# HELP x_total help text", "# TYPE x_total counter", `x_total{action="a"} 2`, "# TYPE y gauge", `y{state="new"} 1`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "old") {
		t.Errorf("stale gauge left over:\n%s", out)
	}
}

func TestCollectStreams(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{},{"TranscodeSession":{"videoDecision":"transcode","transcodeHwRequested":true}},{"TranscodeSession":{"videoDecision":"copy"}}]}}`))
	}))
	defer plex.Close()
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"NowPlayingItem":{"Name":"a"},"PlayState":{"PlayMethod":"Transcode"},"TranscodingInfo":{"IsVideoDirect":false,"HardwareAccelerationType":"none"}},
			{"NowPlayingItem":{"Name":"b"},"PlayState":{"PlayMethod":"DirectPlay"}},{"PlayState":{}}]`))
	}))
	defer jf.Close()
	a := &App{Plex: NewPlex(plex.URL, "/"), Jellyfin: NewJellyfin(jf.URL, "k")}
	if err := a.collectPlexStreams(); err != nil {
		t.Fatal(err)
	}
	if err := a.collectJellyfinStreams(); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	metrics.Write(&b)
	out := b.String()
	for _, want := range []string{
		`plexclean_streams{server="plex",kind="direct"} 2`, `plexclean_streams{server="plex",kind="transcode_hw"} 1`,
		`plexclean_streams{server="jellyfin",kind="transcode_sw"} 1`, `plexclean_streams{server="jellyfin",kind="direct"} 1`,
		`plexclean_streams{server="jellyfin",kind="transcode_hw"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestCollectArr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/queue":
			w.Write([]byte(`{"records":[{"trackedDownloadState":"downloading"},{"trackedDownloadState":"downloading"},{}]}`))
		case "/api/v3/wanted/missing":
			w.Write([]byte(`{"totalRecords":7}`))
		case "/api/v3/series":
			w.Write([]byte(`[{"statistics":{"sizeOnDisk":100,"episodeFileCount":2}},{"statistics":{"sizeOnDisk":50,"episodeFileCount":1}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := collectArr("sonarr", NewArr(srv.URL, "k")); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	metrics.Write(&b)
	out := b.String()
	for _, want := range []string{
		`plexclean_arr_queue_items{app="sonarr",state="downloading"} 2`, `plexclean_arr_queue_items{app="sonarr",state="unknown"} 1`,
		`plexclean_arr_missing{app="sonarr"} 7`, `plexclean_arr_library_bytes{app="sonarr"} 150`, `plexclean_arr_library_files{app="sonarr"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
