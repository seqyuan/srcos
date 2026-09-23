package web

import (
	"strings"
	"testing"
)

func TestRenderMarkdownBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"heading", "# Title", []string{"<h1>Title</h1>"}},
		{"h3", "### Deep", []string{"<h3>Deep</h3>"}},
		{"paragraph", "hello\nworld", []string{"<p>hello world</p>"}},
		{"fenced code", "```go\nx := 1\n```", []string{`<pre><code class="language-go">`, "x := 1", "</code></pre>"}},
		{"indented code", "    $ ls\n    a.txt", []string{"<pre><code>", "$ ls", "a.txt", "</code></pre>"}},
		{"hr", "---", []string{"<hr>"}},
		{"blockquote", "> quoted\n> more", []string{"<blockquote>", "<p>quoted more</p>", "</blockquote>"}},
		{"unordered", "- a\n- b", []string{"<ul>", "<li>a</li>", "<li>b</li>", "</ul>"}},
		{"ordered", "1. a\n2. b", []string{"<ol>", "<li>a</li>", "<li>b</li>", "</ol>"}},
		{"table", "| a | b |\n| --- | ---: |\n| 1 | 2 |", []string{"<table", "<th>a</th>", `class="align-right"`, "<td>1</td>"}},
		{"task list", "- [x] done\n- [ ] todo", []string{"checkbox", "checked", "done"}},
	}
	for _, c := range cases {
		got := renderMarkdown(c.in)
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: renderMarkdown(%q) = %q, missing %q", c.name, c.in, got, want)
			}
		}
	}
}

func TestRenderMarkdownInline(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"**bold**", "<strong>bold</strong>"},
		{"__bold__", "<strong>bold</strong>"},
		{"*it*", "<em>it</em>"},
		{"~~gone~~", "<del>gone</del>"},
		{"use `x < y` here", "use <code>x &lt; y</code> here"},
		{"[text](https://example.com)", `<a href="https://example.com" target="_blank"`},
		{"<https://example.com>", `<a href="https://example.com"`},
		{"![alt](https://example.com/a.png)", `<img src="https://example.com/a.png" alt="alt"`},
		// Underscores inside an identifier must survive untouched.
		{"total_count", "total_count"},
	}
	for _, c := range cases {
		got := renderInline(c.in)
		if !strings.Contains(got, c.want) {
			t.Errorf("renderInline(%q) = %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

func TestRenderMarkdownEscapesHTML(t *testing.T) {
	got := renderMarkdown("<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>")
	if strings.Contains(got, "<script") {
		t.Errorf("raw HTML leaked through: %q", got)
	}
	if strings.Contains(got, "onerror") && !strings.Contains(got, "&lt;img") {
		t.Errorf("raw attribute leaked through: %q", got)
	}
}

func TestRenderMarkdownRejectsDangerousURLs(t *testing.T) {
	for _, in := range []string{
		"[x](javascript:alert(1))",
		"[x](java\nscript:alert(1))",
		"[x](data:text/html,<script>)",
		"![x](vbscript:msgbox)",
		"[x](relative/path.md)", // relative links would resolve against /view
	} {
		got := renderInline(in)
		if strings.Contains(got, "<a ") || strings.Contains(got, "<img ") {
			t.Errorf("renderInline(%q) = %q, want no link/img", in, got)
		}
	}
}

func TestSafeURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://example.com", true},
		{"http://example.com/a?b=1", true},
		{"mailto:a@b.c", true},
		{"#section", true},
		{"javascript:alert(1)", false},
		{"JaVaScRiPt:alert(1)", false},
		{"java\tscript:alert(1)", false},
		{"data:text/html,x", false},
		{"/etc/passwd", false},
		{"", false},
	}
	for _, c := range cases {
		if _, ok := safeURL(c.in); ok != c.want {
			t.Errorf("safeURL(%q) = %v, want %v", c.in, ok, c.want)
		}
	}
}
