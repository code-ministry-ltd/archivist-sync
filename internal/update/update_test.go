package update

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"aead.dev/minisign"
)

// release serves a fake release of `bin`, its SHA256SUMS signed with `signer`.
func release(t *testing.T, bin []byte, signer minisign.PrivateKey) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(bin)
	sums := []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), AssetName()))
	sig := minisign.Sign(signer, sums)
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.0"}`))
	})
	mux.HandleFunc("/dl/v1.2.0/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("/dl/v1.2.0/SHA256SUMS.minisig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sig) })
	mux.HandleFunc("/dl/v1.2.0/"+AssetName(), func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bin) })
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func checker(t *testing.T, s *httptest.Server, pub minisign.PublicKey) *Checker {
	text, _ := pub.MarshalText()
	return &Checker{HTTP: s.Client(), LatestURL: s.URL + "/latest", DownloadBase: s.URL + "/dl/", Key: string(text)}
}

func TestInstallsOnlyReleasesSignedWithTheProjectKey(t *testing.T) {
	pub, priv, _ := minisign.GenerateKey(rand.Reader)
	_, stranger, _ := minisign.GenerateKey(rand.Reader)
	bin := []byte("new program")

	good := checker(t, release(t, bin, priv), pub)
	r, err := good.Latest(context.Background())
	if err != nil || r.Version != "1.2.0" {
		t.Fatalf("latest: %+v %v", r, err)
	}
	got, err := good.Fetch(context.Background(), r)
	if err != nil || string(got) != "new program" {
		t.Fatalf("fetch: %q %v", got, err)
	}

	forged := checker(t, release(t, bin, stranger), pub)
	r, _ = forged.Latest(context.Background())
	if _, err := forged.Fetch(context.Background(), r); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a release signed with another key was accepted: %v", err)
	}
}

func TestNewer(t *testing.T) {
	if !Newer("1.2.0", "1.1.9") || Newer("1.1.0", "1.1.0") || Newer("1.0.9", "1.1.0") || Newer("9.0.0", "dev") {
		t.Error("version order is wrong")
	}
}
