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
// USER_FOLDERS maps users to a base folder ("2-daniela=/downloads/daniela"; the key may also be just the name);
// shows go to <base>/<TV_SUBDIR>, movies to <base>/<MOVIE_SUBDIR>.
//   - An item with a mapped user tag outside that user's folder is moved there.
//   - An item without a user tag inside a user's folder (added or imported directly in Sonarr/Radarr) gets that
//     user's tag, so every item ends up tagged by owner. The tag label is the USER_FOLDERS key.
// Everything else is left alone.

var userTagLabel = regexp.MustCompile(`^\d+\s?-\s?(.+)$`)

// userName returns the user part of a Seerr tag label or USER_FOLDERS key ("2-daniela", "2 - daniela", "daniela").
func userName(label string) string {
	if m := userTagLabel.FindStringSubmatch(label); m != nil {
		return normalizeTitle(m[1])
	}
	return normalizeTitle(label)
}

// userBase returns the USER_FOLDERS key and base folder for the first tag that names a mapped user.
func (a *App) userBase(labels []string) (user, base string) {
	for _, l := range labels {
		if !userTagLabel.MatchString(l) {
			continue
		}
		name := userName(l)
		for u, b := range a.Config.UserFolders {
			if userName(u) == name {
				return u, b
			}
		}
	}
	return "", ""
}

// folderUser returns the USER_FOLDERS key whose <base>/<subdir> contains path.
func (a *App) folderUser(path, subdir string) string {
	for u, b := range a.Config.UserFolders {
		if strings.HasPrefix(filepath.Clean(path), filepath.Join(b, subdir)+"/") {
			return u
		}
	}
	return ""
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
			a.tagByFolder(c, kind, subdir, it, tags)
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

// tagByFolder gives an untagged item in a user's folder that user's tag (creating the tag if needed).
func (a *App) tagByFolder(c *Arr, kind, subdir string, it map[string]any, tags map[int]string) {
	path, _ := it["path"].(string)
	title, _ := it["title"].(string)
	user := a.folderUser(path, subdir)
	if user == "" {
		return
	}
	label := strings.ToLower(user)
	if !userTagLabel.MatchString(label) {
		return // a plain name key can route, but isn't a Seerr tag label to create
	}
	if a.Config.DryRun {
		log.Printf("[dry run] Route: would tag %s %q with %s (in %s's folder)", kind, title, label, user)
		return
	}
	id := -1
	for tid, l := range tags {
		if l == label {
			id = tid
		}
	}
	if id < 0 {
		var t SonarrTag
		if err := c.do("POST", "/api/v3/tag", map[string]string{"label": label}, &t); err != nil {
			log.Printf("Route: creating tag %s: %v", label, err)
			return
		}
		id = t.ID
		tags[id] = label
	}
	ids := append(toInts(it["tags"]), id)
	it["tags"] = ids
	itemID := toInts([]any{it["id"]})[0]
	if err := c.do("PUT", fmt.Sprintf("/api/v3/%s/%d", kind, itemID), it, nil); err != nil {
		log.Printf("Route: tagging %s %q: %v", kind, title, err)
		return
	}
	log.Printf("Route: tagged %s %q with %s (in %s's folder)", kind, title, label, user)
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
