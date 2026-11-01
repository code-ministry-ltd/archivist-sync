// Package update keeps Archivist Sync up to date (spec 0012 §7, §8): once a day it looks for a
// newer release, and installs it only if the release's SHA256SUMS carries the project's
// signature and the program matches its line there.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"aead.dev/minisign"
)

// PublicKey is the project's release key; only releases it signed are installed.
const PublicKey = "RWSPOnZ/LRy5Y5q73uXD0wS2a+ppM/n27eqUlel3BNxj/DPMHdj4RYEU"

// Repo is where releases are published.
const Repo = "code-ministry-ltd/archivist-sync"

// Release is a release's version and where its files are.
type Release struct {
	Version string
	// Base is the URL its files sit under.
	Base string
}

// AssetName is this system's program in a release, such as archivist-sync_darwin_arm64.
func AssetName() string {
	name := fmt.Sprintf("archivist-sync_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Checker finds and fetches releases. Its URLs can be pointed elsewhere for tests.
type Checker struct {
	HTTP *http.Client
	// LatestURL answers with the latest release's tag_name, as GitHub's API does.
	LatestURL string
	// DownloadBase is followed by the tag and a file name.
	DownloadBase string
	Key          string
}

// NewChecker checks GitHub releases with the project's key.
func NewChecker() *Checker {
	return &Checker{
		HTTP:         &http.Client{Timeout: 5 * time.Minute},
		LatestURL:    "https://api.github.com/repos/" + Repo + "/releases/latest",
		DownloadBase: "https://github.com/" + Repo + "/releases/download/",
		Key:          PublicKey,
	}
}

// Latest is the newest release.
func (c *Checker) Latest(ctx context.Context) (Release, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.LatestURL, nil)
	req.Header.Set("accept", "application/vnd.github+json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return Release{}, fmt.Errorf("release check answered %d", res.StatusCode)
	}
	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return Release{}, err
	}
	return Release{Version: strings.TrimPrefix(out.TagName, "v"), Base: c.DownloadBase + out.TagName + "/"}, nil
}

// Newer reports whether version a is later than b; a development build is never updated.
func Newer(a, b string) bool {
	if b == "dev" || b == "" {
		return false
	}
	pa, pb := parts(a), parts(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parts(v string) [3]int {
	var out [3]int
	for i, s := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		n, _ := strconv.Atoi(strings.SplitN(s, "-", 2)[0])
		out[i] = n
	}
	return out
}

func (c *Checker) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("download answered %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, limit))
}

// ErrBadSignature means a release wasn't signed with the project's key, or doesn't match it.
var ErrBadSignature = errors.New("the release isn't signed by The Archivist; not installed")

// Fetch downloads this system's program from a release and checks it: SHA256SUMS must carry
// the project's signature, and the program must match its line there.
func (c *Checker) Fetch(ctx context.Context, r Release) ([]byte, error) {
	sums, err := c.get(ctx, r.Base+"SHA256SUMS", 1<<20)
	if err != nil {
		return nil, err
	}
	sig, err := c.get(ctx, r.Base+"SHA256SUMS.minisig", 1<<16)
	if err != nil {
		return nil, err
	}
	var key minisign.PublicKey
	if err := key.UnmarshalText([]byte(c.Key)); err != nil {
		return nil, err
	}
	if !minisign.Verify(key, sums, sig) {
		return nil, ErrBadSignature
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == AssetName() {
			want = fields[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("the release has no program for this computer")
	}
	bin, err := c.get(ctx, r.Base+AssetName(), 200<<20)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return nil, ErrBadSignature
	}
	return bin, nil
}

// Replace puts a new program in place of the one at path. The running program keeps working:
// on Windows it's moved aside first, since a running program can't be overwritten there.
func Replace(path string, bin []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		old := path + ".old"
		_ = os.Remove(old)
		if err := os.Rename(path, old); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}

// CleanUp removes what an earlier update left behind beside the program.
func CleanUp(path string) {
	_ = os.Remove(path + ".old")
	_ = os.Remove(filepath.Clean(path + ".new"))
}
