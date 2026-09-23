package resource

import "testing"

func TestClaimByExtension(t *testing.T) {
	reg := Default()
	cases := []struct {
		path  string
		isDir bool
		want  string
	}{
		{"a.md", false, "markdown"},
		{"README.MD", false, "markdown"},
		{"a.markdown", false, "markdown"},
		{"a.csv", false, "table"},
		{"a.tsv", false, "table"},
		{"a.png", false, "image"},
		{"photo.JPEG", false, "image"},
		{"icon.svg", false, "image"},
		{"report.pdf", false, "pdf"},
		{"index.html", false, "html"},
		{"page.htm", false, "html"},
		{"notes.txt", false, "text"},
		{"run.log", false, "text"},
		{"weird.xyz", false, "text"},
		{"Makefile", false, "text"},
		{"sub/dir/thing.csv", false, "table"},
		{"anything", true, "dir"},
		{"a.csv", true, "dir"}, // the filesystem wins over an extension
	}
	for _, c := range cases {
		got := reg.Claim(c.path, c.isDir)
		if got.ID != c.want {
			t.Errorf("Claim(%q, %v) = %q, want %q", c.path, c.isDir, got.ID, c.want)
		}
	}
}

func TestClaimPriorityAndFallback(t *testing.T) {
	custom := &Registry{viewers: []Viewer{
		{ID: "low", Kind: KindText, Patterns: []string{"*"}},
		{ID: "high", Kind: KindText, Priority: 10, Patterns: []string{"*.md"}},
	}}
	if got := custom.Claim("a.md", false); got.ID != "high" {
		t.Errorf("higher priority must win, got %q", got.ID)
	}
	// A registry with no matching pattern and no text entry still answers.
	empty := &Registry{viewers: []Viewer{
		{ID: "only-dirs", Kind: KindDir, Patterns: []string{"*"}},
	}}
	if got := empty.Claim("a.bin", false); got.Kind != KindText {
		t.Errorf("fallback = %+v, want text", got)
	}
	// A nil registry is the same as no registry.
	var nilReg *Registry
	if got := nilReg.Claim("a.md", false); got.Kind != KindText {
		t.Errorf("nil registry = %+v, want text", got)
	}
}

func TestLookupAndAll(t *testing.T) {
	reg := Default()
	if v, ok := reg.Lookup("markdown"); !ok || v.Kind != KindMarkdown {
		t.Fatalf("Lookup(markdown) = %+v, %v", v, ok)
	}
	if _, ok := reg.Lookup("nope"); ok {
		t.Error("Lookup(nope) must fail")
	}
	all := reg.All()
	if len(all) == 0 {
		t.Fatal("All() is empty")
	}
	// All() is in claim order: priority descending.
	for i := 1; i < len(all); i++ {
		if all[i-1].Priority < all[i].Priority {
			t.Errorf("All() is not in priority order: %+v", all)
			break
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"*", "a/b", false}, // * does not cross a separator
		{"*.md", "a.md", true},
		{"*.md", "a.txt", false},
		{"sub/*.md", "sub/a.md", true},
		{"sub/*.md", "a.md", false},
		{"**/*.md", "a.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**", "a/b/c", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "a/c", false}, // ? does not cross a separator
		{"*.[ch]", "main.c", true},
		{"*.[ch]", "main.h", true},
		{"*.[ch]", "main.go", false},
		{"[a-c]at", "bat", true},
		{"[a-c]at", "cat", true},
		{"[a-c]at", "hat", false},
		{"[!a-c]at", "hat", true},
		{"[!a-c]at", "bat", false},
		{"A*.TXT", "abc.txt", true}, // case-insensitive
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}
