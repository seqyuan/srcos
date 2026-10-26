package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func preserveLogState(t *testing.T) {
	t.Helper()
	writer := log.Writer()
	flags := log.Flags()
	prefix := log.Prefix()
	def := slog.Default()
	t.Cleanup(func() {
		log.SetOutput(writer)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
		slog.SetDefault(def)
	})
}

func TestInstallJSONBridgesTheStandardLogger(t *testing.T) {
	preserveLogState(t)
	var buf bytes.Buffer
	if err := Install("json", &buf); err != nil {
		t.Fatal(err)
	}
	log.Printf("hello %d", 1)
	slog.Info("request", "request_id", "abc")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	var std, access map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &std); err != nil {
		t.Fatalf("standard log is not JSON: %v (%s)", err, lines[0])
	}
	if got, _ := std["msg"].(string); !strings.Contains(got, "hello 1") {
		t.Fatalf("bridged msg = %q", got)
	}
	if err := json.Unmarshal([]byte(lines[1]), &access); err != nil {
		t.Fatalf("access log is not JSON: %v (%s)", err, lines[1])
	}
	if access["request_id"] != "abc" {
		t.Fatalf("access fields = %v", access)
	}
}

func TestInstallRejectsAnUnknownFormat(t *testing.T) {
	preserveLogState(t)
	if err := Install("bogus", &bytes.Buffer{}); err == nil {
		t.Fatal("unknown format was accepted")
	}
}
