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

type plexSection struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Location []struct {
		Path string `json:"path"`
	} `json:"Location"`
}

// sections returns the sections of a type ("movie", "show") whose folders are below Root.
func (p *Plex) sections(typ string) ([]plexSection, error) {
	var s struct {
		MediaContainer struct {
			Directory []plexSection `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := p.get("/library/sections", &s); err != nil {
		return nil, err
	}
	var out []plexSection
	for _, d := range s.MediaContainer.Directory {
		in := false
		for _, l := range d.Location {
			in = in || l.Path == p.Root || strings.HasPrefix(l.Path, p.Root+"/")
		}
		if d.Type == typ && in {
			out = append(out, d)
		}
	}
	return out, nil
}

// ShowFolder returns the folder (Plex's path) of the show with this TVDB id, or "" if Plex doesn't have it.
func (p *Plex) ShowFolder(tvdbID int) (string, error) {
	secs, err := p.sections("show")
	if err != nil {
		return "", err
	}
	want := fmt.Sprintf("tvdb://%d", tvdbID)
	for _, d := range secs {
		var items struct {
			MediaContainer struct {
				Metadata []map[string]json.RawMessage `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := p.get("/library/sections/"+d.Key+"/all?type=2&includeGuids=1", &items); err != nil {
			return "", err
		}
		for _, m := range items.MediaContainer.Metadata {
			var guids []struct {
				ID string `json:"id"`
			}
			var key string
			if raw, ok := m["Guid"]; !ok || json.Unmarshal(raw, &guids) != nil || json.Unmarshal(m["ratingKey"], &key) != nil {
				continue
			}
			for _, g := range guids {
				if g.ID != want {
					continue
				}
				var meta struct {
					MediaContainer struct {
						Metadata []struct {
							Location []struct {
								Path string `json:"path"`
							} `json:"Location"`
						} `json:"Metadata"`
					} `json:"MediaContainer"`
				}
				if err := p.get("/library/metadata/"+key, &meta); err != nil {
					return "", err
				}
				if md := meta.MediaContainer.Metadata; len(md) > 0 && len(md[0].Location) > 0 {
					return md[0].Location[0].Path, nil
				}
			}
		}
	}
	return "", nil
}

// CollectionFilms returns the files of the movies in the Plex collection named like the TMDB collection
// ("Mission: Impossible Collection"), across the movie sections below Root.
func (p *Plex) CollectionFilms(collection string) ([]archivedFilm, error) {
	norm := func(s string) string {
		return normalizeTitle(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), " collection"))
	}
	secs, err := p.sections("movie")
	if err != nil {
		return nil, err
	}
	var out []archivedFilm
	for _, d := range secs {
		var cols struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey string `json:"ratingKey"`
					Title     string `json:"title"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := p.get("/library/sections/"+d.Key+"/collections", &cols); err != nil {
			return nil, err
		}
		for _, c := range cols.MediaContainer.Metadata {
			if norm(c.Title) != norm(collection) {
				continue
			}
			var kids struct {
				MediaContainer struct {
					Metadata []struct {
						Year  int `json:"year"`
						Media []struct {
							Part []struct {
								File string `json:"file"`
							} `json:"Part"`
						} `json:"Media"`
					} `json:"Metadata"`
				} `json:"MediaContainer"`
			}
			if err := p.get("/library/collections/"+c.RatingKey+"/children", &kids); err != nil {
				return nil, err
			}
			for _, m := range kids.MediaContainer.Metadata {
				for _, md := range m.Media {
					for _, pt := range md.Part {
						base := pt.File[strings.LastIndex(pt.File, "/")+1:]
						if i := strings.LastIndex(base, "."); i > 0 {
							base = base[:i]
						}
						out = append(out, archivedFilm{Stem: base, Year: m.Year})
					}
				}
			}
		}
	}
	return out, nil
}
