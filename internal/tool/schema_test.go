package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

// The JSON Schema rendering is ADR-018's first derivation: an agent (or the MCP
// client driving it) must be able to build a valid call from it, so the
// mapping from a frozen type set to schema types is a contract.
func TestInterfaceJSONSchema(t *testing.T) {
	i := Interface{
		Inputs: []Input{
			{Name: "samples", Type: TypeString, Description: "comma-separated sample ids", Required: true},
			{Name: "cores", Type: TypeInt, Min: ptr(1.0), Max: ptr(64.0), Default: float64(4)},
			{Name: "frac", Type: TypeFloat, Min: ptr(0.0), Max: ptr(1.0)},
			{Name: "paired", Type: TypeBool, Default: true},
			{Name: "mode", Type: TypeEnum, Values: []string{"fast", "thorough"}, Required: true},
			{Name: "ref", Type: TypePath, From: "data,scratch", Select: "directory", Required: true},
			{Name: "report", Type: TypeFile, Label: "report file"},
			{Name: "dir", Type: TypeDirPath},
		},
		Outputs: []Output{{Name: "out", Type: OutDirectory}},
	}

	raw, err := json.Marshal(i.JSONSchema())
	if err != nil {
		t.Fatal(err)
	}
	schema := string(raw)

	// The document must be a closed object: an extra key is a caller's bug, and
	// with additionalProperties:false a validating client catches it.
	for _, want := range []string{
		`"type":"object"`,
		`"additionalProperties":false`,
		`"required":["samples","mode","ref"]`,
		`"samples":{"description":"comma-separated sample ids","type":"string"}`,
		`"cores":{"default":4,"maximum":64,"minimum":1,"type":"integer"}`,
		`"frac":{"maximum":1,"minimum":0,"type":"number"}`,
		`"paired":{"default":true,"type":"boolean"}`,
		`"mode":{"enum":["fast","thorough"],"type":"string"}`,
		// A path parameter is a string in the sandbox's path space, and says
		// where its values come from — an agent cannot browse by itself.
		`"ref":{"description":"Sandbox path to a directory; list it with srcos_list_paths (storages: data, scratch)","type":"string"}`,
		// The label carries the description when no description is authored.
		`"report":{"description":"Sandbox path to a file — report file","type":"string"}`,
		`"dir":{"description":"Sandbox path to a directory","type":"string"}`,
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema is missing %s\n%s", want, schema)
		}
	}
	// Outputs are not part of the arguments schema.
	if strings.Contains(schema, `"out"`) {
		t.Errorf("outputs leaked into the argument schema: %s", schema)
	}
}

func TestInterfaceJSONSchemaWithoutInputs(t *testing.T) {
	raw, err := json.Marshal(Interface{}.JSONSchema())
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"type":"object"`) || !strings.Contains(s, `"properties":{}`) {
		t.Fatalf("empty interface = %s", s)
	}
	if strings.Contains(s, "required") {
		t.Fatalf("nothing is required in an empty interface: %s", s)
	}
}

func TestInterfaceJSONSchemaSkipsUnnamedInputs(t *testing.T) {
	// The validator rejects unnamed inputs at registration; the schema simply
	// omits one rather than emitting an invalid property name.
	i := Interface{Inputs: []Input{{Name: "  ", Type: TypeString}, {Name: "ok", Type: TypeString}}}
	raw, _ := json.Marshal(i.JSONSchema())
	if strings.Contains(string(raw), `"  "`) {
		t.Fatalf("an unnamed input became a property: %s", raw)
	}
	if !strings.Contains(string(raw), `"ok"`) {
		t.Fatalf("the named input is missing: %s", raw)
	}
}

func ptr(f float64) *float64 { return &f }
