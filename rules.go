package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"
)

// ruleMemory remembers which movies the cinema and collection rules already handled, so each rule acts once per
// movie: when the owner re-monitors a movie in Radarr or deletes its exclusion, plex-clean leaves it that way.
type ruleMemory struct {
	Unmonitored map[int]string `json:"unmonitored"` // tmdb -> when and why
	Excluded    map[int]string `json:"excluded"`
	path        string
	changed     bool
}

// loadRuleMemory reads the memory file. Without one (first run), every movie Radarr already has unmonitored without a
// file and every existing exclusion counts as handled: earlier versions applied the rules without remembering them.
func loadRuleMemory(path string, radarrMovies []map[string]any, excluded map[int]bool) *ruleMemory {
	m := &ruleMemory{Unmonitored: map[int]string{}, Excluded: map[int]string{}, path: path}
	if path == "" {
		return m
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, m); err != nil {
			log.Printf("Simkl: reading %s: %v", path, err)
		}
		if m.Unmonitored == nil {
			m.Unmonitored = map[int]string{}
		}
		if m.Excluded == nil {
			m.Excluded = map[int]string{}
		}
		return m
	}
	if !errors.Is(err, os.ErrNotExist) {
		log.Printf("Simkl: reading %s: %v", path, err)
		return m
	}
	stamp := time.Now().Format("2006-01-02") + " (before v2.5)"
	for _, mv := range radarrMovies {
		if tmdb := toInts([]any{mv["tmdbId"]}); len(tmdb) == 1 && mv["monitored"] == false && mv["hasFile"] != true {
			m.Unmonitored[tmdb[0]] = stamp
		}
	}
	for id := range excluded {
		m.Excluded[id] = stamp
	}
	m.changed = true
	m.save()
	return m
}

func (m *ruleMemory) markUnmonitored(tmdb int, reason string) {
	m.Unmonitored[tmdb] = time.Now().Format("2006-01-02") + " " + reason
	m.changed = true
}

func (m *ruleMemory) markExcluded(tmdb int, reason string) {
	m.Excluded[tmdb] = time.Now().Format("2006-01-02") + " " + reason
	m.changed = true
}

func (m *ruleMemory) save() {
	if m.path == "" || !m.changed {
		return
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("Simkl: saving %s: %v", m.path, err)
		return
	}
	if err := os.Rename(tmp, m.path); err != nil {
		log.Printf("Simkl: saving %s: %v", m.path, err)
		return
	}
	m.changed = false
}
