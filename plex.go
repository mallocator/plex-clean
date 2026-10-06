package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Plex reads the movie collection from Plex's local API (no token needed from an allowed network). Plex matches the
// curated library far better than Jellyfin, so the Simkl collection rule uses it to spot movies already owned.
type Plex struct {
	BaseURL string
	Root    string // only sections whose folders are below this path count as the collection
	Client  *http.Client
}

func NewPlex(baseURL, root string) *Plex {
	return &Plex{BaseURL: baseURL, Root: strings.TrimRight(root, "/"), Client: &http.Client{Timeout: 60 * time.Second}}
}

func (p *Plex) get(path string, out any) error {
	req, err := http.NewRequest("GET", p.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("plex GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return fmt.Errorf("plex GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// CollectionMovies returns the TMDB IDs of the movies in the movie sections below Root.
func (p *Plex) CollectionMovies() (map[int]bool, error) {
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key      string `json:"key"`
				Type     string `json:"type"`
				Location []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := p.get("/library/sections", &sections); err != nil {
		return nil, err
	}
	ids := map[int]bool{}
	for _, d := range sections.MediaContainer.Directory {
		inRoot := false
		for _, l := range d.Location {
			inRoot = inRoot || l.Path == p.Root || strings.HasPrefix(l.Path, p.Root+"/")
		}
		if d.Type != "movie" || !inRoot {
			continue
		}
		// Items carry both "guid" (a string) and "Guid" (the external IDs); encoding/json matches keys
		// case-insensitively, so pick "Guid" by hand.
		var items struct {
			MediaContainer struct {
				Metadata []map[string]json.RawMessage `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := p.get("/library/sections/"+d.Key+"/all?type=1&includeGuids=1", &items); err != nil {
			return nil, err
		}
		for _, m := range items.MediaContainer.Metadata {
			var guids []struct {
				ID string `json:"id"`
			}
			if raw, ok := m["Guid"]; !ok || json.Unmarshal(raw, &guids) != nil {
				continue
			}
			for _, g := range guids {
				var id int
				if _, err := fmt.Sscanf(g.ID, "tmdb://%d", &id); err == nil {
					ids[id] = true
				}
			}
		}
	}
	return ids, nil
}
