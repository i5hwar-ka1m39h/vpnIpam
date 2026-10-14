package ipam

import (
	"errors"
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
	if _, err := p.Allocate("s2"); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected pool exhaustion error, got %v", err)
	}
}

func TestReleaseNotFound(t *testing.T) {
	p := mustPool(t, "10.8.0.0/30")
	if err := p.Release("notId"); !errors.Is(err, ErrAlreadyReleased) {
		t.Fatalf("expection already released err, got %v", err)
	}
}
