package flow

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/seqyuan/srcos/internal/tool"
)

// This file turns a validated flow + a sample table into concrete work: one job
// per (node, sample row), with every parameter resolved.
//
// It is a *planner*: it decides what would run and with which values, and it
// touches nothing. That separation is what makes `flow run --dry-run` possible
// — and being able to see the expansion before it happens is the difference
// between a pipeline you trust and a pipeline you hope works (annopi's
// "展开结果可读可审查" is the same instinct).

// SampleRow is one row of the sample table.
type SampleRow struct {
	// Index is the 0-based row number, used to keep per-sample directories
	// unique even when two rows share a sample name.
	Index int
	// ID is the sample's own name, from the `sample_id` column when the table
	// has one (it is also what `--param`-free flows use for display).
	ID     string
	Values map[string]string
}

// Samples is a parsed sample table.
type Samples struct {
	Path    string
	Columns []string
	Rows    []SampleRow
}

// LoadSamples reads a sample table (CSV or TSV, sniffed from the first line).
func LoadSamples(path string) (*Samples, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ParseSamples(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.Path = path
	return s, nil
}

// ParseSamples parses a sample table.
//
// The delimiter is sniffed (tab wins when the header has one) because these
// tables are written by hand, by Excel, and by pandas, and none of them agree.
func ParseSamples(text string) (*Samples, error) {
	text = strings.TrimPrefix(text, "\ufeff") // Excel's BOM
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("the sample table is empty")
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	if strings.Contains(firstLine(text), "\t") {
		reader.Comma = '\t'
	}
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("not a valid CSV/TSV table: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("the sample table has no header row")
	}

	header := make([]string, 0, len(records[0]))
	seen := map[string]bool{}
	for _, col := range records[0] {
		name := strings.TrimSpace(col)
		if name == "" {
			return nil, fmt.Errorf("the sample table has an empty column name")
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate column %q in the sample table", name)
		}
		seen[name] = true
		header = append(header, name)
	}

	out := &Samples{Columns: header}
	for i, rec := range records[1:] {
		if len(rec) == 0 {
			continue
		}
		if len(rec) != len(header) {
			return nil, fmt.Errorf("row %d has %d field(s), the header has %d", i+2, len(rec), len(header))
		}
		values := make(map[string]string, len(header))
		for j, name := range header {
			values[name] = strings.TrimSpace(rec[j])
		}
		// A completely empty row is a trailing newline, not a sample.
		empty := true
		for _, v := range values {
			if v != "" {
				empty = false
				break
			}
		}
		if empty {
			continue
		}
		out.Rows = append(out.Rows, SampleRow{Index: i, ID: values["sample_id"], Values: values})
	}
	if len(out.Rows) == 0 {
		return nil, fmt.Errorf("the sample table has a header but no rows")
	}
	return out, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// CheckColumns verifies that every sample column the flow reads exists.
//
// It is a separate call from ParseSamples because the flow is what knows which
// columns it needs; a table for a different flow is the operator's mistake to
// see, not the parser's.
func (f *Flow) CheckColumns(s *Samples) error {
	if s == nil {
		return fmt.Errorf("a sample table is required")
	}
	have := map[string]bool{}
	for _, c := range s.Columns {
		have[c] = true
	}
	var missing []string
	for _, field := range f.SampleFields() {
		if !have[field] {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the sample table is missing column(s) %v (it has %v)", missing, s.Columns)
	}
	return nil
}

// Unit is one job the plan calls for: a node, one sample row, resolved params.
type Unit struct {
	Node    string
	ToolID  string
	Tool    *tool.Tool
	Sample  SampleRow
	Segment string
	// Params are ready for validation and for job.json: every value has been
	// through the tool's own type check in Plan.
	Params map[string]any
	// Outputs are the declared output paths for this unit, in the view this
	// node's tool will see them (sandbox paths, or host paths when the tool
	// runs without a sandbox).
	Outputs []string
	// Tags identify the unit in the job list: filtering by run or sample is how
	// an operator makes sense of 200 generated jobs.
	Tags map[string]string
}

// PlanInput is everything the planner needs.
type PlanInput struct {
	Flow    *Flow
	Samples *Samples
	// Params are the values the user supplied for exposed inputs, keyed by
	// input name, or by "<node>.<input>" when a name alone would be ambiguous.
	Params map[string]string
	RunID  string
	// Manifests resolves a node's tool (the same lookup validation used).
	Manifests map[string]*tool.Tool
	// DataDir is the deployment's data directory, used to derive the host view
	// of flow paths for tools that run without a sandbox.
	DataDir string
	User    string
	// Form maps a platform path to the form one node's tool will see. Nil means
	// "sandbox paths only" (every tool has a sandbox).
	Form func(nodeID string, sandboxPath string) string
}

// Plan expands a flow into the jobs it would run, in dependency order.
//
// Every parameter of every job is decided here: from the sample table, from the
// user's values, from an upstream node's output, or from the node's own output
// mapping. A value that has no source is an error — which cannot happen for a
// validated flow, and is checked anyway because the plan is also reachable
// through `--dry-run` on a flow someone edited five seconds ago.
func Plan(in PlanInput) ([]Unit, error) {
	if in.Flow == nil || in.Samples == nil {
		return nil, fmt.Errorf("a flow and a sample table are required")
	}
	if err := in.Flow.CheckColumns(in.Samples); err != nil {
		return nil, err
	}
	if !ValidRunID(in.RunID) {
		return nil, fmt.Errorf("invalid run id %q", in.RunID)
	}

	// Bindings and exposes, indexed for lookup.
	boundTo := map[string]Binding{}
	for _, b := range in.Flow.Bindings {
		_, _, name, _ := parseAddr(b.To)
		node, _, _, _ := parseAddr(b.To)
		boundTo[node+"."+name] = b
	}
	exposed := map[string]Expose{}
	for _, e := range in.Flow.Expose {
		exposed[e.Node+"."+e.Input] = e
	}

	// A user value addressed by bare input name must be unambiguous.
	userParamFor := func(nodeID, input string) (string, bool) {
		if v, ok := in.Params[nodeID+"."+input]; ok {
			return v, true
		}
		v, ok := in.Params[input]
		return v, ok
	}

	var units []Unit
	for _, node := range in.Flow.Order() {
		manifest := in.Manifests[node.ID]
		if manifest == nil {
			return nil, fmt.Errorf("node %q has no resolved tool", node.ID)
		}
		form := func(sandboxPath string) string {
			if in.Form == nil {
				return sandboxPath
			}
			return in.Form(node.ID, sandboxPath)
		}

		for _, row := range in.Samples.Rows {
			segment := SampleSegment(row.Index, row.ID)
			params := map[string]any{}
			var missing []string

			for _, input := range manifest.Interface.Inputs {
				key := node.ID + "." + input.Name

				// 1. an edge from an upstream node's output
				if b, ok := boundTo[key]; ok {
					upstream, _, outName, _ := parseAddr(b.From)
					params[input.Name] = form(SandboxOutput(in.RunID, upstream, segment, outName))
					continue
				}
				// 2. an exposure: the table, the user, or this node's own output
				if e, ok := exposed[key]; ok {
					switch {
					case e.SampleField() != "":
						value, present := row.Values[e.SampleField()]
						if !present {
							missing = append(missing, fmt.Sprintf("%s ← sample.%s (no such column)", key, e.SampleField()))
							continue
						}
						coerced, err := coerce(input, value)
						if err != nil {
							return nil, fmt.Errorf("%s: %v", key, err)
						}
						params[input.Name] = coerced
					case e.From == FromUser:
						value, ok := userParamFor(node.ID, input.Name)
						if !ok {
							if input.Required {
								missing = append(missing, fmt.Sprintf("%s ← user (pass --param %s=<value>)", key, input.Name))
							}
							continue
						}
						coerced, err := coerce(input, value)
						if err != nil {
							return nil, fmt.Errorf("%s: %v", key, err)
						}
						params[input.Name] = coerced
					case e.OutputName() != "":
						outName := e.OutputName()
						if _, ok := findOutput(manifest, outName); !ok {
							return nil, fmt.Errorf("expose %s: node %q has no output %q", key, node.ID, outName)
						}
						params[input.Name] = form(SandboxOutput(in.RunID, node.ID, segment, outName))
					default:
						return nil, fmt.Errorf("expose %s: unknown source %q", key, e.From)
					}
					continue
				}
				// 3. nothing: the tool's own default applies (job.Validate is
				// the authority on whether that is enough).
			}
			if len(missing) > 0 {
				return nil, fmt.Errorf("run %s node %q sample %s: %s",
					in.RunID, node.ID, segment, strings.Join(missing, "; "))
			}

			outputs := make([]string, 0, len(manifest.Interface.Outputs))
			for _, out := range manifest.Interface.Outputs {
				outputs = append(outputs, form(SandboxOutput(in.RunID, node.ID, segment, out.Name)))
			}

			units = append(units, Unit{
				Node: node.ID, ToolID: manifest.ID, Tool: manifest, Sample: row, Segment: segment,
				Params: params, Outputs: outputs,
				Tags: map[string]string{
					"flow":   in.Flow.ID,
					"run":    in.RunID,
					"node":   node.ID,
					"sample": row.ID,
				},
			})
		}
	}
	return units, nil
}

// coerce turns a table value into the type the input declares.
//
// A sample table is text, and a tool with `type: int` should receive a number —
// otherwise the value fails the tool's own type check and the operator gets a
// confusing message about the wrong thing.
func coerce(in tool.Input, value string) (any, error) {
	switch in.Type {
	case tool.TypeInt:
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("param %s: %q is not an integer", in.Name, value)
		}
		return n, nil
	case tool.TypeFloat:
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return nil, fmt.Errorf("param %s: %q is not a number", in.Name, value)
		}
		return f, nil
	case tool.TypeBool:
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("param %s: %q is not a boolean", in.Name, value)
		}
		return b, nil
	default:
		return value, nil
	}
}

// UnitsForNode returns the planned units of one node, in sample order.
func UnitsForNode(units []Unit, nodeID string) []Unit {
	var out []Unit
	for _, u := range units {
		if u.Node == nodeID {
			out = append(out, u)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sample.Index < out[j].Sample.Index })
	return out
}

// NodeIDs returns the planned node ids in dependency order (the order the units
// appear in), which is also the order the runner submits them.
func NodeIDs(units []Unit) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range units {
		if !seen[u.Node] {
			seen[u.Node] = true
			out = append(out, u.Node)
		}
	}
	return out
}

// UserParamNames lists the exposed inputs a user must supply by name, for help
// text and for a clear "what do I need to pass" message.
func (f *Flow) UserParamNames() []string {
	var out []string
	for _, e := range f.Expose {
		if e.From == FromUser {
			out = append(out, e.Node+"."+e.Input)
		}
	}
	sort.Strings(out)
	return out
}

// OutputParamNames lists the exposed inputs wired to the node's own outputs
// (the "tell the tool where to write" cases).
func (f *Flow) OutputParamNames() []string {
	var out []string
	for _, e := range f.Expose {
		if e.OutputName() != "" {
			out = append(out, e.Node+"."+e.Input+" ← "+e.From)
		}
	}
	sort.Strings(out)
	return out
}
