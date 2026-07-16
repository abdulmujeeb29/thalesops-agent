package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thalesops/agent/internal/models"
)

func TestVerifyStaticArtifact(t *testing.T) {
	// missing dir → error
	if err := verifyStaticArtifact(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing dir must fail")
	}
	// empty dir → error
	empty := t.TempDir()
	if err := verifyStaticArtifact(empty); err == nil {
		t.Fatal("empty dir must fail")
	}
	// files but no index.html → error
	noIndex := t.TempDir()
	os.WriteFile(filepath.Join(noIndex, "app.js"), []byte("x"), 0o644)
	if err := verifyStaticArtifact(noIndex); err == nil {
		t.Fatal("dir without index.html must fail")
	}
	// valid site → ok
	ok := t.TempDir()
	os.WriteFile(filepath.Join(ok, "index.html"), []byte("<h1>hi</h1>"), 0o644)
	if err := verifyStaticArtifact(ok); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}
}

func TestFlipCurrentIsAtomicAndRepointable(t *testing.T) {
	appDir := t.TempDir()
	relA := filepath.Join(appDir, "releases", "a")
	relB := filepath.Join(appDir, "releases", "b")
	os.MkdirAll(relA, 0o755)
	os.MkdirAll(relB, 0o755)
	os.WriteFile(filepath.Join(relA, "index.html"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(relB, "index.html"), []byte("B"), 0o644)

	current := filepath.Join(appDir, "current")

	if err := flipCurrent(appDir, relA); err != nil {
		t.Fatalf("first flip: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(current, "index.html")); string(got) != "A" {
		t.Fatalf("current should serve A, got %q", got)
	}
	// flip over an EXISTING symlink (the rename-over case) → serves B
	if err := flipCurrent(appDir, relB); err != nil {
		t.Fatalf("re-flip: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(current, "index.html")); string(got) != "B" {
		t.Fatalf("current should serve B after flip, got %q", got)
	}
	// rollback = flip back → serves A again
	if err := flipCurrent(appDir, relA); err != nil {
		t.Fatalf("rollback flip: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(current, "index.html")); string(got) != "A" {
		t.Fatalf("current should serve A after rollback, got %q", got)
	}
}

func TestPruneOldReleasesKeepsNewestAndLive(t *testing.T) {
	appDir := t.TempDir()
	releases := filepath.Join(appDir, "releases")
	// six releases, oldest → newest
	var dirs []string
	for _, name := range []string{"r1", "r2", "r3", "r4", "r5", "r6"} {
		d := filepath.Join(releases, name)
		os.MkdirAll(d, 0o755)
		now := time.Now()
		// stagger mtimes so ordering is deterministic
		mt := now.Add(-time.Duration(6-len(dirs)) * time.Minute)
		os.Chtimes(d, mt, mt)
		dirs = append(dirs, d)
	}
	// current points at the OLDEST (r1) — prune must never delete it
	if err := flipCurrent(appDir, dirs[0]); err != nil {
		t.Fatal(err)
	}

	sh := NewLogShipper(func([]models.LogLine) error { return nil }, nil)
	defer sh.Close()
	pruneOldReleases(sh, appDir, 3)

	left, _ := os.ReadDir(releases)
	if len(left) != 4 { // newest 3 + the live r1
		names := []string{}
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("expected 4 releases after prune (3 newest + live), got %v", names)
	}
	if _, err := os.Stat(dirs[0]); err != nil {
		t.Fatal("prune deleted the LIVE release")
	}
}
