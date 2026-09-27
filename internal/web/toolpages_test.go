package web

import (
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/tool"
)

// A service tool page must be its lifecycle surface: the form starts it, and a
// live instance offers "open" and "stop" — without which a normal user could
// see the tool but never instantiate it.
func TestServiceToolPageOffersLifecycle(t *testing.T) {
	svc := &tool.Tool{ID: "web", Name: "Web", Kind: tool.KindService, Version: "1.0.0"}
	inst := &inspect.InstanceView{
		ID: "alice-web-svc", Tool: "web", Kind: "service",
		State: "running", RoutePath: "/proxy/alice/web",
	}
	html := ToolFormPage("srcos", "alice", svc, nil, inst)

	for _, want := range []string{
		"启动服务",                  // the form's action label
		"/api/tools/", "/start", // the script routes services to the start endpoint
		"svcStop('alice-web-svc')", // the stop button
		"打开服务",                     // the route link
		`href="/proxy/alice/web/"`, // and where it points
		"运行中",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("service page is missing %q", want)
		}
	}
}

// With no live instance the page says so and offers no stop button.
func TestServiceToolPageWithoutInstance(t *testing.T) {
	svc := &tool.Tool{ID: "web", Name: "Web", Kind: tool.KindService, Version: "1.0.0"}
	html := ToolFormPage("srcos", "alice", svc, nil, nil)
	if !strings.Contains(html, "未运行") {
		t.Error("a service with no instance must say so")
	}
	if strings.Contains(html, "svcStop(") {
		t.Error("there is nothing to stop")
	}
}

// A task has no lifecycle: its page stays a submission form.
func TestTaskToolPageHasNoLifecycle(t *testing.T) {
	task := &tool.Tool{ID: "demo", Name: "Demo", Kind: tool.KindTask, Version: "1.0.0"}
	html := ToolFormPage("srcos", "alice", task, nil, nil)
	if strings.Contains(html, "svcStop(") || strings.Contains(html, "启动服务") {
		t.Error("a task page must not offer service lifecycle controls")
	}
	if !strings.Contains(html, "运行") {
		t.Error("a task page must offer its submit action")
	}
}

// The instance detail page lets a user stop their own running task or service.
func TestTaskPageOffersStopForALiveInstance(t *testing.T) {
	running := inspect.InstanceView{
		ID: "alice-web-svc", Tool: "web", Kind: "service",
		State: "running", RoutePath: "/proxy/alice/web",
	}
	html := TaskPage("srcos", running, nil, "")
	if !strings.Contains(html, "tkStop('alice-web-svc')") {
		t.Error("a running instance must offer a stop button")
	}
	if !strings.Contains(html, `href="/proxy/alice/web/"`) {
		t.Error("a running service must offer an open link")
	}

	done := running
	done.State = "succeeded"
	done.RoutePath = ""
	html = TaskPage("srcos", done, nil, "")
	if strings.Contains(html, "tkStop(") {
		t.Error("a finished instance has nothing to stop")
	}
}
