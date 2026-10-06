package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Arr is a minimal client for the Sonarr/Radarr v3 API (same conventions in both).
type Arr struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

func NewArr(baseURL, apiKey string) *Arr {
	return &Arr{BaseURL: baseURL, APIKey: apiKey, Client: &http.Client{Timeout: 30 * time.Second}}
}

// Sonarr adds the episode lookups plex-clean needs.
type Sonarr struct{ Arr }

func (s *Sonarr) arr() *Arr { return &s.Arr }

type SonarrSeries struct {
	ID              int    `json:"id"`
	Title           string `json:"title"`
	CleanTitle      string `json:"cleanTitle"`
	Path            string `json:"path"`
	SeriesType      string `json:"seriesType"` // standard, daily, anime
	Monitored       bool   `json:"monitored"`
	Tags            []int  `json:"tags"`
	AlternateTitles []struct {
		Title string `json:"title"`
	} `json:"alternateTitles"`
}

type SonarrEpisode struct {
	ID            int  `json:"id"`
	SeasonNumber  int  `json:"seasonNumber"`
	EpisodeNumber int  `json:"episodeNumber"`
	HasFile       bool `json:"hasFile"`
	EpisodeFileID int  `json:"episodeFileId"`
}

type SonarrEpisodeFile struct {
	ID   int    `json:"id"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type SonarrTag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

func NewSonarr(baseURL, apiKey string) *Sonarr {
	return &Sonarr{Arr: *NewArr(baseURL, apiKey)}
}

// TagMap returns tag labels by ID.
func (s *Arr) TagMap() (map[int]string, error) {
	var tags []SonarrTag
	if err := s.do("GET", "/api/v3/tag", nil, &tags); err != nil {
		return nil, err
	}
	m := map[int]string{}
	for _, t := range tags {
		m[t.ID] = t.Label
	}
	return m, nil
}

func (s *Arr) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, s.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", s.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s %s: %w", s.BaseURL, method, path, err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s %s %s: %s %s", s.BaseURL, method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// FindSeries matches a show name from Plex/Jellyfin against Sonarr's titles and alternate titles.
func (s *Sonarr) FindSeries(name string) (*SonarrSeries, error) {
	var all []SonarrSeries
	if err := s.do("GET", "/api/v3/series", nil, &all); err != nil {
		return nil, err
	}
	want := normalizeTitle(name)
	for i := range all {
		sr := &all[i]
		if normalizeTitle(sr.Title) == want {
			return sr, nil
		}
		for _, alt := range sr.AlternateTitles {
			if normalizeTitle(alt.Title) == want {
				return sr, nil
			}
		}
	}
	return nil, nil
}

// TagLabels returns the labels of the given tag IDs.
func (s *Sonarr) TagLabels(ids []int) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var tags []SonarrTag
	if err := s.do("GET", "/api/v3/tag", nil, &tags); err != nil {
		return nil, err
	}
	byID := map[int]string{}
	for _, t := range tags {
		byID[t.ID] = t.Label
	}
	var labels []string
	for _, id := range ids {
		if l, ok := byID[id]; ok {
			labels = append(labels, l)
		}
	}
	return labels, nil
}

func (s *Sonarr) FindEpisode(seriesID, season, episode int) (*SonarrEpisode, error) {
	var eps []SonarrEpisode
	if err := s.do("GET", fmt.Sprintf("/api/v3/episode?seriesId=%d&seasonNumber=%d", seriesID, season), nil, &eps); err != nil {
		return nil, err
	}
	for i := range eps {
		if eps[i].SeasonNumber == season && eps[i].EpisodeNumber == episode {
			return &eps[i], nil
		}
	}
	return nil, nil
}

func (s *Sonarr) EpisodeFile(id int) (*SonarrEpisodeFile, error) {
	var f SonarrEpisodeFile
	if err := s.do("GET", fmt.Sprintf("/api/v3/episodefile/%d", id), nil, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *Sonarr) DeleteEpisodeFile(id int) error {
	return s.do("DELETE", fmt.Sprintf("/api/v3/episodefile/%d", id), nil, nil)
}

func (s *Sonarr) Unmonitor(episodeID int) error {
	return s.do("PUT", "/api/v3/episode/monitor", map[string]any{"episodeIds": []int{episodeID}, "monitored": false}, nil)
}

func isSuccess(code int) bool { return code >= 200 && code < 300 }
