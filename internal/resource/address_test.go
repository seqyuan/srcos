package resource

import (
	"errors"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		in   string
		want Address
	}{
		{"srcos://file/home/notes.md", Address{Provider: "file", Scope: "home", Path: "notes.md"}},
		{"srcos://file/home", Address{Provider: "file", Scope: "home"}},
		{"srcos://file/home/", Address{Provider: "file", Scope: "home"}},
		{"srcos://file/home/a/b/c.txt", Address{Provider: "file", Scope: "home", Path: "a/b/c.txt"}},
		{"srcos://file/data/ref/genes.tsv", Address{Provider: "file", Scope: "data", Path: "ref/genes.tsv"}},
		{
			"srcos://file/workspace/out/x.txt?tool=hello-fanout",
			Address{Provider: "file", Scope: "workspace", Path: "out/x.txt", Tool: "hello-fanout"},
		},
		{"  srcos://file/home/a.txt  ", Address{Provider: "file", Scope: "home", Path: "a.txt"}},
		// A `?` inside a name must be percent-encoded; it decodes to a literal.
		{"srcos://file/home/a%3Fb.txt", Address{Provider: "file", Scope: "home", Path: "a?b.txt"}},
		{"srcos://file/home/a%20b.txt", Address{Provider: "file", Scope: "home", Path: "a b.txt"}},
		{"srcos://file/home/%E4%B8%AD%E6%96%87.md", Address{Provider: "file", Scope: "home", Path: "中文.md"}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"file/home/a.txt",                 // no scheme
		"http://file/home/a.txt",          // wrong scheme
		"srcos:/file/home/a.txt",          // malformed scheme separator
		"srcos://file",                    // no scope
		"srcos://file/",                   // empty scope
		"srcos://other/home/a.txt",        // unknown provider
		"srcos://file/Home/a.txt",         // scope alphabet
		"srcos://file/-bad/a.txt",         // scope must start alphanumeric
		"srcos://file/home/a%zz.txt",      // bad percent-encoding
		"srcos://file/workspace/a.txt",    // workspace needs ?tool=
		"srcos://file/home/a.txt?tool=x",  // tool only applies to workspace
		"srcos://file/home/a.txt?other=1", // unknown parameter
		"srcos://file/home/a.txt?tool=a&tool=b",
		"srcos://file/workspace/a?tool=Bad", // tool alphabet
	}
	for _, in := range cases {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", in, got)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) error %v is not ErrInvalid", in, err)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, in := range []string{
		"srcos://file/home/notes.md",
		"srcos://file/home/a/b/c.txt",
		"srcos://file/workspace/out/x.txt?tool=hello-fanout",
		"srcos://file/data/ref/genes.tsv",
	} {
		a, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got := a.String(); got != in {
			t.Errorf("String() = %q, want %q", got, in)
		}
		again, err := Parse(a.String())
		if err != nil {
			t.Fatalf("re-Parse(%q): %v", a.String(), err)
		}
		if again != a {
			t.Errorf("round trip changed the address: %+v -> %+v", a, again)
		}
	}
}

func TestStringEscapesPath(t *testing.T) {
	a, err := Parse("srcos://file/home/a%3Fb%20c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a.Path != "a?b c.txt" {
		t.Fatalf("path = %q", a.Path)
	}
	// The formatted form must re-escape so a copy-paste of it parses back.
	got := a.String()
	if got != "srcos://file/home/a%3Fb%20c.txt" {
		t.Errorf("String() = %q", got)
	}
}

func TestParent(t *testing.T) {
	a, err := Parse("srcos://file/workspace/out/x.txt?tool=t")
	if err != nil {
		t.Fatal(err)
	}
	parent, ok := a.Parent()
	if !ok || parent.Path != "out" {
		t.Errorf("Parent = %+v, %v", parent, ok)
	}
	// The scope and the tool survive the walk up, so a breadcrumb link keeps
	// addressing the same workspace.
	if parent.Scope != "workspace" || parent.Tool != "t" {
		t.Errorf("Parent lost its scope: %+v", parent)
	}
	root, _ := Parse("srcos://file/home")
	if _, ok := root.Parent(); ok {
		t.Error("the scope root must have no parent")
	}
	one, _ := Parse("srcos://file/home/a.txt")
	p, ok := one.Parent()
	if !ok || p.Path != "" {
		t.Errorf("Parent of a root child = %+v, %v", p, ok)
	}
}
