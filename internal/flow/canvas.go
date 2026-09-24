package flow

import (
	"fmt"
	"math"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the console's half of a flow: where the administrator put the
// nodes, and which inputs still need a source.
//
// Neither is part of the contract. A flow's shape *is* its topology (ADR-010),
// node coordinates are a UI concern, and putting a mouse's output into flow.yaml
// would put a UI concern into a hand-editable document (ADR-023). So they live
// here as a sidecar file and a derivation, and the CLI and the scheduler never
// look at either.

// Layout is a flow's hand-placed node coordinates.
type Layout struct {
	// Nodes maps a node id to its top-left corner on the canvas.
	Nodes map[string]Point `yaml:"nodes" json:"nodes"`
}

// Point is one node's position, in canvas units.
type Point struct {
	X float64 `yaml:"x" json:"x"`
	Y float64 `yaml:"y" json:"y"`
}

// LayoutPath is the sidecar's path: beside flow.yaml, in the flow's own
// directory. A separate file is what keeps flow.yaml diffable and hand-writable.
func LayoutPath(flowsDir, id string) string {
	return filepath.Join(flowsDir, id, "layout.yaml")
}

// LoadLayout reads a flow's saved coordinates, keeping only nodes the flow still
// has.
//
// A missing or unreadable file means "no saved layout", never an error: the
// canvas then derives positions from the topology, which is also how a flow typed
// by hand renders. The filter matters because a flow can be edited outside the
// console — a removed node's old position must not come back.
func LoadLayout(flowsDir, id string, known []string) Layout {
	out := Layout{Nodes: map[string]Point{}}
	data, err := os.ReadFile(LayoutPath(flowsDir, id))
	if err != nil {
		return out
	}
	var stored Layout
	if err := yaml.Unmarshal(data, &stored); err != nil {
		return out
	}
	keep := make(map[string]bool, len(known))
	for _, nodeID := range known {
		keep[nodeID] = true
	}
	for nodeID, p := range stored.Nodes {
		if keep[nodeID] {
			out.Nodes[nodeID] = p
		}
	}
	return out
}

// maxCoordinate bounds a saved position. A canvas that somehow receives a
// coordinate near a float's limit would render an SVG the browser cannot lay
// out; clamping keeps the file drawable without failing a drag.
const maxCoordinate = 100000

// SaveLayout writes the coordinates atomically, keeping only known nodes.
func SaveLayout(flowsDir, id string, l Layout, known []string) error {
	keep := make(map[string]bool, len(known))
	for _, nodeID := range known {
		keep[nodeID] = true
	}
	pruned := Layout{Nodes: map[string]Point{}}
	for nodeID, p := range l.Nodes {
		if !keep[nodeID] {
			continue
		}
		// NaN/Inf are not positions: they would become "NaN" in the SVG
		// transform and make every node vanish with no error anywhere.
		if math.IsNaN(p.X) || math.IsInf(p.X, 0) || math.IsNaN(p.Y) || math.IsInf(p.Y, 0) {
			return fmt.Errorf("layout for node %s is not a finite position", nodeID)
		}
		p.X = clamp(p.X, -maxCoordinate, maxCoordinate)
		p.Y = clamp(p.Y, -maxCoordinate, maxCoordinate)
		pruned.Nodes[nodeID] = p
	}
	data, err := yaml.Marshal(pruned)
	if err != nil {
		return err
	}
	path := LayoutPath(flowsDir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Atomic, like every other record here: a half-written file would make the
	// canvas fall back to the derived layout, which looks like "my positions
	// disappeared" and is impossible to debug.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MissingExpose names the required inputs of a flow's nodes that nothing feeds
// yet, each suggesting the conventional `sample.<input>` source.
//
// It is the complement of the validation rule ("every required input has exactly
// one source"), written next to it so the two cannot drift: what the canvas
// offers to fill in is exactly what validation would otherwise refuse. The
// administrator still confirms each one — the suggested source is a convention
// (a sample table column named after the input), not a fact.
func MissingExpose(f *Flow, resolve func(id, version string) (*tool.Tool, error)) []Expose {
	bound := make(map[string]bool, len(f.Bindings))
	for _, b := range f.Bindings {
		// The key shape is the validator's ("<node>.<input>", parsed from the
		// address) — mirrors, not re-invents, what validation matched against.
		if node, kind, name, ok := parseAddr(b.To); ok && kind == "inputs" {
			bound[node+"."+name] = true
		}
	}
	exposed := make(map[string]bool, len(f.Expose))
	for _, e := range f.Expose {
		exposed[e.Node+"."+e.Input] = true
	}

	var out []Expose
	for _, n := range f.Nodes {
		id, version := splitToolRef(n.Tool)
		manifest, err := resolve(id, version)
		if err != nil || manifest == nil {
			continue // an unknown tool is validation's problem, not this list's
		}
		for _, in := range manifest.Interface.Inputs {
			if !in.Required || in.Default != nil {
				continue // optional, or already fed by the tool's own default
			}
			key := n.ID + "." + in.Name
			if bound[key] || exposed[key] {
				continue
			}
			out = append(out, Expose{Node: n.ID, Input: in.Name, From: FromSamplePrefix + in.Name})
		}
	}
	return out
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
