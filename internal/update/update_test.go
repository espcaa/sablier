package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// a release with this platform's binary, the way `make release` lays it out
func release(t *testing.T, bin, sumOf []byte) string {
	sum := sha256.Sum256(sumOf)
	mux := http.NewServeMux()
	mux.HandleFunc("/"+AssetName(), func(w http.ResponseWriter, r *http.Request) { w.Write(bin) })
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + AssetName() + "\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDownloadAndReplace(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "sablier")
	os.WriteFile(exe, []byte("old"), 0o755)

	bin, err := Download(context.Background(), http.DefaultClient, release(t, []byte("new"), []byte("new")))
	if err != nil {
		t.Fatal(err)
	}
	if err := Replace(exe, bin); err != nil {
		t.Fatal(err)
	}
	Cleanup(exe)
	got, _ := os.ReadFile(exe)
	info, _ := os.Stat(exe)
	if string(got) != "new" || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("got %q, mode %v", got, info.Mode())
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
		t.Fatalf("left files behind: %v", entries)
	}
}

func TestDownloadRejectsBadChecksum(t *testing.T) {
	if _, err := Download(context.Background(), http.DefaultClient, release(t, []byte("evil"), []byte("new"))); err == nil {
		t.Fatal("installed a binary that doesn't match its checksum")
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.2.0", "v0.1.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"v1.0.0", "dev", false},
		{"v0.2.0-alpha.10", "v0.2.0-alpha.4", true},
		{"v0.2.0-alpha.4", "v0.2.0-alpha.10", false},
		{"v0.2.0", "v0.2.0-alpha.4", true},
		{"v0.2.0-alpha.4", "v0.2.0", false},
		{"v0.2.0-beta", "v0.2.0-alpha.4", true},
		{"v0.2.0-alpha.1", "v0.2.0-alpha", true},
		{"v0.2.0-alpha", "v0.2.0-alpha.1", false},
		{"v0.2.0+build.2", "v0.2.0+build.1", false},
		{"v1.0.0-rc.1", "v1.0.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
