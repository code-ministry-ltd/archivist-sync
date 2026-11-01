// Command sign signs a release's SHA256SUMS with the project's release key (spec 0012 §8). The
// key comes from the MINISIGN_SECRET_KEY environment variable, set only in the release workflow.
package main

import (
	"fmt"
	"os"

	"aead.dev/minisign"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sign <file>")
		os.Exit(2)
	}
	var key minisign.PrivateKey
	if err := key.UnmarshalText([]byte(os.Getenv("MINISIGN_SECRET_KEY"))); err != nil {
		fmt.Fprintln(os.Stderr, "MINISIGN_SECRET_KEY isn't a minisign secret key:", err)
		os.Exit(1)
	}
	msg, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[1]+".minisig", minisign.Sign(key, msg), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
