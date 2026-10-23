package web

import (
	"html"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/inspect"
)

// An instance ID may come from a job directory name, which a tool can create in
// its own workspace. HTML-escaping it into an inline JS string is not enough:
// the browser decodes &#39; before compiling the handler. The rendered handler
// must decode to one quoted JSON literal, never to executable code outside it.
func TestAdminPageEscapesInstanceIDForJavaScript(t *testing.T) {
	payload := `x');alert(document.domain);('`
	page := AdminPage("SRCOS", "admin", AdminData{
		Instances: []inspect.AdminInstance{{
			Owner: "alice",
			Inst:  inspect.InstanceView{ID: payload, Tool: "demo", Kind: "task", Name: "n", State: "running"},
		}},
	})

	var handler string
	for _, line := range strings.Split(page, "\n") {
		if strings.Contains(line, "admStop(") {
			handler = html.UnescapeString(line)
			break
		}
	}
	if handler == "" {
		t.Fatalf("admStop handler not found in page:\n%s", page)
	}
	// The whole payload stays inside the double-quoted argument.
	if !strings.Contains(handler, `admStop("x');alert(document.domain);('")`) {
		t.Fatalf("instance id was not kept inside one JS string literal:\n%s", handler)
	}
	// If it had been single-quoted, the decoded handler would execute the
	// payload outside the argument. Make that regression explicit.
	if strings.Contains(handler, `admStop('x');alert`) {
		t.Fatalf("instance id escaped its JS string literal:\n%s", handler)
	}
}
