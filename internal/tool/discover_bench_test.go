package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchManifest is the smallest valid task tool (the same shape the flowrun
// tests use), so the benchmark measures Discover/Digest, not validation.
const benchManifest = `
schemaVersion: 1
id: bench
version: 1.0.0
name: Bench
kind: task
backend: local
sandbox: none
entry: work.sh
interface:
  inputs:
    - {name: sample_id, type: string, required: true}
    - {name: outdir, type: directory, required: true}
  outputs:
    - {name: outs, type: directory}
resources: {cpu: 1, memory: "256Mi", walltime: "0:01:00"}
`

// BenchmarkDiscover is the #3 baseline. Discover → Load → Digest reads every
// file's *bytes* on every call, so a tool package that ships a large payload
// makes every list/detail request (and every canvas save) proportional to it.
func BenchmarkDiscover(b *testing.B) {
	for _, mb := range []int{0, 50} {
		b.Run(fmt.Sprintf("payload=%dMiB", mb), func(b *testing.B) {
			root := b.TempDir()
			dir := filepath.Join(root, "bench")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(benchManifest), 0o644); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				b.Fatal(err)
			}
			if mb > 0 {
				if err := os.WriteFile(filepath.Join(dir, "payload.bin"), make([]byte, mb<<20), 0o644); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Discover(root); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
