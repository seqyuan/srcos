package web

import (
	"embed"
	"io/fs"
	"strings"
)

// The admin console's canvas is the one page built from a real frontend project
// (webui/, Vite + React). ADR-012's compromise: everything else stays a Go
// template, and the built assets are embedded in the same binary through
// embed.FS — so a deployment is still one file to copy.
//
// internal/web/dist is committed with a placeholder index.html and a .gitignore
// that keeps build products out of the repository: //go:embed needs the
// directory to exist, and a checkout must build without Node.
//
//go:embed all:dist
var uiAssets embed.FS

// UIAssets returns the built frontend, rooted at dist/.
func UIAssets() fs.FS {
	sub, err := fs.Sub(uiAssets, "dist")
	if err != nil {
		return nil
	}
	return sub
}

// UIBuilt reports whether a real build is present (rather than the placeholder).
//
// The placeholder is a valid page that explains how to build the canvas, so the
// answer is only used for a log line at startup — never to refuse a request.
func UIBuilt() bool {
	data, err := uiAssets.ReadFile("dist/index.html")
	if err != nil {
		return false
	}
	return !strings.Contains(string(data), "流程画布尚未构建")
}
