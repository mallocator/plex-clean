package main

import (
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Per-user download folders. Seerr tags every request with the requester ("2-daniela"; older versions "2 - daniela").
// USER_FOLDERS maps those names to a base folder; shows go to <base>/<TV_SUBDIR>, movies to <base>/<MOVIE_SUBDIR>.
// Anything without a mapped user tag is left where it is.

var userTagLabel = regexp.MustCompile(`^\d+\s?-\s?(.+)$`)

// userBase returns the base folder for the first tag that names a mapped user.
func (a *App) userBase(labels []string) (user, base string) {
	for _, l := range labels {
		m := userTagLabel.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		name := normalizeTitle(m[1])
		for u, b := range a.Config.UserFolders {
			if normalizeTitle(u) == name {
				return u, b
			}
		}
	}
	return "", ""
}

var routeMu sync.Mutex

// Route moves Seerr-requested shows and movies into their requester's folder. Runs on Sonarr/Radarr webhooks and
// periodically as a fallback.
func (a *App) Route() {
	if len(a.Config.UserFolders) == 0 {
		return
	}
	routeMu.Lock()
	defer routeMu.Unlock()
	if a.Sonarr != nil {
		if err := a.routeArr(a.Sonarr.arr(), "series", a.Config.TVSubdir); err != nil {
			log.Printf("Route (Sonarr): %v", err)
		}
	}
	if a.Radarr != nil {
		if err := a.routeArr(a.Radarr, "movie", a.Config.MovieSubdir); err != nil {
			log.Printf("Route (Radarr): %v", err)
		}
	}
}

func (a *App) routeArr(c *Arr, kind, subdir string) error {
	tags, err := c.TagMap()
	if err != nil {
		return err
	}
	var items []map[string]any
	if err := c.do("GET", "/api/v3/"+kind, nil, &items); err != nil {
		return err
	}
	for _, it := range items {
		var labels []string
		for _, id := range toInts(it["tags"]) {
			labels = append(labels, tags[id])
		}
		user, base := a.userBase(labels)
		if user == "" {
			continue
		}
		root := filepath.Join(base, subdir)
		path, _ := it["path"].(string)
		title, _ := it["title"].(string)
		if path == "" || strings.HasPrefix(filepath.Clean(path), root+"/") {
			continue // already there
		}
		dst := filepath.Join(root, filepath.Base(path))
		if a.Config.DryRun {
			log.Printf("[dry run] Route: would move %s %q to %s (requested by %s)", kind, title, dst, user)
			continue
		}
		it["path"], it["rootFolderPath"] = dst, root
		id := toInts([]any{it["id"]})[0]
		if err := c.do("PUT", fmt.Sprintf("/api/v3/%s/%d?moveFiles=true", kind, id), it, nil); err != nil {
			log.Printf("Route: moving %s %q to %s: %v", kind, title, dst, err)
			continue
		}
		log.Printf("Route: moved %s %q to %s (requested by %s)", kind, title, dst, user)
	}
	return nil
}

// toInts converts JSON numbers (float64) to ints.
func toInts(v any) []int {
	list, _ := v.([]any)
	out := make([]int, 0, len(list))
	for _, x := range list {
		if f, ok := x.(float64); ok {
			out = append(out, int(f))
		}
	}
	return out
}
