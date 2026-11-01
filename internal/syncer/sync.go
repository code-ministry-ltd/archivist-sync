package syncer

import (
	"context"
	"errors"
	"sync"

	"github.com/code-ministry-ltd/archivist-sync/internal/archive"
	"github.com/code-ministry-ltd/archivist-sync/internal/rules"
)

// UploadConcurrency is how many files go up at once.
const UploadConcurrency = 3

// Outcome is how one sync of a folder ended.
type Outcome struct {
	// State is "synced", "waiting" (the deletion guard holds removals), "empty" (nothing to
	// send, so nothing is removed) or "missing" (the folder isn't there).
	State    string `json:"state"`
	Uploaded int    `json:"uploaded"`
	// Unchanged is how many files the archive already had.
	Unchanged int `json:"unchanged"`
	// Removed is how many documents the import will remove; Waiting how many the guard holds.
	Removed  int    `json:"removed"`
	Waiting  int    `json:"waiting"`
	ImportID string `json:"importId,omitempty"`
	Scan     Scan   `json:"scan"`
}

// Options are what one sync needs besides the folder.
type Options struct {
	Client *archive.Client
	Dir    string
	Root   string
	Cache  *Cache
	// AllowRemoval lets the sync remove many documents: Remove them, on the page or in the app.
	AllowRemoval bool
	// Progress, if set, hears how many files have gone up.
	Progress func(done, total int)
}

// Run syncs one folder: scan, manifest, the deletion guard, uploads, commit (spec 0012 §6.3).
func Run(ctx context.Context, o Options) (Outcome, error) {
	scan, err := ScanFolder(o.Dir, o.Cache)
	if errors.Is(err, ErrMissing) {
		return Outcome{State: "missing"}, nil
	}
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Scan: scan}
	// An empty folder would empty its archive folder: a drive not mounted, a cloud folder
	// not yet downloaded. Nothing is sent and nothing removed.
	if len(scan.Entries) == 0 {
		out.State = "empty"
		return out, o.Cache.Save()
	}

	pages := (len(scan.Entries) + rules.ManifestPageSize - 1) / rules.ManifestPageSize
	var upload []string
	for page := 0; page < pages; page++ {
		end := min((page+1)*rules.ManifestPageSize, len(scan.Entries))
		res, err := o.Client.Manifest(ctx, o.Root, page, page == pages-1, scan.Entries[page*rules.ManifestPageSize:end])
		if err != nil {
			return out, err
		}
		upload = append(upload, res.Upload...)
		out.Unchanged += res.Unchanged
		out.Removed = res.DeleteCount
	}

	if !o.AllowRemoval && rules.NeedsDeletionConfirmation(out.Removed, len(scan.Entries)) {
		out.State = "waiting"
		out.Waiting = out.Removed
		out.Removed = 0
		if err := o.Client.SetWaitingRemovals(ctx, o.Root, out.Waiting); err != nil {
			return out, err
		}
		return out, o.Cache.Save()
	}

	if err := uploadAll(ctx, o, upload); err != nil {
		return out, err
	}
	out.Uploaded = len(upload)
	id, err := o.Client.Commit(ctx, o.Root)
	if err != nil {
		return out, err
	}
	out.ImportID = id
	out.State = "synced"
	if o.AllowRemoval {
		// Whatever was waiting has gone; clear it, and Remove them with it.
		if err := o.Client.SetWaitingRemovals(ctx, o.Root, 0); err != nil {
			return out, err
		}
	}
	return out, o.Cache.Save()
}

func uploadAll(ctx context.Context, o Options, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		next  int
		done  int
		first error
		wg    sync.WaitGroup
	)
	take := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if first != nil || next >= len(paths) {
			return "", false
		}
		p := paths[next]
		next++
		return p, true
	}
	for range min(UploadConcurrency, len(paths)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				p, ok := take()
				if !ok {
					return
				}
				// A file that changed or vanished since the scan, or that the archive refuses (too
				// large after all), is left for the next sync rather than failing this one.
				var err error
				if data, rerr := readFile(o.Dir, p); rerr == nil {
					if uerr := o.Client.Upload(ctx, o.Root, p, data); uerr != nil {
						var ae *archive.Error
						if !errors.As(uerr, &ae) || (ae.Status != 400 && ae.Status != 413) {
							err = uerr
						}
					}
				}
				mu.Lock()
				if err != nil && first == nil {
					first = err
					cancel()
				}
				done++
				d := done
				mu.Unlock()
				if o.Progress != nil {
					o.Progress(d, len(paths))
				}
			}
		}()
	}
	wg.Wait()
	return first
}
