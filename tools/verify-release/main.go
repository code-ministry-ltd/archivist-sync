// Command verify-release prints a release's SHA256SUMS once its signature checks out against
// the project's release key. The Archivist's website pins these checksums in its install
// scripts (spec 0012 §8), so a changed download is refused even on first install.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"aead.dev/minisign"

	"github.com/code-ministry-ltd/archivist-sync/internal/update"
)

func get(url string) []byte {
	c := &http.Client{Timeout: time.Minute}
	res, err := c.Get(url)
	if err != nil || res.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "couldn't download", url)
		os.Exit(1)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return raw
}

func main() {
	if len(os.Args) != 2 || !strings.HasPrefix(os.Args[1], "v") {
		fmt.Fprintln(os.Stderr, "usage: verify-release v1.2.3")
		os.Exit(2)
	}
	base := "https://github.com/" + update.Repo + "/releases/download/" + os.Args[1] + "/"
	sums, sig := get(base+"SHA256SUMS"), get(base+"SHA256SUMS.minisig")
	var key minisign.PublicKey
	if err := key.UnmarshalText([]byte(update.PublicKey)); err != nil || !minisign.Verify(key, sums, sig) {
		fmt.Fprintln(os.Stderr, "the release's signature doesn't check out; don't use it")
		os.Exit(1)
	}
	os.Stdout.Write(sums)
}
