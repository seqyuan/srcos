package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// A tool input of type int may be a database id or a coordinate above 2^53.
// The REST body must keep it exact, just like job.json (which uses UseNumber).
func TestParseBodyKeepsLargeIntegersExact(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/jobs", strings.NewReader(`{"n":9007199254740993}`))
	var body struct {
		N any `json:"n"`
	}
	if err := parseBody(req, &body); err != nil {
		t.Fatal(err)
	}
	n, ok := body.N.(json.Number)
	if !ok {
		t.Fatalf("number decoded as %T, want json.Number", body.N)
	}
	if n.String() != "9007199254740993" {
		t.Fatalf("number = %s, want exact 9007199254740993", n)
	}
}
