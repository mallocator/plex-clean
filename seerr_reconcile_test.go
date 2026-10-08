package main

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestSeerrReconcile(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	seerr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v1/media/"):
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/api/v1/media/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/media":
			w.Write([]byte(`{"pageInfo":{"pages":1,"page":1},"results":[
				{"id":1,"mediaType":"tv","tmdbId":101,"tvdbId":0,"status":3,"seasons":[{"seasonNumber":1,"status":3}]},
				{"id":2,"mediaType":"tv","tmdbId":102,"tvdbId":0,"status":3,"seasons":[]},
				{"id":3,"mediaType":"tv","tmdbId":103,"tvdbId":903,"status":3,"seasons":[]},
				{"id":4,"mediaType":"tv","tmdbId":104,"tvdbId":0,"status":3,"seasons":[]},
				{"id":5,"mediaType":"tv","tmdbId":105,"tvdbId":0,"status":3,"seasons":[{"seasonNumber":1,"status":5},{"seasonNumber":2,"status":3}]},
				{"id":6,"mediaType":"movie","tmdbId":106,"status":3}]}`))
		case r.URL.Path == "/api/v1/request":
			w.Write([]byte(`{"results":[{"media":{"id":4}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer seerr.Close()
	sonarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 102 is in Sonarr by TMDB, 103 by TVDB
		w.Write([]byte(`[{"tvdbId":555,"tmdbId":102},{"tvdbId":903,"tmdbId":0}]`))
	}))
	defer sonarr.Close()

	a := &App{Seerr: NewSeerr(seerr.URL, "k", 1), Sonarr: NewSonarr(sonarr.URL, "k")}
	a.SeerrReconcile()
	sort.Strings(deleted)
	// only record 1: not in Sonarr, no request, nothing available (4 has a request, 5 an available season, 6 is a movie)
	if strings.Join(deleted, ",") != "1" {
		t.Fatalf("deleted %v, want [1]", deleted)
	}
}

func TestSeerrReconcileSkipsWithoutSonarrSeries(t *testing.T) {
	called := false
	seerr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"results":[]}`))
	}))
	defer seerr.Close()
	sonarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) }))
	defer sonarr.Close()
	a := &App{Seerr: NewSeerr(seerr.URL, "k", 1), Sonarr: NewSonarr(sonarr.URL, "k")}
	a.SeerrReconcile()
	if called {
		t.Fatal("Seerr was contacted although Sonarr reported no series")
	}
}
