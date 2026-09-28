package image

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeRunner struct {
	calls [][]string
	reply func(name string, args []string) (string, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.reply == nil {
		return "", nil
	}
	return f.reply(name, args)
}

func TestPullRunsTheRuntimeAndReturnsDest(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "x.sif")
	fake := &fakeRunner{}
	got, err := Pull(context.Background(), fake, PullOptions{
		Ref: "docker://ubuntu:22.04", Dest: dest, Bin: "apptainer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != dest {
		t.Fatalf("Pull returned %q, want %q", got, dest)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %v", fake.calls)
	}
	got0 := fake.calls[0]
	want := []string{"apptainer", "pull", "--force", dest, "docker://ubuntu:22.04"}
	for i := range want {
		if got0[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got0, want)
		}
	}
}

func TestPullSkipsAnExistingImageUnlessForced(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "x.sif")
	if err := os.WriteFile(dest, []byte("sif"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRunner{}
	if _, err := Pull(context.Background(), fake, PullOptions{Ref: "x", Dest: dest, Bin: "apptainer"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("an already-warm image was re-pulled: %v", fake.calls)
	}
	if _, err := Pull(context.Background(), fake, PullOptions{Ref: "x", Dest: dest, Bin: "apptainer", Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("--force did not re-pull: %v", fake.calls)
	}
}

func TestPullRejectsABadDestinationAndEmptyRef(t *testing.T) {
	if _, err := Pull(context.Background(), &fakeRunner{}, PullOptions{Ref: "x", Dest: "/tmp/x.tar", Bin: "apptainer"}); err == nil {
		t.Fatal("a destination without .sif/.simg must be rejected")
	}
	if _, err := Pull(context.Background(), &fakeRunner{}, PullOptions{Dest: "/tmp/x.sif", Bin: "apptainer"}); err == nil {
		t.Fatal("an empty ref must be rejected")
	}
}

func TestDefaultDest(t *testing.T) {
	cases := map[string]string{
		"docker://ubuntu:22.04": "/img/ubuntu-22.04.sif",
		"library://alpine":      "/img/alpine.sif",
		"/local/x.sif":          "/img/local-x.sif",
	}
	for ref, want := range cases {
		if got := DefaultDest("/img", ref); got != want {
			t.Errorf("DefaultDest(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestListFindsImagesAndTreatsMissingAsEmpty(t *testing.T) {
	dir := t.TempDir()
	for name := range map[string]bool{"b.sif": true, "a.simg": true, "note.txt": true} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || filepath.Base(got[0]) != "a.simg" || filepath.Base(got[1]) != "b.sif" {
		t.Fatalf("List = %v", got)
	}
	if empty, err := List(filepath.Join(dir, "nope")); err != nil || len(empty) != 0 {
		t.Fatalf("a missing dir must be empty, not an error: %v %v", empty, err)
	}
}
