package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Jellyfin reads favourites: a movie a configured user marks as favourite (♥) is archived like one tagged in Radarr.
type Jellyfin struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

func NewJellyfin(baseURL, apiKey string) *Jellyfin {
	return &Jellyfin{BaseURL: baseURL, APIKey: apiKey, Client: &http.Client{Timeout: 60 * time.Second}}
}

func (j *Jellyfin) get(path string, out any) error {
	req, err := http.NewRequest("GET", j.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", j.APIKey))
	resp, err := j.Client.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return fmt.Errorf("jellyfin GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// FavoriteMovies returns the TMDB IDs of the movies the named users marked as favourite.
func (j *Jellyfin) FavoriteMovies(users []string) (map[int]bool, error) {
	var all []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := j.get("/Users", &all); err != nil {
		return nil, err
	}
	ids := map[int]bool{}
	for _, u := range all {
		wanted := false
		for _, n := range users {
			wanted = wanted || strings.EqualFold(n, u.Name)
		}
		if !wanted {
			continue
		}
		var items struct {
			Items []struct {
				ProviderIds map[string]string `json:"ProviderIds"`
			} `json:"Items"`
		}
		q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie"}, "Filters": {"IsFavorite"}, "Fields": {"ProviderIds"}}
		if err := j.get("/Users/"+u.ID+"/Items?"+q.Encode(), &items); err != nil {
			return nil, err
		}
		for _, it := range items.Items {
			for k, v := range it.ProviderIds {
				if strings.EqualFold(k, "tmdb") {
					if id, err := strconv.Atoi(v); err == nil {
						ids[id] = true
					}
				}
			}
		}
	}
	return ids, nil
}

// MergeArchiveVersions links the Jellyfin items of one film (TMDB id) below pathPrefix into one item with several
// versions; Jellyfin groups versions by itself only inside a per-movie folder, and the archive is flat. Groups of
// more than six items are refused (a broken filter or a mismatch, not versions). Returns how many items were merged.
func (j *Jellyfin) MergeArchiveVersions(tmdbID int, pathPrefix string) (int, error) {
	var items struct {
		Items []struct {
			ID          string            `json:"Id"`
			Path        string            `json:"Path"`
			ProviderIds map[string]string `json:"ProviderIds"`
		} `json:"Items"`
	}
	q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie"}, "Fields": {"ProviderIds,Path"}}
	if err := j.get("/Items?"+q.Encode(), &items); err != nil {
		return 0, err
	}
	var ids []string
	for _, it := range items.Items {
		if !strings.HasPrefix(it.Path, strings.TrimRight(pathPrefix, "/")+"/") {
			continue
		}
		for k, v := range it.ProviderIds {
			if strings.EqualFold(k, "tmdb") && v == strconv.Itoa(tmdbID) {
				ids = append(ids, it.ID)
			}
		}
	}
	if len(ids) < 2 {
		return 0, nil
	}
	if len(ids) > 6 {
		return 0, fmt.Errorf("%d items for tmdb %d, not merging", len(ids), tmdbID)
	}
	req, err := http.NewRequest("POST", j.BaseURL+"/Videos/MergeVersions?ids="+strings.Join(ids, ","), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", j.APIKey))
	resp, err := j.Client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return 0, fmt.Errorf("jellyfin merge versions: %s", resp.Status)
	}
	return len(ids), nil
}
