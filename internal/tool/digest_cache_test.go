package tool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// poisonCachedDigest replaces the cached digest for dir with a sentinel while
// keeping its stat signature: a later Digest that returns the sentinel proves it
// answered from the cache; one that returns anything else proves it recomputed.
func poisonCachedDigest(t *testing.T, dir string) {
	t.Helper()
	e, ok := digestCache.Load(dir)
	if !ok {
		t.Fatalf("no cache entry for %s", dir)
	}
	entry := e.(digestCacheEntry)
	entry.digest = "sentinel"
	digestCache.Store(dir, entry)
}

func TestDigestServesFromCacheWhenStatSignatureUnchanged(t *testing.T) {
	dir := writeTool(t, map[string]string{"tool.yaml": minimalTask, "work.sh": minimalWork})
	if _, err := Digest(dir); err != nil {
		t.Fatal(err)
	}
	poisonCachedDigest(t, dir)
	got, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sentinel" {
		t.Fatalf("Digest recomputed although the stat signature was unchanged: %q", got)
	}
}

func TestDigestInvalidatesOnContentChange(t *testing.T) {
	dir := writeTool(t, map[string]string{"tool.yaml": minimalTask, "work.sh": minimalWork})
	first, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	poisonCachedDigest(t, dir)
	// A different size is a different stat signature.
	if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte(minimalWork+"\n# changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == "sentinel" {
		t.Fatal("cache served a stale digest after a content change")
	}
	if got == first {
		t.Fatal("a content change must change the digest")
	}
}

func TestDigestInvalidatesOnSameSizeContentChange(t *testing.T) {
	dir := writeTool(t, map[string]string{"tool.yaml": minimalTask, "work.sh": "aaaa"})
	first, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	poisonCachedDigest(t, dir)
	path := filepath.Join(dir, "work.sh")
	if err := os.WriteFile(path, []byte("bbbb"), 0o644); err != nil { // same size, new content
		t.Fatal(err)
	}
	// Pin mtime to a different instant: a coarse-timestamp filesystem (or a
	// fast rewrite) could otherwise leave the signature unchanged.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	got, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == "sentinel" || got == first {
		t.Fatalf("a same-size content change with a new mtime must change the digest (got %q)", got)
	}
}

func TestDigestInvalidatesOnModeChange(t *testing.T) {
	dir := writeTool(t, map[string]string{"tool.yaml": minimalTask, "work.sh": minimalWork})
	first, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	poisonCachedDigest(t, dir)
	if err := os.Chmod(filepath.Join(dir, "work.sh"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == "sentinel" {
		t.Fatal("chmod must invalidate the cache (mode is part of the signature)")
	}
	if got == first {
		t.Fatal("chmod must change the digest (mode is part of the content hash)")
	}
}
