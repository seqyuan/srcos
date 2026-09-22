package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/tool"
)

func writeJob(t *testing.T, id, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// demo is a tool with one required input that also carries a default, one
// optional enum, and a resource ceiling the job may only lower.
func demoTool() *tool.Tool {
	return &tool.Tool{
		SchemaVersion: 1,
		ID:            "demo",
		Version:       "0.1.0",
		Kind:          tool.KindTask,
		Backend:       tool.BackendLocal,
		Resources:     tool.Resources{CPU: 8, Memory: "8Gi", Walltime: "4:00:00"},
		Interface: tool.Interface{
			Inputs: []tool.Input{
				{Name: "samples", Type: tool.TypeString, Required: true, Default: "S1,S2"},
				{
					Name: "parallel", Type: tool.TypeInt, Default: json.Number("2"),
					Min: ptr(1.0), Max: ptr(20.0),
				},
				{Name: "mode", Type: tool.TypeEnum, Values: []string{"fast", "slow"}},
				{Name: "ref", Type: tool.TypePath, From: "cluster-share"},
			},
			Outputs: []tool.Output{{Name: "outs", Type: tool.OutDirectory}},
		},
		RequiresStorages: []string{"cluster-share"},
	}
}

func ptr[T any](v T) *T { return &v }

func TestLoadRequiresSchemaAndName(t *testing.T) {
	dir := writeJob(t, "j", `{"schemaVersion":1}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("expected name error, got %v", err)
	}
	dir = writeJob(t, "j2", `{"schemaVersion":2,"name":"x"}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "schemaVersion") {
		t.Fatalf("expected schemaVersion error, got %v", err)
	}
}

func TestLoadAcceptsIntegersExactly(t *testing.T) {
	// UseNumber keeps large integers exact; float64 would round them.
	dir := writeJob(t, "j", `{"schemaVersion":1,"name":"x","params":{"big":9007199254740993}}`)
	j, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := asString(j.Params["big"]); s != "9007199254740993" {
		t.Fatalf("integer lost precision: %s", s)
	}
}

func TestValidateRejectsUndeclaredParam(t *testing.T) {
	dir := writeJob(t, "j", `{"schemaVersion":1,"name":"x","params":{"nope":"1"}}`)
	j, _ := Load(dir)
	err := Validate(j, demoTool())
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("expected undeclared-param error, got %v", err)
	}
}

func TestValidateRequiredIsSatisfiedByToolDefault(t *testing.T) {
	// demoTool declares `samples` as required *with* a default. Omitting it is
	// therefore legal — but the default must actually reach the tool, which
	// EffectiveParams is responsible for.
	dir := writeJob(t, "j", `{"schemaVersion":1,"name":"x"}`)
	j, _ := Load(dir)
	if err := Validate(j, demoTool()); err != nil {
		t.Fatalf("a required input with a default should be satisfiable: %v", err)
	}

	eff := EffectiveParams(j, demoTool())
	if eff["samples"] != "S1,S2" {
		t.Fatalf("tool default was not applied: %v", eff)
	}
	env := strings.Join(ParamEnv(eff), " ")
	if !strings.Contains(env, "SRCOS_PARAM_SAMPLES=S1,S2") {
		t.Fatalf("default did not reach the environment: %s", env)
	}
	if !strings.Contains(env, "SRCOS_PARAM_PARALLEL=2") {
		t.Fatalf("numeric default did not reach the environment: %s", env)
	}
}

func TestValidateRequiredWithNoDefault(t *testing.T) {
	tl := demoTool()
	tl.Interface.Inputs[0].Default = nil // `samples` no longer has a fallback
	dir := writeJob(t, "j", `{"schemaVersion":1,"name":"x"}`)
	j, _ := Load(dir)
	err := Validate(j, tl)
	if err == nil || !strings.Contains(err.Error(), "required param") {
		t.Fatalf("expected required-param error, got %v", err)
	}
}

func TestValidateRejectsBlankStringOverridingDefault(t *testing.T) {
	// A blank string means "left empty in the form", so it falls back to the
	// default rather than blanking it out.
	dir := writeJob(t, "j", `{"schemaVersion":1,"name":"x","params":{"samples":"   "}}`)
	j, _ := Load(dir)
	tl := demoTool()
	tl.Interface.Inputs[0].Default = nil
	if err := Validate(j, tl); err == nil {
		t.Fatal("a blank value must not satisfy a required input without a default")
	}
}

func TestValidateRejectsResourceEscalation(t *testing.T) {
	cases := map[string]string{
		"cpu":      `{"schemaVersion":1,"name":"x","resources":{"cpu":16}}`,
		"memory":   `{"schemaVersion":1,"name":"x","resources":{"memory":"64Gi"}}`,
		"walltime": `{"schemaVersion":1,"name":"x","resources":{"walltime":"99:00:00"}}`,
	}
	for what, body := range cases {
		dir := writeJob(t, "j", body)
		j, err := Load(dir)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		err = Validate(j, demoTool())
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("%s: expected an escalation error, got %v", what, err)
		}
	}
}

func TestValidateAllowsLowering(t *testing.T) {
	dir := writeJob(t, "j", `{"schemaVersion":1,"name":"x","resources":{"cpu":2,"memory":"1Gi","walltime":"0:05:00"}}`)
	j, _ := Load(dir)
	if err := Validate(j, demoTool()); err != nil {
		t.Fatalf("lowering resources must be allowed: %v", err)
	}
	eff := EffectiveResources(j, demoTool())
	if eff.CPU != 2 || eff.Memory != "1Gi" || eff.Walltime != "0:05:00" {
		t.Fatalf("EffectiveResources = %+v", eff)
	}
}

func TestEffectiveResourcesFallsBackToTool(t *testing.T) {
	dir := writeJob(t, "j", `{"schemaVersion":1,"name":"x"}`)
	j, _ := Load(dir)
	eff := EffectiveResources(j, demoTool())
	if eff != demoTool().Resources {
		t.Fatalf("no overrides should yield the tool declaration, got %+v", eff)
	}
}

func TestValidateParamTypes(t *testing.T) {
	bad := map[string]string{
		"int gets a float":   `{"schemaVersion":1,"name":"x","params":{"parallel":1.5}}`,
		"int above max":      `{"schemaVersion":1,"name":"x","params":{"parallel":99}}`,
		"int below min":      `{"schemaVersion":1,"name":"x","params":{"parallel":0}}`,
		"enum not a member":  `{"schemaVersion":1,"name":"x","params":{"mode":"turbo"}}`,
		"string gets a bool": `{"schemaVersion":1,"name":"x","params":{"samples":true}}`,
	}
	for what, body := range bad {
		dir := writeJob(t, "j", body)
		j, err := Load(dir)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := Validate(j, demoTool()); err == nil {
			t.Errorf("%s: should have been rejected", what)
		}
	}

	good := `{"schemaVersion":1,"name":"x","params":{"parallel":12,"mode":"fast","samples":"A,B","ref":"/data/share/ref"}}`
	dir := writeJob(t, "j", good)
	j, _ := Load(dir)
	if err := Validate(j, demoTool()); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
}

func TestValidateRejectsRelativeOutput(t *testing.T) {
	dir := writeJob(t, "j", `{"schemaVersion":1,"name":"x","outputs":["out"]}`)
	j, _ := Load(dir)
	err := Validate(j, demoTool())
	if err == nil || !strings.Contains(err.Error(), "absolute sandbox path") {
		t.Fatalf("expected absolute-path error, got %v", err)
	}
}

func TestScanListsAndReportsBroken(t *testing.T) {
	root := t.TempDir()
	writeJobAt(t, root, "good", `{"schemaVersion":1,"name":"ok"}`)
	writeJobAt(t, root, "bad", `{"schemaVersion":1}`) // missing name
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Jobs) != 1 || res.Jobs[0].ID != "good" {
		t.Fatalf("jobs = %+v", res.Jobs)
	}
	// A malformed submission is reported, never silently dropped.
	if len(res.Broken) != 1 || !strings.Contains(res.Broken[0], "bad") {
		t.Fatalf("broken = %v", res.Broken)
	}
}

func TestScanMissingDirIsEmpty(t *testing.T) {
	res, err := Scan(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("a missing drop-box is not an error: %v", err)
	}
	if len(res.Jobs) != 0 || len(res.Broken) != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestParamEnvName(t *testing.T) {
	cases := map[string]string{
		"sample_id": "SRCOS_PARAM_SAMPLE_ID",
		"sample-id": "SRCOS_PARAM_SAMPLE_ID",
		"X":         "SRCOS_PARAM_X",
	}
	for in, want := range cases {
		if got := ParamEnvName(in); got != want {
			t.Errorf("ParamEnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParamEnvIsDeterministic(t *testing.T) {
	params := map[string]any{"b": "2", "a": "1", "c": "3"}
	first := strings.Join(ParamEnv(params), ",")
	for i := 0; i < 20; i++ {
		if got := strings.Join(ParamEnv(params), ","); got != first {
			t.Fatalf("ParamEnv ordering is unstable: %q vs %q", first, got)
		}
	}
	if first != "SRCOS_PARAM_A=1,SRCOS_PARAM_B=2,SRCOS_PARAM_C=3" {
		t.Fatalf("unexpected order: %s", first)
	}
}

func writeJobAt(t *testing.T, root, id, body string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
