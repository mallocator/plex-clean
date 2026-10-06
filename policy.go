package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

const maxAttempts = 24 // with the default 5-minute interval, about two hours of retries

func logf(format string, args ...any) { log.Printf(format, args...) }

// ProcessDue applies the house rules to every queued episode whose grace period has passed.
func (a *App) ProcessDue() {
	for _, it := range a.Queue.Due(a.now(), a.Config.GracePeriod) {
		done, err := a.apply(it)
		if err != nil {
			n := a.Queue.Failed(it)
			log.Printf("%s: %v (attempt %d/%d)", it, err, n, maxAttempts)
			if n >= maxAttempts {
				log.Printf("%s: giving up", it)
				a.Queue.Remove(it)
			}
			continue
		}
		if done {
			a.Queue.Remove(it)
		}
	}
}

// apply handles one due episode. It returns done=true when the item can leave the queue.
func (a *App) apply(it PendingItem) (bool, error) {
	var series *SonarrSeries
	if a.Sonarr != nil {
		var err error
		if series, err = a.Sonarr.FindSeries(it.Series); err != nil {
			return false, err
		}
	}
	if series == nil {
		handled, err := a.applyByName(it)
		if err != nil {
			return false, err
		}
		if !handled {
			a.debugf("%s: no rule for this show, keeping", it)
		}
		return true, nil
	}
	labels, err := a.Sonarr.TagLabels(series.Tags)
	if err != nil {
		return false, err
	}
	action := ""
	for _, l := range labels {
		switch l {
		case a.Config.ArchiveTag:
			action = "archive" // archive wins if both tags are set: keeping is the safer choice
		case a.Config.DeleteTag:
			if action == "" {
				action = "delete"
			}
		}
	}
	if action == "" {
		if handled, err := a.applyByName(it); handled || err != nil {
			return err == nil, err
		}
		log.Printf("%s: no %q or %q tag on %s, keeping", it, a.Config.DeleteTag, a.Config.ArchiveTag, series.Title)
		return true, nil
	}

	ep, err := a.Sonarr.FindEpisode(series.ID, it.Season, it.Episode)
	if err != nil {
		return false, err
	}
	archiveRoot := a.Config.ArchiveDir
	if series.SeriesType == "anime" && a.Config.AnimeArchiveDir != "" {
		archiveRoot = a.Config.AnimeArchiveDir
	}
	archiveDst := func(f string) string {
		return filepath.Join(archiveRoot, filepath.Base(series.Path), fmt.Sprintf("Season %02d", it.Season), filepath.Base(f))
	}
	if ep == nil || !ep.HasFile || ep.EpisodeFileID == 0 {
		// Sonarr knows the show but not this file: downloaded by a qBittorrent RSS rule (before the show moved
		// to Sonarr, or a show an import list added). ARCHIVE_SHOWS/DELETE_SHOWS win; otherwise the show's tag
		// applies to the file found by name.
		if handled, err := a.applyByName(it); handled || err != nil {
			return err == nil, err
		}
		if err := a.applyToFiles(it, action, archiveDst); err != nil {
			return false, err
		}
		return true, nil
	}
	file, err := a.Sonarr.EpisodeFile(ep.EpisodeFileID)
	if err != nil {
		return false, err
	}

	if action == "archive" {
		dst := archiveDst(file.Path)
		if a.Config.DryRun {
			log.Printf("[dry run] %s: would copy %s to %s, then delete it in Sonarr", it, file.Path, dst)
			return true, nil
		}
		if err := copyFile(file.Path, dst, file.Size); err != nil {
			return false, fmt.Errorf("archiving to %s: %w", dst, err)
		}
		log.Printf("%s: archived to %s", it, dst)
	} else if a.Config.DryRun {
		log.Printf("[dry run] %s: would delete %s in Sonarr", it, file.Path)
		return true, nil
	}

	if err := a.Sonarr.DeleteEpisodeFile(file.ID); err != nil {
		return false, err
	}
	if err := a.Sonarr.Unmonitor(ep.ID); err != nil {
		log.Printf("%s: deleted, but unmonitoring failed: %v", it, err)
	}
	log.Printf("%s: deleted %s and unmonitored the episode", it, file.Path)
	return true, nil
}

// copyFile copies src to dst through a temp file and checks the size. An existing dst of the
// expected size counts as already archived.
func copyFile(src, dst string, wantSize int64) error {
	if st, err := os.Stat(dst); err == nil && st.Size() == wantSize {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0775); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0664)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && wantSize > 0 && n != wantSize {
		err = fmt.Errorf("copied %d bytes, expected %d", n, wantSize)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
