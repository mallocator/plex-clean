package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArchiveIsNeverDeleted(t *testing.T) {
	// Sonarr reports a file inside a protected (archive) folder: plex-clean must not delete it.
	a, fs := appWithSonarr(t)
	path := fs.files[11].Path
	a.Config.ProtectedDirs = []string{filepath.Dir(filepath.Dir(filepath.Dir(path)))}
	watch(a, "The Simpsons", 38, 2, 25*time.Hour)
	a.ProcessDue()
	if len(fs.deleted) != 0 {
		t.Errorf("deleted %v inside the archive", fs.deleted)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("archive file gone")
	}

	// Name rules never remove files found in the archive, even if a search dir points there.
	b := newTestApp(t)
	b.Config.SearchDirs = []string{b.Config.ArchiveDir}
	b.Config.DeleteShows = []string{"Futurama"}
	f := filepath.Join(b.Config.ArchiveDir, "Futurama", "Futurama.S14E01.mkv")
	os.MkdirAll(filepath.Dir(f), 0755)
	os.WriteFile(f, []byte("x"), 0644)
	watch(b, "Futurama", 14, 1, 25*time.Hour)
	b.ProcessDue()
	if _, err := os.Stat(f); err != nil {
		t.Error("name rule deleted a file in the archive")
	}
}

func TestDeletionsOnlyBelowDeleteRoots(t *testing.T) {
	a, fs := appWithSonarr(t) // its media dir is a temp dir, outside /downloads
	a.Config.DeleteRoots = []string{"/downloads"}
	watch(a, "The Simpsons", 38, 2, 25*time.Hour)
	a.ProcessDue()
	if len(fs.deleted) != 0 {
		t.Errorf("deleted %v outside the delete roots", fs.deleted)
	}
	if err := a.deletable("/downloads/ravi/tv/x.mkv"); err != nil {
		t.Error(err)
	}
	for _, p := range []string{"/archive/Futurama/x.mkv", "/downloadsX/y", "/volume1/Video/Movies/x.mkv"} {
		a.Config.ArchiveDir = "/archive"
		if a.deletable(p) == nil {
			t.Errorf("%s should be refused", p)
		}
	}
}
