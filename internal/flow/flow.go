// Package flow is the orchestration contract: a DAG of *registered tools*, plus
// the validation that makes it instantiable without anyone writing a command.
//
// The division of labour is ADR-009's, and it is why this package has no
// executor:
//
//   - a tool developer writes a function body (`work.sh`) and its type signature
//     (`interface`) — one step, one tool;
//   - a flow administrator writes how the steps are *strung together* —
//     nodes, dependencies, and which output feeds which input;
//   - a user fills the exposed parameters and a sample table.
//
// So a flow contains no shell, no `${}` templating, and no backend: those are
// properties of the tools it references. What it contains is wiring, and wiring
// is what this package checks — statically, at registration, so that a flow
// that validates is guaranteed to expand into runnable jobs.
//
// The schema is deliberately aligned with annopi's pipeline.yml (ADR-010):
// nodes ↔ tasks, depends_on ↔ dependencies, expose ↔ the sample/config
// references. The vocabulary changed; the shape did not.
package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/srcos/internal/tool"
)

// SchemaVersion is the only flow schema version SRCOS understands.
const SchemaVersion = 1

// When values: whether a node runs after a failed upstream.
const (
	WhenOnSuccess = "on_success"
	WhenAlways    = "always"
)

// MaxRetry bounds a node's automatic retries. A retry loop that can grow without
// bound is how a broken tool eats a login node for an afternoon.
const MaxRetry = 5

// ExposeSource values: where a parameter's value comes from.
const (
	// FromUser is a value the user fills once, shared by every sample.
	FromUser = "user"
	// FromSamplePrefix marks a value that comes from a sample-table column:
	// "sample.<column>".
	FromSamplePrefix = "sample."
	// FromOutputPrefix marks a value that is the path of one of the node's own
	// declared outputs: "output.<name>".
	//
	// It exists because a tool that writes somewhere must be *told* where, and
	// the platform owns flow paths (see layout.go). A binding passes an upstream
	// output downstream; this passes a node's own output into its parameter —
	// which is how the same wire works at both ends without any path templating.
	FromOutputPrefix = "output."
)

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	verRe  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
	addrRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+\.(inputs|outputs)\.[a-zA-Z0-9_-]+$`)
)

// Flow is the parsed flow.yaml.
type Flow struct {
	SchemaVersion int    `yaml:"schemaVersion" json:"schemaVersion"`
	ID            string `yaml:"id" json:"id"`
	Version       string `yaml:"version" json:"version"`
	Name          string `yaml:"name" json:"name"`
	Description   string `yaml:"description,omitempty" json:"description,omitempty"`

	Nodes    []Node    `yaml:"nodes" json:"nodes"`
	Bindings []Binding `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	Expose   []Expose  `yaml:"expose,omitempty" json:"expose,omitempty"`

	// Dir is the flow package directory this file was loaded from. It is not
	// part of flow.yaml.
	Dir string `yaml:"-" json:"-"`
}

// Node is one step: a registered tool, run once per sample row.
type Node struct {
	// ID addresses the node in bindings and expose. It may not contain ".":
	// addresses are dotted, so a node id with a dot would be ambiguous.
	ID   string `yaml:"id" json:"id"`
	Tool string `yaml:"tool" json:"tool"`
	// DependsOn is an AND: the node is submitted once every upstream succeeded
	// (or, for when: always, once every upstream reached a terminal state).
	DependsOn []string `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	// When is "on_success" (default) or "always".
	When string `yaml:"when,omitempty" json:"when,omitempty"`
	// Retry bounds automatic retries of this node.
	Retry *Retry `yaml:"retry,omitempty" json:"retry,omitempty"`
}

// Retry is a node's retry policy.
type Retry struct {
	Max int `yaml:"max" json:"max"`
}

// WhenOr returns the effective `when` value.
func (n Node) WhenOr() string {
	if n.When == "" {
		return WhenOnSuccess
	}
	return n.When
}

// RetryMax returns the effective retry bound.
func (n Node) RetryMax() int {
	if n.Retry == nil {
		return 0
	}
	return n.Retry.Max
}

// Binding connects an upstream output to a downstream input.
type Binding struct {
	From string `yaml:"from" json:"from"` // <node>.outputs.<name>
	To   string `yaml:"to" json:"to"`     // <node>.inputs.<name>
}

// Expose marks an input the user (or the sample table) provides.
type Expose struct {
	Node  string `yaml:"node" json:"node"`
	Input string `yaml:"input" json:"input"`
	// From is "user" or "sample.<column>".
	From string `yaml:"from" json:"from"`
}

// SampleField returns the sample-table column this exposure reads, or "" when
// it is a one-off user value.
func (e Expose) SampleField() string {
	if strings.HasPrefix(e.From, FromSamplePrefix) {
		return strings.TrimPrefix(e.From, FromSamplePrefix)
	}
	return ""
}

// OutputName returns the node output this exposure points at, or "".
func (e Expose) OutputName() string {
	if strings.HasPrefix(e.From, FromOutputPrefix) {
		return strings.TrimPrefix(e.From, FromOutputPrefix)
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────
// 加载与发现
// ─────────────────────────────────────────────────────────────────────────

// Load reads and validates one flow.yaml.
func Load(path string) (*Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Flow
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.Dir = filepath.Dir(path)
	return &f, nil
}

// Discover loads every flow under a directory (one level: <dir>/<id>/flow.yaml),
// sorted by id.
//
// A malformed flow is reported rather than skipped: a flow that silently
// disappeared from the list is worse than one that fails loudly, because the
// operator's next question is "why is my flow not there".
func Discover(dir string) ([]*Flow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Flow
	var problems []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "flow.yaml")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		f, err := Load(path)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if f.ID != e.Name() {
			problems = append(problems, fmt.Sprintf("%s: id %q does not match the directory name %q",
				path, f.ID, e.Name()))
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(problems) > 0 {
		return out, fmt.Errorf("%d flow problem(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return out, nil
}

// Find loads one flow by id.
func Find(dir, id string) (*Flow, error) {
	if !idRe.MatchString(id) {
		return nil, fmt.Errorf("invalid flow id %q", id)
	}
	path := filepath.Join(dir, id, "flow.yaml")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no flow %s under %s", id, dir)
	}
	return Load(path)
}

// ValidateAgainst registers the static half of a flow's meaning: the graph, the
// wiring, and the tools it references.
//
// `resolve` returns a tool manifest by id (and reports a version mismatch), and
// is injected so this package does not have to know where tool packages live —
// the CLI and the gateway pass the same lookup.
func (f *Flow) ValidateAgainst(resolve func(id, version string) (*tool.Tool, error)) error {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	// ── 结构 ──────────────────────────────────────────────────────────
	if f.SchemaVersion != SchemaVersion {
		bad("schemaVersion must be %d, got %d", SchemaVersion, f.SchemaVersion)
	}
	if !idRe.MatchString(f.ID) {
		bad("id must match %s, got %q", idRe, f.ID)
	}
	if strings.TrimSpace(f.Version) == "" {
		bad("version is required")
	} else if !isVersionLike(f.Version) {
		bad("version %q should look like 0.1.0", f.Version)
	}
	if strings.TrimSpace(f.Name) == "" {
		bad("name is required")
	}
	if len(f.Nodes) == 0 {
		bad("a flow needs at least one node")
	}

	// ── 节点 ──────────────────────────────────────────────────────────
	nodes := map[string]*Node{}
	for i := range f.Nodes {
		n := &f.Nodes[i]
		where := fmt.Sprintf("nodes[%d]", i)
		if n.ID != "" {
			where = fmt.Sprintf("node %q", n.ID)
		}
		if !idRe.MatchString(n.ID) {
			bad("%s: id must match %s", where, idRe)
		}
		// Addresses are dotted (`count.outputs.outs`), so a node id containing a
		// dot could never be addressed unambiguously.
		if strings.Contains(n.ID, ".") {
			bad("%s: id may not contain '.' (bindings address nodes as <id>.<inputs|outputs>.<name>)", where)
		}
		if _, dup := nodes[n.ID]; dup {
			bad("%s: duplicate node id", where)
		}
		nodes[n.ID] = n
		if strings.TrimSpace(n.Tool) == "" {
			bad("%s: tool is required", where)
		}
		switch n.WhenOr() {
		case WhenOnSuccess, WhenAlways:
		default:
			bad("%s: when must be %q or %q, got %q", where, WhenOnSuccess, WhenAlways, n.When)
		}
		if max := n.RetryMax(); n.Retry != nil && (max < 0 || max > MaxRetry) {
			bad("%s: retry.max must be between 0 and %d, got %d", where, MaxRetry, max)
		}
	}
	for _, n := range f.Nodes {
		seen := map[string]bool{}
		for _, dep := range n.DependsOn {
			if dep == n.ID {
				bad("node %q depends on itself", n.ID)
				continue
			}
			if seen[dep] {
				bad("node %q lists %q twice in depends_on", n.ID, dep)
				continue
			}
			seen[dep] = true
			if _, ok := nodes[dep]; !ok {
				bad("node %q depends on unknown node %q", n.ID, dep)
			}
		}
	}
	if cycle := findCycle(f); len(cycle) > 0 {
		bad("dependency cycle: %s", strings.Join(cycle, " → "))
	}

	// ── 节点引用的工具 ─────────────────────────────────────────────────
	manifests := map[string]*tool.Tool{}
	for _, n := range f.Nodes {
		if strings.TrimSpace(n.Tool) == "" {
			continue
		}
		id, version := splitToolRef(n.Tool)
		t, err := resolve(id, version)
		if err != nil {
			bad("node %q: %v", n.ID, err)
			continue
		}
		if t.Kind != tool.KindTask {
			// A service is a long-lived instance with a port and a lifecycle; a
			// DAG node is "run to completion". Mixing them would need a second
			// meaning for "the node is done".
			bad("node %q: tool %s is kind %s — flows may only reference kind: task tools", n.ID, t.ID, t.Kind)
		}
		manifests[n.ID] = t
	}

	// ── 连线 ──────────────────────────────────────────────────────────
	boundInput := map[string]string{} // "<node>.<input>" -> binding.from
	for i, b := range f.Bindings {
		where := fmt.Sprintf("bindings[%d]", i)
		fromNode, fromKind, fromName, ok := parseAddr(b.From)
		if !ok || fromKind != "outputs" {
			bad("%s: from must be <node>.outputs.<name>, got %q", where, b.From)
			continue
		}
		toNode, toKind, toName, ok := parseAddr(b.To)
		if !ok || toKind != "inputs" {
			bad("%s: to must be <node>.inputs.<name>, got %q", where, b.To)
			continue
		}

		upManifest := manifests[fromNode]
		if !nodeExists(f, fromNode) {
			bad("%s: from refers to unknown node %q", where, fromNode)
			continue
		}
		if upManifest == nil {
			continue // the node exists but its tool failed validation above
		}
		downManifest := manifests[toNode]
		if !nodeExists(f, toNode) {
			bad("%s: to refers to unknown node %q", where, toNode)
			continue
		}
		if downManifest == nil {
			continue
		}

		out, ok := findOutput(upManifest, fromName)
		if !ok {
			bad("%s: node %q has no output %q", where, fromNode, fromName)
			continue
		}
		in, ok := findInput(downManifest, toName)
		if !ok {
			bad("%s: node %q has no input %q", where, toNode, toName)
			continue
		}
		key := toNode + "." + toName
		if other, dup := boundInput[key]; dup {
			bad("%s: input %s.%s is already fed by %s (an input takes one source)", where, toNode, toName, other)
			continue
		}
		boundInput[key] = b.From

		if err := compatible(out.Type, in); err != nil {
			bad("%s: %s.%s (%s) → %s.%s: %v", where, fromNode, fromName, out.Type, toNode, toName, err)
		}
		// A wire carries the upstream's *output path*, which only exists once the
		// upstream has run. So a binding is also an ordering statement: without a
		// dependency the downstream could start first and read a path that is not
		// there yet. (The dependency may be transitive — an intermediate step in
		// between does not remove the file.)
		if !dependsTransitively(f, toNode, fromNode) {
			bad("%s: %s reads %s's output but does not depend on it — add %q to %s's depends_on",
				where, toNode, fromNode, fromNode, toNode)
		}
	}

	// ── 暴露 ──────────────────────────────────────────────────────────
	exposedInput := map[string]string{}
	for i, e := range f.Expose {
		where := fmt.Sprintf("expose[%d]", i)
		n, manifest := manifestOf(manifests, f, e.Node)
		if n == nil {
			bad("%s: unknown node %q", where, e.Node)
			continue
		}
		if _, ok := findInput(manifest, e.Input); !ok {
			bad("%s: node %q has no input %q", where, e.Node, e.Input)
			continue
		}
		key := e.Node + "." + e.Input
		if other, dup := exposedInput[key]; dup {
			bad("%s: input %s.%s is already exposed by %s", where, e.Node, e.Input, other)
			continue
		}
		if from, bound := boundInput[key]; bound {
			bad("%s: input %s.%s is already fed by binding %s", where, e.Node, e.Input, from)
			continue
		}
		switch {
		case e.From == FromUser, e.SampleField() != "":
		case e.OutputName() != "":
			// A node may only be handed paths of outputs it actually declares:
			// otherwise the value would name a directory nothing produces.
			if _, ok := findOutput(manifest, e.OutputName()); !ok {
				bad("%s: node %q has no output %q", where, e.Node, e.OutputName())
				continue
			}
		default:
			bad("%s: from must be %q, %q or %q, got %q",
				where, FromUser, FromSamplePrefix+"<field>", FromOutputPrefix+"<name>", e.From)
			continue
		}
		exposedInput[key] = e.From
	}

	// ── 闭环：每个 required 输入有且仅有一个来源 ─────────────────────
	for _, n := range f.Nodes {
		manifest := manifests[n.ID]
		if manifest == nil {
			continue // already reported
		}
		for _, in := range manifest.Interface.Inputs {
			if !in.Required {
				continue
			}
			key := n.ID + "." + in.Name
			_, bound := boundInput[key]
			_, exposed := exposedInput[key]
			if bound && exposed {
				// Both maps already refuse a second source, so this can only
				// happen across the two lists — reported here for a clear message.
				bad("input %s is provided by both a binding and expose(from: %s)", key, exposedInput[key])
				continue
			}
			if !bound && !exposed {
				if in.Default != nil {
					// A tool-level default feeds it — exactly as it does for a
					// plain job, where job.Validate judges satisfiability on the
					// *effective* params. Demanding a source the tool already has
					// would make a flow unable to use its own tools' defaults.
					continue
				}
				bad("required input %s has no source: add a binding, an expose entry, or give the tool a default", key)
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("flow %s: %d problem(s):\n  - %s", f.ID, len(problems),
			strings.Join(problems, "\n  - "))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// 图
// ─────────────────────────────────────────────────────────────────────────

// Order returns the nodes in topological order (dependencies first), with ties
// broken by id so the result is stable.
//
// A cycle is impossible in a validated flow; an unvalidated one gets a partial
// order rather than a hang. Note the graph has to be built from the *complete*
// node set first: computing in-degrees while the set is still growing silently
// treats a cycle as a chain.
func (f *Flow) Order() []Node {
	// Ensure the node map is complete before any in-degree is computed.
	byID := make(map[string]Node, len(f.Nodes))
	present := make(map[string]bool, len(f.Nodes))
	for _, n := range f.Nodes {
		byID[n.ID] = n
		present[n.ID] = true
	}

	indegree := make(map[string]int, len(f.Nodes))
	dependents := make(map[string][]string, len(f.Nodes))
	for _, n := range f.Nodes {
		if _, seen := indegree[n.ID]; !seen {
			indegree[n.ID] = 0
		}
		seenDep := map[string]bool{}
		for _, dep := range n.DependsOn {
			// An unknown dependency cannot be waited for; a duplicate would
			// break the in-degree bookkeeping below.
			if !present[dep] || dep == n.ID || seenDep[dep] {
				continue
			}
			seenDep[dep] = true
			indegree[n.ID]++
			dependents[dep] = append(dependents[dep], n.ID)
		}
	}

	var out []Node
	emitted := map[string]bool{}
	for len(out) < len(f.Nodes) {
		var ready []string
		for id, deg := range indegree {
			if !emitted[id] && deg == 0 {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			break // a cycle: return the partial order rather than looping
		}
		sort.Strings(ready)
		for _, id := range ready {
			out = append(out, byID[id])
			emitted[id] = true
			for _, next := range dependents[id] {
				indegree[next]--
			}
		}
	}
	return out
}

// Depth returns how many layers the DAG has (1 for a single node), which is what
// a canvas renders as columns.
func (f *Flow) Depth() int {
	level := map[string]int{}
	best := 0
	for _, n := range f.Order() {
		depth := 1
		for _, dep := range n.DependsOn {
			if l, ok := level[dep]; ok && l+1 > depth {
				depth = l + 1
			}
		}
		level[n.ID] = depth
		if depth > best {
			best = depth
		}
	}
	return best
}

// SampleFields lists the sample-table columns this flow reads, sorted.
func (f *Flow) SampleFields() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range f.Expose {
		field := e.SampleField()
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

// dependsTransitively reports whether `node` depends on `target`, directly or
// through other nodes.
//
// A cycle cannot appear in a validated flow, but this walks with a visited set
// anyway: it also runs while the graph is still being validated, where a cycle
// is exactly the kind of thing being looked for.
func dependsTransitively(f *Flow, node, target string) bool {
	if node == target {
		return false
	}
	deps := map[string][]string{}
	for _, n := range f.Nodes {
		deps[n.ID] = n.DependsOn
	}
	seen := map[string]bool{node: true}
	queue := append([]string{}, deps[node]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == target {
			return true
		}
		if seen[next] {
			continue
		}
		seen[next] = true
		queue = append(queue, deps[next]...)
	}
	return false
}

// findCycle returns a cycle as a node path, or nil.
func findCycle(f *Flow) []string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	edges := map[string][]string{}
	for _, n := range f.Nodes {
		edges[n.ID] = n.DependsOn
	}

	var stack []string
	var cycle []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = grey
		stack = append(stack, id)
		for _, dep := range edges[id] {
			if _, known := edges[dep]; !known {
				continue
			}
			switch color[dep] {
			case grey:
				// Found the back edge: the cycle is from dep's position in the
				// stack to here.
				for i, s := range stack {
					if s == dep {
						cycle = append(append([]string{}, stack[i:]...), dep)
						return true
					}
				}
				cycle = []string{dep, id, dep}
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	for _, n := range f.Nodes {
		if color[n.ID] == white {
			if visit(n.ID) {
				return cycle
			}
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// 小工具
// ─────────────────────────────────────────────────────────────────────────

// splitToolRef splits "id@version" into its parts (version may be empty).
func splitToolRef(ref string) (id, version string) {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return strings.TrimSpace(ref[:i]), strings.TrimSpace(ref[i+1:])
	}
	return strings.TrimSpace(ref), ""
}

// parseAddr parses `<node>.<inputs|outputs>.<name>`.
func parseAddr(addr string) (node, kind, name string, ok bool) {
	if !addrRe.MatchString(addr) {
		return "", "", "", false
	}
	parts := strings.SplitN(addr, ".", 3)
	return parts[0], parts[1], parts[2], true
}

// manifestOf resolves a node and its manifest by node id.
func manifestOf(manifests map[string]*tool.Tool, f *Flow, nodeID string) (*Node, *tool.Tool) {
	for i := range f.Nodes {
		if f.Nodes[i].ID == nodeID {
			return &f.Nodes[i], manifests[nodeID]
		}
	}
	return nil, nil
}

// nodeExists reports whether a node id is declared (independent of whether its
// tool resolved).
func nodeExists(f *Flow, nodeID string) bool {
	for i := range f.Nodes {
		if f.Nodes[i].ID == nodeID {
			return true
		}
	}
	return false
}

func findInput(t *tool.Tool, name string) (tool.Input, bool) {
	if t == nil {
		return tool.Input{}, false
	}
	for _, in := range t.Interface.Inputs {
		if in.Name == name {
			return in, true
		}
	}
	return tool.Input{}, false
}

func findOutput(t *tool.Tool, name string) (tool.Output, bool) {
	if t == nil {
		return tool.Output{}, false
	}
	for _, out := range t.Interface.Outputs {
		if out.Name == name {
			return out, true
		}
	}
	return tool.Output{}, false
}

// compatible reports whether an upstream output type may feed an input.
//
// Only paths travel between nodes: SRCOS does not parse or transform business
// data (ADR-005/006), so an output can never satisfy a scalar input. That is a
// feature: "how do I get this number into the next step?" has exactly one
// answer — the upstream tool writes a file and the downstream reads the path.
func compatible(out tool.OutputType, in tool.Input) error {
	switch in.Type {
	case tool.TypeFile:
		if out != tool.OutFile {
			return fmt.Errorf("a %s output cannot fill a file input", out)
		}
		return nil
	case tool.TypeDirectory, tool.TypeDirPath:
		if out != tool.OutDirectory {
			return fmt.Errorf("a %s output cannot fill a %s input", out, in.Type)
		}
		return nil
	case tool.TypePath:
		switch in.Select {
		case "file":
			if out != tool.OutFile {
				return fmt.Errorf("a %s output cannot fill a path input with select: file", out)
			}
		case "directory":
			if out != tool.OutDirectory {
				return fmt.Errorf("a %s output cannot fill a path input with select: directory", out)
			}
		}
		return nil
	default:
		return fmt.Errorf("a %s output cannot fill a %s input: only paths travel between nodes "+
			"(have the upstream tool write a file and read it downstream)", out, in.Type)
	}
}

func isVersionLike(v string) bool { return verRe.MatchString(strings.TrimSpace(v)) }
