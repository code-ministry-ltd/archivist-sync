package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/code-ministry-ltd/archivist-sync/internal/archive"
	"github.com/code-ministry-ltd/archivist-sync/internal/archive/archivetest"
	"github.com/code-ministry-ltd/archivist-sync/internal/rules"
)

func write(t *testing.T, dir, rel, text string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*archivetest.Fake, string, Options) {
	t.Helper()
	fake := archivetest.New()
	t.Cleanup(fake.Close)
	fake.Roots["archivist-sync-papers"] = &archivetest.Root{}
	dir := t.TempDir()
	return fake, dir, Options{
		Client: archive.New(fake.URL, fake.Token, "test"),
		Dir:    dir,
		Root:   "archivist-sync-papers",
		Cache:  OpenCache(filepath.Join(t.TempDir(), "cache.json")),
	}
}

func TestScanSendsWhatTheArchiveTakesAndListsTheRest(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Tax/return.pdf", "pdf")
	write(t, dir, "notes.md", "# Notes")
	write(t, dir, "photo.jpg", "jpg")
	write(t, dir, ".hidden/secret.md", "no")
	write(t, dir, ".DS_Store", "x")
	write(t, dir, "~$draft.docx", "lock")
	big := make([]byte, rules.MaxFileBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "big.pdf"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	scan, err := ScanFolder(dir, OpenCache(filepath.Join(t.TempDir(), "c.json")))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range scan.Entries {
		paths = append(paths, e.Path)
	}
	if fmt.Sprint(paths) != "[Tax/return.pdf notes.md]" {
		t.Errorf("sent %v", paths)
	}
	if fmt.Sprint(scan.LeftOut) != "[{big.pdf too_large} {photo.jpg format}]" {
		t.Errorf("left out %v", scan.LeftOut)
	}
}

func TestTheHashCacheReadsOnlyChangedFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "one")
	cache := OpenCache(filepath.Join(t.TempDir(), "c.json"))
	if _, err := ScanFolder(dir, cache); err != nil {
		t.Fatal(err)
	}
	// Poison the cached hash: a rescan of an unchanged file must reuse it, not read the file.
	for k, v := range cache.files {
		v.Hash = "cached"
		cache.files[k] = v
	}
	scan, _ := ScanFolder(dir, cache)
	if scan.Entries[0].Hash != "cached" {
		t.Error("an unchanged file was read again")
	}
	write(t, dir, "a.md", "two, longer")
	scan, _ = ScanFolder(dir, cache)
	if scan.Entries[0].Hash == "cached" {
		t.Error("a changed file wasn't read again")
	}
}

func TestSyncSendsWhatChangedAndMirrorsDeletions(t *testing.T) {
	fake, dir, o := setup(t)
	write(t, dir, "a.md", "one")
	write(t, dir, "b.md", "two")
	out, err := Run(context.Background(), o)
	if err != nil || out.State != "synced" || out.Uploaded != 2 {
		t.Fatalf("first sync: %+v %v", out, err)
	}
	write(t, dir, "a.md", "one, changed")
	os.Remove(filepath.Join(dir, "b.md"))
	out, err = Run(context.Background(), o)
	if err != nil || out.Uploaded != 1 || out.Removed != 1 || out.Unchanged != 0 {
		t.Fatalf("second sync: %+v %v", out, err)
	}
	fake.Lock(func() {
		if len(fake.Docs["archivist-sync-papers"]) != 1 {
			t.Errorf("archive holds %v", fake.Docs["archivist-sync-papers"])
		}
	})
}

func TestTheGuardHoldsALargeRemovalUntilAllowed(t *testing.T) {
	fake, dir, o := setup(t)
	for i := range 30 {
		write(t, dir, fmt.Sprintf("n%d.md", i), fmt.Sprint(i))
	}
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		os.Remove(filepath.Join(dir, fmt.Sprintf("n%d.md", i)))
	}
	out, err := Run(context.Background(), o)
	if err != nil || out.State != "waiting" || out.Waiting != 25 {
		t.Fatalf("guard: %+v %v", out, err)
	}
	fake.Lock(func() {
		if fake.Roots["archivist-sync-papers"].WaitingRemovals != 25 || len(fake.Docs["archivist-sync-papers"]) != 30 {
			t.Error("the archive wasn't told, or documents were removed")
		}
	})
	o.AllowRemoval = true
	out, err = Run(context.Background(), o)
	if err != nil || out.State != "synced" || out.Removed != 25 {
		t.Fatalf("allowed: %+v %v", out, err)
	}
	fake.Lock(func() {
		if fake.Roots["archivist-sync-papers"].WaitingRemovals != 0 || len(fake.Docs["archivist-sync-papers"]) != 5 {
			t.Error("removal wasn't made, or the flag wasn't cleared")
		}
	})
}

func TestAMissingOrEmptyFolderRemovesNothing(t *testing.T) {
	fake, dir, o := setup(t)
	write(t, dir, "a.md", "one")
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "a.md"))
	if out, _ := Run(context.Background(), o); out.State != "empty" {
		t.Errorf("empty folder: %s", out.State)
	}
	o.Dir = filepath.Join(dir, "unplugged")
	if out, _ := Run(context.Background(), o); out.State != "missing" {
		t.Errorf("missing folder: %s", out.State)
	}
	fake.Lock(func() {
		if len(fake.Docs["archivist-sync-papers"]) != 1 {
			t.Error("a document was removed")
		}
	})
}
