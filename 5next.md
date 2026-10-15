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

---

## Step 6: `internal/session/manager.go` — what to build (do this next)

> Goal: in-memory session lifecycle on top of your `Pool`. No Postgres, no HTTP yet.
> Rule: Manager owns workflow, Pool owns IPs. Manager never edits `p.allocated` directly.

### 6.1 Concepts first

A session is a state machine:
```
active -> terminated (client DELETE)
active -> expired (TTL passed, reaper or lazy-check)
terminated/expired are final. Heartbeat on final -> error.
Terminate on final -> success (idempotent, no error).
```

Two IDs, don't mix them:
- `ID`: server-generated, unique per connection. e.g. `sess_01K...` via `crypto/rand`. Returned to client, used in GET/DELETE.
- `ClientID`: client-provided, e.g. `laptop-1`. Many sessions can share it (reconnects). Used for policy `max 2 per client`.

### 6.2 Tasks

**a) Create `internal/session/session.go` (types only):**
- [ ] `type Status string` with `StatusActive, StatusTerminated, StatusExpired`
- [ ] `type Session struct { ID, ClientID string; IP netip.Addr; Status Status; CreatedAt, LastSeenAt, ExpiresAt time.Time }`
- [ ] Validate: `ClientID` 1-64 chars, `[a-zA-Z0-9-_]` only. Empty -> `ErrInvalidClientID`.

**b) Create `internal/session/manager.go`:**
```go
type Manager struct {
  mu sync.RWMutex
  pool *ipam.Pool
  sessions map[string]*Session // id -> session
  ttl time.Duration
}
func NewManager(pool *ipam.Pool, ttl time.Duration) *Manager
func (m *Manager) CreateSession(clientID string) (*Session, error)
func (m *Manager) Get(id string) (*Session, error)
func (m *Manager) Heartbeat(id string) (*Session, error)
func (m *Manager) Terminate(id string) error // idempotent
func (m *Manager) Stats() (active int)
```

- [ ] `CreateSession`: 1) validate clientID, 2) generate ID (`crypto/rand` 16 bytes hex, prefix `sess_`), 3) `ip, err := pool.Allocate(id)` — NOTE: use session ID as Pool key, not clientID, 4) build Session with `ExpiresAt = now.Add(ttl)`, store copy under `mu`, return copy.
  - If `Allocate` fails with `ErrPoolExhausted` -> return `ErrNoCapacity` (wrap for API 503 later).
  - If anything fails after Allocate, you must `pool.Release(id)` or you leak IP. This is the #1 bug.
- [ ] `Get`: RLock, copy out, if `time.Now().After(s.ExpiresAt)` and status==active -> treat as expired (lazy expire) OR return as-is and let reaper handle. Pick lazy+reaper, document it.
- [ ] `Heartbeat`: Lock, if not found -> `ErrNotFound`, if terminated/expired -> `ErrGone`, else `LastSeenAt=now, ExpiresAt=now+ttl`, return copy.
- [ ] `Terminate`: Lock, if not found -> `ErrNotFound`, if already terminated/expired -> `return nil` (idempotent, do NOT call pool.Release twice), else set status=terminated, unlock, then `pool.Release(id)`. Order: mark first, release second. Second call finds terminated -> nil.
- [ ] Always return copies (`*Session` copy), never internal pointer. Else caller mutates map without lock = race.

**c) Errors (in `session` package, lowercase):**
```go
var ErrNotFound, ErrGone, ErrInvalidClientID, ErrNoCapacity
```

### 6.3 Tests to write (`internal/session/manager_test.go`)

- [ ] Create->Get->Heartbeat extends expiry->Terminate->Terminate again nil->Get shows terminated
- [ ] Heartbeat on terminated -> ErrGone
- [ ] Exhaust: tiny `/30` pool, 2 creates ok, 3rd -> ErrNoCapacity, Stats().Used unchanged
- [ ] Concurrent: 20 goroutines Create unique clientIDs, assert unique IPs, `go test -race` green

Run: `go test ./internal/session -v -count=1 && go test -race ./...`

### 6.4 Done when

- [ ] Double-Terminate returns nil, IP released only once, IP reusable by next Create
- [ ] No direct `Pool` map access from session tests
- [ ] `-race` green

### 6.5 Common pitfalls

1. Using `clientID` as Pool key -> second connect from same laptop returns same IP (wrong). Pool key must be session `ID`.
2. Holding `Manager.mu` while calling `pool.Allocate()` which takes `Pool.mu` -> lock ordering. It works single-instance but document order: `Manager -> Pool`, never reverse, to avoid deadlock later.
3. Returning internal `*Session` pointer instead of copy -> caller race.
4. Forgetting Release on Create failure path -> leak, Stats().Used grows forever.

Show me your `Session` struct + `CreateSession` signature first, then implement one method at a time.
