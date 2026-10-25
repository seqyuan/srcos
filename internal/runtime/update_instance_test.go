package runtime

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestUpdateInstanceCreatesAndApplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "instances", "alice-demo-x.yaml")
	got, applied, err := UpdateInstance(path, func(cur *Instance) error {
		cur.ID = "alice-demo-x"
		cur.State = StateRunning
		return nil
	})
	if err != nil || !applied {
		t.Fatalf("update = %v %v", applied, err)
	}
	if got.State != StateRunning {
		t.Fatalf("state = %s", got.State)
	}
	saved, err := LoadInstance(path)
	if err != nil || saved.ID != "alice-demo-x" || saved.State != StateRunning {
		t.Fatalf("saved = %+v %v", saved, err)
	}
}

func TestUpdateInstanceRespectsADeliberateStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "instances", "alice-demo-x.yaml")
	if err := SaveInstance(path, &Instance{ID: "alice-demo-x", State: StateRunning}); err != nil {
		t.Fatal(err)
	}
	// The stop wins.
	if _, applied, err := UpdateInstance(path, func(cur *Instance) error {
		if cur.State == StateStopped {
			return ErrInstanceChanged
		}
		cur.State = StateStopped
		cur.Error = "cancelled"
		return nil
	}); err != nil || !applied {
		t.Fatalf("stop = %v %v", applied, err)
	}
	// A terminal verdict arriving later must not resurrect it.
	got, applied, err := UpdateInstance(path, func(cur *Instance) error {
		if cur.State == StateStopped {
			return ErrInstanceChanged
		}
		cur.State = StateSucceeded
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("a terminal write overwrote a deliberate stop")
	}
	if got.State != StateStopped || got.Error != "cancelled" {
		t.Fatalf("record = %+v", got)
	}
}

func TestUpdateInstanceSerializesReadModifyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "instances", "alice-demo-x.yaml")
	if err := SaveInstance(path, &Instance{ID: "alice-demo-x", State: StateRunning}); err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := UpdateInstance(path, func(cur *Instance) error {
				cur.ExitCode++
				return nil
			})
			if err != nil {
				t.Errorf("update: %v", err)
			}
		}()
	}
	wg.Wait()
	saved, err := LoadInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ExitCode != n {
		t.Fatalf("lost updates: exitCode=%d, want %d", saved.ExitCode, n)
	}
}

func TestUpdateInstanceChangedErrorLeavesTheRecordAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "instances", "alice-demo-x.yaml")
	if err := SaveInstance(path, &Instance{ID: "alice-demo-x", State: StateRunning}); err != nil {
		t.Fatal(err)
	}
	got, applied, err := UpdateInstance(path, func(cur *Instance) error {
		cur.State = StateFailed
		return ErrInstanceChanged
	})
	if err != nil || applied {
		t.Fatalf("update = %v %v", applied, err)
	}
	if got.State != StateRunning {
		t.Fatalf("returned = %+v", got)
	}
	saved, _ := LoadInstance(path)
	if saved.State != StateRunning {
		t.Fatalf("the record changed: %+v", saved)
	}
}

func TestErrInstanceChangedIsSentinel(t *testing.T) {
	if !errors.Is(ErrInstanceChanged, ErrInstanceChanged) {
		t.Fatal("sentinel")
	}
}
