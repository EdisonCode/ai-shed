package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSource serves one release from memory and counts downloads.
type fakeSource struct {
	binary    string
	checksum  string // overrides the real checksum when set
	downloads int
	err       error
}

func (f *fakeSource) Latest(context.Context) (string, error) { return "v1.2.3", f.err }

func (f *fakeSource) Download(_ context.Context, _, dir string, files ...string) error {
	if f.err != nil {
		return f.err
	}
	f.downloads++
	sum := f.checksum
	if sum == "" {
		h := sha256.Sum256([]byte(f.binary))
		sum = hex.EncodeToString(h[:])
	}
	if err := os.WriteFile(filepath.Join(dir, files[0]), []byte(f.binary), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, checksumsFile), []byte(sum+"  "+files[0]+"\nffff  shed-other\n"), 0o644)
}

func TestIsRelease(t *testing.T) {
	for version, want := range map[string]bool{
		"v0.1.0": true, "v12.3.45": true,
		"dev": false, "6f9c728": false, "v0.1.0-3-gabc123": false, "v0.1.0-dirty": false, "0.1.0": false,
	} {
		if got := IsRelease(version); got != want {
			t.Errorf("IsRelease(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestBinaryIsDownloadedVerifiedAndExecutable(t *testing.T) {
	src := &fakeSource{binary: "the linux build"}
	path, err := Binary(context.Background(), src, t.TempDir(), "v1.2.3", "linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(data) != "the linux build" || info.Mode().Perm() != 0o755 || filepath.Base(path) != "shed-linux-amd64" {
		t.Fatalf("binary at %s = %q, mode %v", path, data, info.Mode())
	}
}

func TestBinaryIsDownloadedOnce(t *testing.T) {
	src, cache := &fakeSource{binary: "b"}, t.TempDir()
	for range 2 {
		if _, err := Binary(context.Background(), src, cache, "v1.2.3", "linux-amd64"); err != nil {
			t.Fatal(err)
		}
	}
	if src.downloads != 1 {
		t.Fatalf("downloads = %d, want 1: the second call must use the cache", src.downloads)
	}
}

func TestBinaryWithWrongChecksumIsRejectedAndRemoved(t *testing.T) {
	src, cache := &fakeSource{binary: "tampered", checksum: strings.Repeat("0", 64)}, t.TempDir()
	_, err := Binary(context.Background(), src, cache, "v1.2.3", "linux-amd64")
	if err == nil || !strings.Contains(err.Error(), "does not match its published checksum") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(cache, "v1.2.3", "shed-linux-amd64")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("a binary that failed its checksum must not stay in the cache")
	}
}

func TestCorruptedCacheIsDownloadedAgain(t *testing.T) {
	src, cache := &fakeSource{binary: "good"}, t.TempDir()
	path, err := Binary(context.Background(), src, cache, "v1.2.3", "linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupted"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Binary(context.Background(), src, cache, "v1.2.3", "linux-amd64"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "good" || src.downloads != 2 {
		t.Fatalf("binary = %q after %d downloads; want the good one fetched again", data, src.downloads)
	}
}

func TestDownloadFailureIsReported(t *testing.T) {
	src := &fakeSource{err: errors.New("release not found")}
	if _, err := Binary(context.Background(), src, t.TempDir(), "v9.9.9", "linux-amd64"); err == nil || !strings.Contains(err.Error(), "release not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestReplaceSwapsTheBinary(t *testing.T) {
	dir := t.TempDir()
	target, fresh := filepath.Join(dir, "shed"), filepath.Join(dir, "download")
	os.WriteFile(target, []byte("old"), 0o755)
	os.WriteFile(fresh, []byte("new"), 0o644)
	if err := Replace(target, fresh); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(data) != "new" || info.Mode().Perm() != 0o755 {
		t.Fatalf("target = %q, mode %v", data, info.Mode())
	}
	if _, err := os.Stat(target + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the temporary file was left behind")
	}
}
