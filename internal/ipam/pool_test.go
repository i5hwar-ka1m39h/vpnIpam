package ipam

import (
	"errors"
	"sync"
	"testing"
)

func mustPool(t *testing.T, cidr string) *Pool {
	t.Helper()
	p, err := NewPool(cidr)
	if err != nil {
		t.Fatalf("NewPool(%q) failed: %v", cidr, err)
	}

	return p
}

func TestAllocateAndReuse(t *testing.T) {
	p := mustPool(t, "10.8.0.0/30")

	ip1, err := p.Allocate("s1")
	if err != nil {
		t.Fatalf("alloc s1: %v", err)
	}

	ipAgain, err := p.Allocate("s1")
	if err != nil {
		t.Fatalf("alloc s1: %v", err)
	}

	if ipAgain != ip1 {
		t.Fatalf("expected same ip %v, got %v", ip1, ipAgain)
	}

	if err := p.Release("s1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	ip2, _ := p.Allocate("s2")

	if ip2 != ip1 {
		t.Fatalf("expected resuse %v, got %v", ip1, ip2)
	}

}

func TestExhaused(t *testing.T) {
	p := mustPool(t, "10.8.0.0/30")
	p.Allocate("s1")
	p.Allocate("s2")
	if _, err := p.Allocate("s3"); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected pool exhaustion error, got %v", err)
	}
}

func TestReleaseNotFound(t *testing.T) {
	p := mustPool(t, "10.8.0.0/30")
	if err := p.Release("notId"); !errors.Is(err, ErrAlreadyReleased) {
		t.Fatalf("expection already released err, got %v", err)
	}
}

func TestStats(t *testing.T) {
	p := mustPool(t, "10.8.0.0/24")
	p.Allocate("s1")
	p.Allocate("s2")
	p.Allocate("s3")
	p.Allocate("s4")
	p.Allocate("s5")
	p.Allocate("s6")
	p.Allocate("s7")

	s := p.Stats()
	if s.Total != 254 || s.Used != 7 || s.Available != 247 {
		t.Fatalf("bad stats expected total 254, used 7 and available 247, got %+v", s)
	}

	p.Release("s7")
	p.Release("s2")

	s = p.Stats()
	if s.Total != 254 || s.Used != 5 || s.Available != 249 {
		t.Fatalf("bad stats expected total 254, used 5 and available 249, got %+v", s)
	}
}

func TestConcurrentAllocationNoDuplicate(t *testing.T) {
	p := mustPool(t, "10.8.0.0/24")

	const N = 50
	var wg sync.WaitGroup

	ips := make(chan string, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip, err := p.Allocate("sess-" + string(rune(i)))
			if err != nil {
				t.Errorf("alloc %d: %v", i, err)
				return
			}
			ips <- ip.String()
		}(i)
	}
	wg.Wait()
	close(ips)

	seen := map[string]bool{}
	for ip := range ips {
		if seen[ip] {
			t.Fatalf("duplicate is %s", ip)
		}
		seen[ip] = true
	}

	if got := p.Stats().Used; got != N {
		t.Fatalf("expected used %d , got %d", N, got)
	}
}
