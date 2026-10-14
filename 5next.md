# Immediate Next 5 Steps (do in order, don't jump to Postgres yet)

> Current state: `Pool` builds, Allocate+Release work, but no Stats, no tests, no race proof.
> Goal after these 5: IPAM is trusted, tested, and DB-ready.

---

## Step 1: Cleanup `NewPool` — store masked prefix once

**Why:** You call `prefix.Masked()` on every Allocate. Store it once, avoid `10.8.0.5/24` bugs forever.

**Tasks:**
- [ ] In `NewPool`, right after `ParsePrefix`, do `prefix = prefix.Masked()`
- [ ] Use `p.prefix.Addr()` everywhere (lines 52, 71) — no more `Masked()` in hot path
- [ ] Lowercase errors: `errors.New("invalid ipv4 pool")` etc. (Go convention)
- [ ] Add `ErrInvalidSessionID = errors.New("invalid session id")` and use it in Allocate/Release instead of `errors.New("a session id is required")`
- [ ] Run: `go vet ./... && go build ./...`

**Done when:** `go vet` green, `NewPool("10.8.0.5/24")` + `NewPool("10.8.0.0/30")` both give correct broadcast + reserved.

**Hint:**
```go
prefix, err := netip.ParsePrefix(cidr)
if err != nil { return nil, ... }
prefix = prefix.Masked() // normalize once
```

---

## Step 2: Add `Stats()` + `Capacity()`

**Why:** API `/v1/ipam/stats`, reaper, and Prometheus all need this. Forces you to think about total math.

**Tasks:**
- [ ] Add type:
```go
type Stats struct { Total, Used, Available int }
```
- [ ] Implement:
```go
func (p *Pool) Stats() Stats {
  p.mu.Lock(); defer p.mu.Unlock()
  hostBits := 32 - p.prefix.Bits()
  total := (1 << hostBits) - 2 // minus network+broadcast
  if total < 0 { total = 0 } // covers /31, /32
  used := len(p.allocated)
  return Stats{Total: total, Used: used, Available: total - used}
}
```
- [ ] Manual check: `/24` -> 254, `/30` -> 2, `/29` -> 6

**Done when:** `Stats()` after 0 alloc = `{254 0 254}`, after 1 alloc = `{254 1 253}`.

**Common mistake:** Reading `len(p.allocated)` without lock = data race. Always lock.

---

## Step 3: Lock the idempotency contract (5 min decision, saves DB pain)

**Why:** Your DB schema depends on this. Decide now, document in code comment.

**Tasks:**
- [ ] Choose and comment above each method:
  - `Allocate` duplicate sessionID: Option A strict `return {}, ErrDuplicateSession` (current) vs Option B idempotent `return existingIP, nil`. Pick A for now, but write `// Allocate is strict: duplicate sessionID returns ErrDuplicateSession`.
  - `Release` missing sessionID: currently `ErrSessionNotFound`. For VPN `DELETE` idempotency you will want caller to treat “already gone = success”. Keep error here, but note: `SessionManager.Terminate` will map it to success (204).
- [ ] Add empty-ID guard to `Release` too.
- [ ] Update `ErrDuplicateSession` message to lowercase `session already exists`.

**Done when:** Someone reading `pool.go` knows without asking what double-Allocate and double-Release do.

---

## Step 4: Write `pool_test.go` — basic correctness

**Why:** Without this you cannot trust Step 5 or Postgres later.

**Tasks:** Create `internal/ipam/pool_test.go`:
- [ ] `TestNewPoolInvalid`: `"foo"`, `"10.0.0.0/7"` (too big), `"::1/128"` (v6) -> error
- [ ] `TestAllocateExhaust`: use `/30` (only 2 usable: .1, .2). Allocate 2x success, 3rd -> `ErrPoolExhausted`
- [ ] `TestDuplicateSession`: Allocate("s1") -> IP, Allocate("s1") -> `ErrDuplicateSession`, `Stats().Used==1`
- [ ] `TestReleaseReuse`: Allocate("s1")->IP1, Release("s1")->nil, Allocate("s2")-> same IP1 (reuse proves free-list works)
- [ ] `TestReleaseNotFound`: Release("nope") -> `ErrSessionNotFound`
- [ ] Run: `go test ./internal/ipam -v -count=1`

**Hint skeleton:**
```go
func TestAllocateExhaust(t *testing.T) {
  p, _ := NewPool("10.8.0.0/30")
  _, _ = p.Allocate("s1")
  _, _ = p.Allocate("s2")
  if _, err := p.Allocate("s3"); !errors.Is(err, ErrPoolExhausted) {
    t.Fatalf("expected exhausted, got %v", err)
  }
}
```

**Done when:** `go test -v` all green.

---

## Step 5: Prove concurrency + prep for DB (no Postgres yet)

**Why:** This is the interview question: “100 goroutines Allocate, any duplicates?”

**Tasks:**
- [ ] Add `TestConcurrentAllocate`:
```go
// 50 goroutines x Allocate unique sessionIDs on /24
// collect IPs via channel, assert all unique, assert Used==50
// then concurrent Release, assert Used==0
```
- [ ] Run: `go test -race ./...` must be green. If red, your `mu` is wrong.
- [ ] Add tiny benchmark: `BenchmarkAllocate(b)` with `b.N` Allocate/Release loop. Run `go test -bench=. -benchmem`. Note ns/op — this tells you O(n) scan cost.
- [ ] Define next interface (do NOT implement Postgres yet, just the contract in `internal/store/store.go`):
```go
// Draft only
type Store interface {
  CreateSession(id, clientID, ip string, expiresAt time.Time) error // UNIQUE(ip)
  GetByID(id string) (...) 
  MarkTerminated(id string) error
}
```
- [ ] Run final gate: `go vet ./... && go test -race ./...`

**Done when:** `-race` green, benchmark runs, `store.go` interface exists as comment/draft. THEN you are ready for goose+sqlc.

---

## Order + Commands cheat-sheet

```bash
# after each step
go vet ./... && go build ./...
go test ./internal/ipam -v -count=1
go test -race ./...
```

Do Step 1-2 today (30 min), Step 3-4 tomorrow, Step 5 when tests green. Show me `pool_test.go` + `go test -race` output and we’ll design the Postgres schema together.
