package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PendingItem is a watched episode waiting for its grace period to pass.
type PendingItem struct {
	Series    string    `json:"series"`
	Season    int       `json:"season"`
	Episode   int       `json:"episode"`
	Title     string    `json:"title,omitempty"`
	Source    string    `json:"source"`
	WatchedAt time.Time `json:"watched_at"`
	Attempts  int       `json:"attempts,omitempty"`
}

func (p PendingItem) Key() string {
	return fmt.Sprintf("%s|%d|%d", normalizeTitle(p.Series), p.Season, p.Episode)
}

func (p PendingItem) String() string {
	return fmt.Sprintf("%s S%02dE%02d", p.Series, p.Season, p.Episode)
}

// Queue is a persistent set of pending items keyed by show and episode.
type Queue struct {
	mu    sync.Mutex
	path  string
	items map[string]PendingItem
}

// LoadQueue reads the queue file; a missing file is an empty queue.
func LoadQueue(path string) (*Queue, error) {
	q := &Queue{path: path, items: map[string]PendingItem{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return nil, err
	}
	var list []PendingItem
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, it := range list {
		q.items[it.Key()] = it
	}
	return q, nil
}

// Add queues a watched episode. A second viewing of a queued episode keeps the first time,
// so the grace period isn't extended by watching it again. Returns false if it was already queued.
func (q *Queue) Add(e WatchEvent, at time.Time) bool {
	it := PendingItem{Series: e.Series, Season: e.Season, Episode: e.Episode, Title: e.Title, Source: e.Source, WatchedAt: at}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.items[it.Key()]; ok {
		return false
	}
	q.items[it.Key()] = it
	q.saveLocked()
	return true
}

// Due returns the items whose grace period has passed.
func (q *Queue) Due(now time.Time, grace time.Duration) []PendingItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	var due []PendingItem
	for _, it := range q.items {
		if now.Sub(it.WatchedAt) >= grace {
			due = append(due, it)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].WatchedAt.Before(due[j].WatchedAt) })
	return due
}

func (q *Queue) Remove(it PendingItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.items, it.Key())
	q.saveLocked()
}

// Failed counts a failed attempt and returns the new count.
func (q *Queue) Failed(it PendingItem) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	cur, ok := q.items[it.Key()]
	if !ok {
		return 0
	}
	cur.Attempts++
	q.items[it.Key()] = cur
	q.saveLocked()
	return cur.Attempts
}

func (q *Queue) Items() []PendingItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := make([]PendingItem, 0, len(q.items))
	for _, it := range q.items {
		list = append(list, it)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].WatchedAt.Before(list[j].WatchedAt) })
	return list
}

// saveLocked writes the queue atomically (temp file + rename). Caller holds q.mu.
func (q *Queue) saveLocked() {
	if q.path == "" {
		return
	}
	list := make([]PendingItem, 0, len(q.items))
	for _, it := range q.items {
		list = append(list, it)
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	if err := os.MkdirAll(filepath.Dir(q.path), 0755); err != nil {
		logf("Error creating state directory: %v", err)
		return
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		logf("Error writing queue: %v", err)
		return
	}
	if err := os.Rename(tmp, q.path); err != nil {
		logf("Error saving queue: %v", err)
	}
}

// normalizeTitle makes show names comparable across Plex, Jellyfin and Sonarr:
// lowercase letters and digits only, a trailing "(2024)"-style year removed, leading "the" dropped.
func normalizeTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "("); i > 0 && strings.HasSuffix(s, ")") {
		inner := s[i+1 : len(s)-1]
		if len(inner) == 4 && strings.Trim(inner, "0123456789") == "" {
			s = strings.TrimSpace(s[:i])
		}
	}
	s = strings.TrimPrefix(s, "the ")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
