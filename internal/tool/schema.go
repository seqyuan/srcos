package tool

import (
	"fmt"
	"strings"
)

// This file is the first of ADR-018's three derivations: the interface rendered
// as a JSON Schema, which is the form a tool-calling model (and the MCP client
// driving it) understands.
//
// It lives next to the contract rather than in the MCP layer because the
// interface is the source of truth: the canvas's wire types and the generated
// form are derived from the same fields, and a schema built anywhere else would
// be a fourth opinion about what a tool accepts.

// JSONSchema renders the interface as a JSON Schema object describing a tool
// call's arguments.
//
// The mapping is deliberately conservative — the types are the frozen set of
// ADR-018, so this switch is exhaustive by construction, and anything unknown
// degrades to a plain string rather than to a permissive "any".
func (i Interface) JSONSchema() map[string]any {
	props := map[string]any{}
	var required []string
	for _, in := range i.Inputs {
		if strings.TrimSpace(in.Name) == "" {
			continue
		}
		props[in.Name] = inputSchema(in)
		if in.Required {
			required = append(required, in.Name)
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// inputSchema renders one input.
func inputSchema(in Input) map[string]any {
	out := map[string]any{}
	switch in.Type {
	case TypeInt:
		out["type"] = "integer"
	case TypeFloat:
		out["type"] = "number"
	case TypeBool:
		out["type"] = "boolean"
	case TypeEnum:
		out["type"] = "string"
		if len(in.Values) > 0 {
			out["enum"] = in.Values
		}
	case TypeFile, TypeDirectory, TypeDirPath, TypePath:
		// A path parameter is a string in the *sandbox's* path space: the value
		// a caller passes is the value the tool receives (ADR-020). Where the
		// value may come from belongs in the description, because a caller
		// cannot discover that from the type alone.
		out["type"] = "string"
		desc := pathDescription(in)
		if human := descriptionOf(in); human != "" {
			desc += " — " + human
		}
		out["description"] = desc
	default:
		out["type"] = "string"
		if d := descriptionOf(in); d != "" {
			out["description"] = d
		}
	}

	if in.Min != nil || in.Max != nil {
		// Integer/number bounds. A string parameter with bounds is a manifest
		// mistake the validator already rejected, so this is safe to apply.
		if in.Type == TypeInt || in.Type == TypeFloat {
			if in.Min != nil {
				out["minimum"] = *in.Min
			}
			if in.Max != nil {
				out["maximum"] = *in.Max
			}
		}
	}
	if in.Default != nil {
		out["default"] = in.Default
	}
	return out
}

// pathDescription explains a path parameter in the terms a caller can act on:
// which storage roots it may select from, and whether it wants a file or a
// directory.
func pathDescription(in Input) string {
	var b strings.Builder
	b.WriteString("Sandbox path")
	switch in.Type {
	case TypeFile:
		b.WriteString(" to a file")
	case TypeDirectory, TypeDirPath:
		b.WriteString(" to a directory")
	case TypePath:
		switch in.Select {
		case "file":
			b.WriteString(" to a file")
		case "directory":
			b.WriteString(" to a directory")
		}
	}
	if roots := splitFrom(in.From); len(roots) > 0 {
		fmt.Fprintf(&b, "; list it with srcos_list_paths (storages: %s)", strings.Join(roots, ", "))
	}
	if len(in.Files) > 0 {
		fmt.Fprintf(&b, "; must contain %s", strings.Join(in.Files, ", "))
	}
	return b.String()
}

// descriptionOf is the human-facing text for an input: the authored description
// if there is one, else the label.
func descriptionOf(in Input) string {
	if strings.TrimSpace(in.Description) != "" {
		return in.Description
	}
	return in.Label
}
