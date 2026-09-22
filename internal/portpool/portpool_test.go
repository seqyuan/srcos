package portpool

import (
	"net"
	"strconv"
	"sync"
	"testing"
)

func TestAcquireReturnsFreeLoopbackPorts(t *testing.T) {
	p := New(21000, 21010)
	seen := map[int]bool{}
	for i := 0; i < 11; i++ {
		port, err := p.Acquire("inst")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if port < 21000 || port > 21010 {
			t.Fatalf("port %d outside the range", port)
		}
		if seen[port] {
			t.Fatalf("port %d handed out twice", port)
		}
		seen[port] = true
	}
	if _, err := p.Acquire("inst"); err == nil {
		t.Fatal("expected exhaustion after filling the range")
	}
}

func TestReleaseMakesPortAvailableAgain(t *testing.T) {
	p := New(21100, 21100) // a single port, to force reuse
	port, err := p.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire("b"); err == nil {
		t.Fatal("the only port is taken")
	}
	if owner, ok := p.Owner(port); !ok || owner != "a" {
		t.Fatalf("owner = %q, %v", owner, ok)
	}

	p.Release(port)
	again, err := p.Acquire("b")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	if again != port {
		t.Fatalf("expected to reuse %d, got %d", port, again)
	}
}

func TestReleaseUnknownPortIsNoop(t *testing.T) {
	p := New(21200, 21205)
	taken, err := p.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	p.Release(999999) // never allocated
	p.Release(taken)
	p.Release(taken) // double release
	if n := len(p.InUse()); n != 0 {
		t.Fatalf("in use = %d", n)
	}
}

func TestAcquireSkipsExternallyUsedPort(t *testing.T) {
	// Hold a port outside the pool's knowledge; allocation must step over it
	// rather than hand out a port that cannot be bound.
	blocker, err := net.Listen("tcp", "127.0.0.1:21300")
	if err != nil {
		t.Skipf("cannot bind the probe port: %v", err)
	}
	defer blocker.Close()

	p := New(21300, 21301)
	port, err := p.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	if port == 21300 {
		t.Fatalf("handed out a port that is already bound: %d", port)
	}
	if port != 21301 {
		t.Fatalf("expected 21301, got %d", port)
	}
}

func TestDefaultRangeWhenInvalid(t *testing.T) {
	for _, tc := range [][2]int{{0, 0}, {100, 50}, {80, 100}} {
		lo, hi := New(tc[0], tc[1]).Range()
		if lo != DefaultLow || hi != DefaultHigh {
			t.Fatalf("New(%d,%d) range = %d-%d", tc[0], tc[1], lo, hi)
		}
	}
}

func TestReservePinsAPort(t *testing.T) {
	p := New(21400, 21402)
	if err := p.Reserve(21401, "survivor"); err != nil {
		t.Fatal(err)
	}
	// Reserving again for the same owner is idempotent (startup replay).
	if err := p.Reserve(21401, "survivor"); err != nil {
		t.Fatalf("idempotent reserve: %v", err)
	}
	// A different owner must not steal it.
	if err := p.Reserve(21401, "other"); err == nil {
		t.Fatal("expected a conflict")
	}
	// Out of range is refused.
	if err := p.Reserve(99999, "x"); err == nil {
		t.Fatal("expected a range error")
	}

	// The pinned port is not handed out.
	for i := 0; i < 2; i++ {
		port, err := p.Acquire("new")
		if err != nil {
			t.Fatal(err)
		}
		if port == 21401 {
			t.Fatal("a pinned port was handed out")
		}
	}
}

func TestConcurrentAcquireIsUnique(t *testing.T) {
	// Allocation is the one place a race would route one user's traffic to
	// another user's process, so it is worth an explicit concurrency test.
	p := New(21500, 21599)
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[int]bool{}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, err := p.Acquire("x")
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if got[port] {
				t.Errorf("port %d handed out twice", port)
			}
			got[port] = true
		}()
	}
	wg.Wait()
	if len(got) != 50 {
		t.Fatalf("expected 50 unique ports, got %d", len(got))
	}
}

func TestInUseIsSorted(t *testing.T) {
	p := New(21600, 21610)
	for i := 0; i < 3; i++ {
		if _, err := p.Acquire("x"); err != nil {
			t.Fatal(err)
		}
	}
	used := p.InUse()
	for i := 1; i < len(used); i++ {
		if used[i-1] >= used[i] {
			t.Fatalf("not ascending: %v", used)
		}
	}
}

// TestAcquiredPortIsActuallyBindable is the property that matters: whatever
// Acquire returns must be bindable by the instance.
func TestAcquiredPortIsActuallyBindable(t *testing.T) {
	p := New(21700, 21705)
	port, err := p.Acquire("svc")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("the pool handed out an unbindable port %d: %v", port, err)
	}
	ln.Close()
}
