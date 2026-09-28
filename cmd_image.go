package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/image"
)

// srcos image ... pre-warms container images for sandbox: apptainer tools.
//
// The SIF a tool names must exist on the shared filesystem before a job runs on
// a compute node. This command does that once, up front, using the same runtime
// the sandbox will use (apptainer, falling back to singularity).
func runImageCmd(args []string) {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "pull":
		runImagePull(args)
	case "list", "ls":
		runImageList(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown image subcommand: %s\n", sub)
		printImageUsage()
		os.Exit(1)
	}
}

func printImageUsage() {
	fmt.Fprintln(os.Stderr, "usage: srcos image <pull|list> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  pull <ref> [--to <file.sif>] [--dir <dir>] [--runtime <bin>] [--force]")
	fmt.Fprintln(os.Stderr, "      --to <file.sif>   explicit output path")
	fmt.Fprintln(os.Stderr, "      --dir <dir>       output directory when --to is omitted (default <data>/images)")
	fmt.Fprintln(os.Stderr, "      --runtime <bin>   apptainer | singularity (default: probe)")
	fmt.Fprintln(os.Stderr, "      --force           re-pull even if the file already exists")
	fmt.Fprintln(os.Stderr, "  list [--dir <dir>]    list pre-warmed images")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  -d, --config-dir <dir>  (default <program dir>/config)")
}

func runImagePull(args []string) {
	fs := newFlagSet("image pull")
	configDir := configDirFlag(fs)
	var positional []string
	to := fs.String("to", "", "output .sif path")
	dir := fs.String("dir", "", "output directory")
	runtimeBin := fs.String("runtime", "", "container runtime binary")
	force := fs.Bool("force", false, "re-pull even if present")
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "error: image pull requires a ref (e.g. docker://ubuntu:22.04)")
		os.Exit(1)
	}
	ref := positional[0]

	dest := *to
	if dest == "" {
		d := *dir
		if d == "" {
			d = filepath.Join(config.DataDir(*configDir), "images")
		}
		dest = image.DefaultDest(d, ref)
	}

	// A multi-GB pull over a slow link can legitimately take a long time; the
	// timeout is a backstop, not a policy.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	path, err := image.Pull(ctx, image.ExecRunner{}, image.PullOptions{
		Ref: ref, Dest: dest, Bin: *runtimeBin, Force: *force,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("image ready: %s\n", path)
}

func runImageList(args []string) {
	fs := newFlagSet("image list")
	configDir := configDirFlag(fs)
	var positional []string
	dir := fs.String("dir", "", "directory to list")
	parseFlagsLoose(fs, args, &positional)

	d := *dir
	if d == "" {
		d = filepath.Join(config.DataDir(*configDir), "images")
	}
	imgs, err := image.List(d)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if len(imgs) == 0 {
		fmt.Printf("no images in %s\n", d)
		return
	}
	for _, p := range imgs {
		size := ""
		if fi, err := os.Stat(p); err == nil {
			size = humanBytes(fi.Size())
		}
		fmt.Printf("%s  %s\n", p, size)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", value, units[idx])
}
