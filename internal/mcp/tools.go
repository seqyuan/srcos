package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/seqyuan/srcos/internal/inspect"
)

// This file is the read-only tool surface of ADR-019, in the spirit of ADR-018:
// the schemas are written once here, and every handler answers from package
// inspect — the same code the REST API uses. Nothing in this file talks to the
// filesystem directly.
//
// Naming: every tool is prefixed `srcos_` so an agent connected to several MCP
// servers can tell whose tool it is calling, and so a name collision with
// another server cannot silently shadow one of these.

// tool is one exposed MCP tool.
type tool struct {
	name        string
	title       string
	description string
	inputSchema map[string]any
	call        func(ctx context.Context, env env, args map[string]any) (any, error)
}

// env is what a handler is allowed to know: who is asking, and the read side.
type env struct {
	user   string
	reader *inspect.Reader
}

// readTools is the catalogue, in the order it is listed. All of them are
// read-only, which is why none of them takes a confirmation flag or a dry-run
// switch: there is nothing to confirm.
func readTools() []tool {
	return []tool{
		{
			name:  "srcos_list_tools",
			title: "List tools",
			description: "List the SRCOS tools this user may use. Each entry is the tool's id, kind, backend and " +
				"interface (its parameter names and types). Use srcos_describe_tool for one tool's full interface.",
			inputSchema: objectSchema(map[string]any{}, nil),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				views, err := e.reader.Tools(e.user)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(views))
				for _, v := range views {
					names := make([]string, 0, len(v.Interface.Inputs))
					for _, in := range v.Interface.Inputs {
						names = append(names, in.Name)
					}
					out = append(out, map[string]any{
						"id":          v.ID,
						"version":     v.Version,
						"name":        v.Name,
						"description": v.Description,
						"kind":        v.Kind,
						"backend":     v.Backend,
						"inputs":      names,
					})
				}
				return map[string]any{"tools": out}, nil
			},
		},
		{
			name:  "srcos_describe_tool",
			title: "Describe a tool",
			description: "Describe one tool: its full interface (inputs and outputs), the resources it declares, " +
				"and the storages its path parameters may select from. The `inputSchema` field is the same " +
				"signature rendered as JSON Schema, which is the form a model can use directly.",
			inputSchema: objectSchema(map[string]any{
				"tool": stringProp("Tool id, as listed by srcos_list_tools"),
			}, []string{"tool"}),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				id, err := requireString(args, "tool")
				if err != nil {
					return nil, err
				}
				v, err := e.reader.Tool(e.user, id)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"id":          v.ID,
					"version":     v.Version,
					"name":        v.Name,
					"description": v.Description,
					"kind":        v.Kind,
					"backend":     v.Backend,
					"sandbox":     v.Sandbox,
					"entry":       v.Entry,
					"resources":   v.Resources,
					"interface":   v.Interface,
					"inputSchema": v.Interface.JSONSchema(),
					"storages":    v.Storages,
				}, nil
			},
		},
		{
			name:  "srcos_list_storages",
			title: "List storages",
			description: "List the shared data roots this user's tools declare, in sandbox path space (the paths " +
				"tools actually receive). A storage only appears if one of this user's tools requires it.",
			inputSchema: objectSchema(map[string]any{}, nil),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				views, err := e.reader.StoragesForUser(e.user)
				if err != nil {
					return nil, err
				}
				return map[string]any{"storages": views}, nil
			},
		},
		{
			name:  "srcos_list_paths",
			title: "Browse a storage",
			description: "List one directory inside a storage, as sandbox paths. The storage is derived from the " +
				"tool and input you name — never chosen freely — so what you see here is exactly what the tool " +
				"can mount. Call it once per (tool, input) pair to find a value for a path parameter.",
			inputSchema: objectSchema(map[string]any{
				"tool":    stringProp("Tool id whose input declares the storage"),
				"input":   stringProp("Input (parameter) name of that tool"),
				"storage": stringProp("Optional: which declared storage, when one input lists several"),
				"path":    stringProp("Optional: sandbox path to list; defaults to the storage root"),
				"select":  stringProp("Optional: \"file\" or \"directory\" to filter entries"),
				"limit":   intProp("Optional: maximum entries (default 500)"),
			}, []string{"tool", "input"}),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				toolID, err := requireString(args, "tool")
				if err != nil {
					return nil, err
				}
				input, err := requireString(args, "input")
				if err != nil {
					return nil, err
				}
				listing, err := e.reader.Paths(ctx, e.user, inspect.PathRequest{
					Tool:    toolID,
					Input:   input,
					Storage: optString(args, "storage"),
					Path:    optString(args, "path"),
					Select:  optString(args, "select"),
					Limit:   optInt(args, "limit"),
				})
				if err != nil {
					return nil, err
				}
				return listing, nil
			},
		},
		{
			name:  "srcos_list_instances",
			title: "List instances",
			description: "List this user's instances (tasks and services), newest first, with state, exit code, " +
				"duration, endpoint and declared outputs.",
			inputSchema: objectSchema(map[string]any{
				"tool": stringProp("Optional: only this tool's instances"),
				"kind": stringProp("Optional: \"task\" or \"service\""),
			}, nil),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				views, err := e.reader.Instances(e.user, optString(args, "tool"), optString(args, "kind"))
				if err != nil {
					return nil, err
				}
				return map[string]any{"instances": views}, nil
			},
		},
		{
			name:  "srcos_task_status",
			title: "Instance status",
			description: "Report one instance: state, exit code, error, duration, endpoint, tags and the declared " +
				"outputs with whether they are actually there. Pass the full instance id, or a job id, or an " +
				"unambiguous suffix of either.",
			inputSchema: objectSchema(map[string]any{
				"instance": stringProp("Instance id from srcos_list_instances, or a job id"),
			}, []string{"instance"}),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				needle, err := requireString(args, "instance")
				if err != nil {
					return nil, err
				}
				inst, candidates, err := e.reader.Instance(e.user, needle)
				if err != nil {
					if len(candidates) > 0 {
						return nil, fmt.Errorf("%v (candidates: %s)", err, strings.Join(candidates, ", "))
					}
					return nil, err
				}
				arts, aerr := e.reader.Artifacts(e.user, inst.ID)
				res := map[string]any{"instance": inst}
				if aerr == nil {
					res["artifacts"] = arts
				} else {
					res["artifactsError"] = aerr.Error()
				}
				return res, nil
			},
		},
		{
			name:  "srcos_task_logs",
			title: "Instance logs",
			description: "Return the tail of an instance's log (stdout+stderr as SRCOS captured it). The log is " +
				"SRCOS's own copy, outside the workspace, so the tool cannot have rewritten it.",
			inputSchema: objectSchema(map[string]any{
				"instance": stringProp("Instance id or job id"),
				"tail":     intProp("Optional: lines to return (default 200, max 5000)"),
			}, []string{"instance"}),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				needle, err := requireString(args, "instance")
				if err != nil {
					return nil, err
				}
				text, err := e.reader.Logs(e.user, needle, optInt(args, "tail"))
				if err != nil {
					return nil, err
				}
				return map[string]any{"instance": needle, "log": text}, nil
			},
		},
		{
			name:  "srcos_list_artifacts",
			title: "List artifacts",
			description: "List the outputs a task instance declared, with existence, size and mtime. Only declared " +
				"outputs are reported — not whatever the tool happened to leave behind.",
			inputSchema: objectSchema(map[string]any{
				"instance": stringProp("Instance id or job id"),
			}, []string{"instance"}),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				needle, err := requireString(args, "instance")
				if err != nil {
					return nil, err
				}
				arts, err := e.reader.Artifacts(e.user, needle)
				if err != nil {
					return nil, err
				}
				return map[string]any{"artifacts": arts}, nil
			},
		},
		{
			name:  "srcos_read_file",
			title: "Read a text file",
			description: "Read a text file as a sandbox path. Readable: this user's home (/home/<user>/...), a " +
				"tool's workspace (/workspace/..., which is why `tool` is required for those paths), and the " +
				"storages this user's tools declare. Binary files and paths outside those mounts are refused.",
			inputSchema: objectSchema(map[string]any{
				"path":      stringProp("Sandbox path, e.g. /data/ref/genes.txt or /workspace/out/report.tsv"),
				"tool":      stringProp("Required when path is under /workspace: the tool that owns the workspace"),
				"max_bytes": intProp("Optional: cap the read (default 65536, max 262144)"),
			}, []string{"path"}),
			call: func(ctx context.Context, e env, args map[string]any) (any, error) {
				path, err := requireString(args, "path")
				if err != nil {
					return nil, err
				}
				content, err := e.reader.ReadFile(e.user, inspect.ReadRequest{
					Path:     path,
					Tool:     optString(args, "tool"),
					MaxBytes: int64(optInt(args, "max_bytes")),
				})
				if err != nil {
					return nil, err
				}
				return content, nil
			},
		},
	}
}

// listTools renders the catalogue for `tools/list`.
func (s *Server) listTools() map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]any{
			"name":        t.name,
			"title":       t.title,
			"description": t.description,
			"inputSchema": t.inputSchema,
			"annotations": map[string]any{
				// Every tool in this phase only reads; saying so lets a client
				// auto-approve them without weakening anything.
				"readOnlyHint": true,
			},
		})
	}
	return map[string]any{"tools": out}
}

// textResult is a tool result carrying rendered JSON (or an error message).
func textResult(text string, isError bool) map[string]any {
	res := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
	if isError {
		res["isError"] = true
	}
	return res
}

// render turns a tool's answer into the text an agent reads.
//
// JSON rather than prose: an agent that needs to act on a value (a path, an
// instance id) has to be able to copy it exactly, and a model reading JSON is
// reading the same contract the REST API serves.
func render(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(out)
}

// ─────────────────────────────────────────────────────────────────────────
// argument helpers
// ─────────────────────────────────────────────────────────────────────────

func objectSchema(props map[string]any, required []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func stringProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// requireString reads a mandatory non-empty string argument.
func requireString(args map[string]any, key string) (string, error) {
	v := optString(args, key)
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

// optString reads an optional string argument, rejecting a non-string rather
// than stringifying it: a number where a name belongs is a caller's bug, and
// coercing it would hide the bug behind a confusing answer.
func optString(args map[string]any, key string) string {
	raw, ok := args[key]
	if !ok || raw == nil {
		return ""
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// optInt reads an optional integer argument. JSON numbers arrive as float64, and
// a numeric string is accepted too — that coercion is unambiguous, unlike the
// reverse one.
func optInt(args map[string]any, key string) int {
	raw, ok := args[key]
	if !ok || raw == nil {
		return 0
	}
	switch v := raw.(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}
