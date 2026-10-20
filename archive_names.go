package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Archive layout (owner, 2026-10-19; LOCA-26 in the home-network repo): Video/Movies is one flat folder of
// "<Title> (<Year>) [Label] [Label].ext" files, shows are flat folders of episodes (no season folders, no quality
// suffix). Labels come from a fixed vocabulary: edition, language (only when the audio is neither English nor the
// film's original language), and quality only when the same title and labels already exist. A film of a series the
// owner numbers ("Mission Impossible 7 - Dead Reckoning") continues that numbering, so the series sorts together.

type archivedFilm struct {
	Stem string // file name without extension
	Year int
}

var (
	sonarrQualityRe = regexp.MustCompile(`(?i)\s+\[?(Bluray|WEBDL|WEBRip|HDTV|SDTV|DVD|Raw-HD|Remux)(-\d{3,4}p)?( Remux)?\]?( Proper| REAL| v\d)*$`)
	numberedRe      = regexp.MustCompile(`^(.+?) (\d+|[IVX]+)(?: - .+)? \((\d{4})\)`)
	romanValues     = map[string]int{"I": 1, "II": 2, "III": 3, "IV": 4, "V": 5, "VI": 6, "VII": 7, "VIII": 8, "IX": 9, "X": 10}
	languageLabels  = map[string]string{"german": "Deutsch", "french": "Français", "italian": "Italiano", "portuguese": "Português", "danish": "Dansk", "spanish": "Español"}
)

// archiveEpisodeName drops Sonarr's quality suffix ("… - S01E02 - Title WEBDL-1080p.mkv").
func archiveEpisodeName(base string) string {
	ext := filepath.Ext(base)
	return sonarrQualityRe.ReplaceAllString(strings.TrimSuffix(base, ext), "") + ext
}

// cleanArchiveTitle makes a title safe for SMB/Windows clients, with Radarr-like colon handling.
func cleanArchiveTitle(t string) string {
	t = strings.NewReplacer("Mission: Impossible", "Mission Impossible", " or: ", " or ", ": ", " - ", ":", "-", "/", "-", `\`, "-",
		"?", "", "*", "", `"`, "'", "<", "", ">", "", "|", "").Replace(t)
	return strings.TrimRight(strings.Join(strings.Fields(t), " "), ".")
}

func editionLabel(e string) string {
	l := strings.ToLower(strings.TrimSpace(e))
	switch {
	case l == "":
		return ""
	case strings.HasPrefix(l, "extended"):
		return "Extended"
	case strings.Contains(l, "director"):
		return "Director's Cut"
	case strings.HasPrefix(l, "unrated"):
		return "Unrated"
	case strings.HasPrefix(l, "remastered"):
		return "Remastered"
	}
	return strings.TrimSpace(e)
}

// languageLabel names the audio language when the file has neither English nor the film's original language.
func languageLabel(fileLangs []string, original string) string {
	if len(fileLangs) == 0 {
		return ""
	}
	for _, l := range fileLangs {
		if strings.EqualFold(l, "English") || strings.EqualFold(l, original) || strings.EqualFold(l, "Unknown") {
			return ""
		}
	}
	if l, ok := languageLabels[strings.ToLower(fileLangs[0])]; ok {
		return l
	}
	return fileLangs[0]
}

func resolutionLabel(res int) string {
	switch {
	case res >= 2160:
		return "4K"
	case res >= 1080:
		return "1080p"
	case res >= 720:
		return "720p"
	}
	return "SD"
}

func archiveMovieStem(title string, year int, labels []string) string {
	s := fmt.Sprintf("%s (%d)", title, year)
	for _, l := range labels {
		if l != "" {
			s += " [" + l + "]"
		}
	}
	return s
}

// seriesTitle continues the owner's numbering of a film series: siblings are the archived films of the same TMDB
// collection. If they share a numbered prefix ("Mission Impossible 6 - Fallout (2018)") and the new film is newer
// than all of them, it becomes "<prefix> <n+1> - <subtitle>". Otherwise the official title stays.
func seriesTitle(official string, year int, siblings []archivedFilm) string {
	title := cleanArchiveTitle(official)
	prefixes := map[string]int{}
	maxNum, maxYear, roman := 0, 0, false
	for _, s := range siblings {
		m := numberedRe.FindStringSubmatch(s.Stem)
		if m == nil {
			if s.Year > maxYear {
				maxYear = s.Year
			}
			continue
		}
		prefixes[m[1]]++
		n, err := strconv.Atoi(m[2])
		if err != nil {
			n, roman = romanValues[m[2]], true
		}
		maxNum = max(maxNum, n)
		y, _ := strconv.Atoi(m[3])
		maxYear = max(maxYear, y, s.Year)
	}
	prefix, best := "", 0
	for p, c := range prefixes {
		if c > best || (c == best && p < prefix) {
			prefix, best = p, c
		}
	}
	if prefix == "" || maxNum == 0 || year <= maxYear {
		return title
	}
	num := strconv.Itoa(maxNum + 1)
	if roman {
		for r, v := range romanValues {
			if v == maxNum+1 {
				num = r
			}
		}
	}
	sub := title
	if strings.HasPrefix(strings.ToLower(title), strings.ToLower(prefix)) {
		sub = strings.Trim(title[len(prefix):], " -")
	}
	if sub == "" {
		return prefix + " " + num
	}
	return prefix + " " + num + " - " + sub
}

// archiveStemTaken reports whether a file with this stem (any extension, case-insensitive) is in dir.
func archiveStemTaken(dir, stem string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.EqualFold(strings.TrimSuffix(n, filepath.Ext(n)), stem) {
			return true
		}
	}
	return false
}

// archiveShowDir is the show's folder in an archive root: the folder Plex already has for this TVDB id, else an
// existing folder with the same normalised name, else the Sonarr folder name.
func (a *App) archiveShowDir(root string, tvdbID int, sonarrFolder string) string {
	if a.Plex != nil && tvdbID > 0 {
		if loc, err := a.Plex.ShowFolder(tvdbID); err == nil && loc != "" {
			if st, err := os.Stat(filepath.Join(root, filepath.Base(loc))); err == nil && st.IsDir() {
				return filepath.Join(root, filepath.Base(loc))
			}
		}
	}
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.IsDir() && normalizeTitle(e.Name()) == normalizeTitle(sonarrFolder) {
				return filepath.Join(root, e.Name())
			}
		}
	}
	return filepath.Join(root, sonarrFolder)
}
