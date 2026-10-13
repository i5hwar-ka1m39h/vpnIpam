# VPN Session and IPAM Controller — Build Guide

> Stack: Go · REST API · Concurrency · Database
> Goal: A Go service that allocates VPN client IPs, creates sessions, applies policy decisions, and terminates expired/disconnected sessions.

You will build this in 9 phases. Don't skip ahead. Each phase has: Concept → Tasks → Acceptance Criteria → Self-check.

---

## 0. Mental Model (read this first)

### What is IPAM?
IP Address Management. You own a CIDR like `10.8.0.0/24` (254 usable IPs). Each VPN client that connects needs **one unique IP** while connected, returned to pool on disconnect.

Core problem: **concurrent allocation without duplicates.**

```
Pool: 10.8.0.0/24
Client A connects -> allocate 10.8.0.2 -> session s_123 ACTIVE
Client B connects -> allocate 10.8.0.3 -> session s_124 ACTIVE
Client A disconnects -> release 10.8.0.2 -> session s_123 TERMINATED
Client C connects -> reuse 10.8.0.2
```

### What is a Session?
State machine:

```
CREATED -> ACTIVE -> (HEARTBEAT refresh) -> EXPIRED / TERMINATED
                                     \-> REVOKED (policy deny)
```

Fields: `id, client_id, allocated_ip, status, created_at, last_seen_at, expires_at, idempotency_key`

### Architecture you will build

```
[VPN Client] --REST--> [API Handler] --> [SessionManager] --> [IPAM Allocator]
                              |                 |                    |
                              v                 v                    v
                           [Policy]        [Store (SQL)]      [in-mem + DB]
                              |
                        [Reaper goroutine: ticker -> expire + release]
```

Rules:
1. API never touches DB or IP pool directly. It calls `SessionManager`.
2. IPAM never touches DB directly in Phase 1-2. In Phase 5 you back it with DB.
3. All state changes go through transactions + unique constraints.

---

## Phase 0: Project Setup

**Concept to learn:** Go module layout, `net/netip`, graceful shutdown.

**Tasks:**
- [ ] Decide layout. Recommended:
```
cmd/server/main.go
internal/ipam/allocator.go
internal/session/manager.go
internal/policy/policy.go
internal/store/store.go (interface) + sqlite.go
internal/api/handlers.go + router.go
internal/reaper/reaper.go
migrations/schema.sql
```
- [ ] Add `go.mod` deps minimal: stdlib first. Add `modernc.org/sqlite` OR `github.com/mattn/go-sqlite3` for DB, `github.com/prometheus/client_golang` later for metrics. No web framework — use `net/http`.
- [ ] Add `Makefile`: `run, test, race, lint, migrate`
- [ ] Add config via env: `PORT, CIDR=10.8.0.0/24, SESSION_TTL=5m, DB_PATH`
- [ ] Implement `/healthz` that returns 200. Run it. `curl localhost:8080/healthz`.

**Acceptance:** `go run ./cmd/server` starts, `go vet ./...` passes.

**Self-check:** Why `internal/`? Why stdlib `net/http` over Gin for this project?

---

## Phase 1: IPAM Allocator (in-memory, thread-safe)

**Concept to learn:** CIDR math, mutex vs RWMutex, map + free list.

**Tasks:**
- [ ] Parse CIDR with `net/netip.ParsePrefix`. Reject network/broadcast addresses. List usable IPs.
- [ ] Implement interface:
```go
type Allocator interface {
  Allocate() (netip.Addr, error) // ErrNoCapacity when full
  Release(netip.Addr) error
  Stats() (total, used int)
}
```
- [ ] Internals: `mu sync.Mutex`, `used map[netip.Addr]bool`, `free []netip.Addr` (stack/queue) or `next + free-list`.
- [ ] `Allocate`: lock, pop from free OR bump next pointer, mark used, unlock.
- [ ] `Release`: lock, check was-used (return error if double-release?), mark unused, push to free.
- [ ] Write unit tests: allocate all N, next fails, release one, re-allocate gets same IP.

**Concurrency test (critical):**
- [ ] `go test -race`: spawn 100 goroutines x Allocate/Release 1000 times. Assert no duplicate IPs ever handed out concurrently. Hint: use `sync.WaitGroup` + collect results in channel + check uniqueness.

**Acceptance:** `ErrNoCapacity` when full, no duplicates under `-race`, `Stats()` correct.

**Pitfalls:** Holding lock while doing I/O. Forgetting to copy `netip.Addr` (it is value type — ok). Using `sync.Map` alone — not enough for allocate-check-allocate atomicity, you need Mutex.

**Self-check:** Why can't you do `if _, ok := used[ip]; !ok { used[ip]=true }` without a lock? What is a data race?

---

## Phase 2: Session Lifecycle (no DB yet)

**Concept to learn:** Session state machine, TTL/expiry, idempotency.

**Tasks:**
- [ ] Define `type Status string`: `active, expired, terminated, revoked`.
- [ ] Define `type Session struct { ID string; ClientID string; IP netip.Addr; Status; CreatedAt, LastSeenAt, ExpiresAt time.Time }`
- [ ] Implement `Manager`:
```go
CreateSession(ctx, clientID, idempotencyKey string) (*Session, error)
Heartbeat(ctx, sessionID string) error // updates LastSeenAt + ExpiresAt
TerminateSession(ctx, sessionID string) error // idempotent
GetSession(ctx, sessionID string) (*Session, error)
```
- [ ] `CreateSession`: 1) validate clientID non-empty, 2) check idempotency map, 3) `ipam.Allocate()`, 4) generate ID (`crypto/rand` UUID), 5) `ExpiresAt = now + TTL`, store in `map[string]*Session` with `sync.RWMutex`.
- [ ] `TerminateSession` must be **idempotent**: terminating twice returns success (same result), releases IP only once. Second call = no-op if already terminated/expired.
- [ ] `Heartbeat`: if expired/terminated -> error. Else extend expiry.
- [ ] Unit test lifecycle: create -> get -> heartbeat extends -> terminate -> terminate again (no error, no panic) -> create new reuses IP.

**Acceptance:** Double-terminate safe. Expired sessions not heartbeat-able.

**Self-check:** What does idempotent mean for REST? Why do VPN reconnect storms need it?

---

## Phase 3: Policy Decisions

**Concept to learn:** Separation of policy from mechanism.

**Tasks:**
- [ ] Create `policy.Decide(clientID string, activeSessionsForClient int) (allow bool, reason string)`.
- [ ] Implement rules (start simple, make configurable):
  - max 2 active sessions per `client_id` -> else `deny: session_limit`
  - blocklist: e.g. `blocked-client-*` -> `deny: blocked`
  - (stretch) subnet quota: max X% pool used -> deny new with `capacity_warning`
- [ ] Wire into `CreateSession`: if deny -> return typed error `ErrPolicyDenied{Reason}` without allocating IP. Do NOT allocate then rollback if you can check first.
- [ ] Test table-driven: allowed, over-limit, blocked.

**Acceptance:** Policy deny never consumes an IP (`Stats().used` unchanged).

**Self-check:** Why should policy be a pure function + interface, not SQL sprinkled in manager?

---

## Phase 4: REST API

**Concept to learn:** Validation, status codes, idempotency header.

**API contract to implement:**

```
POST   /v1/sessions              {client_id} + Idempotency-Key header -> 201 {session}
GET    /v1/sessions/{id}         -> 200 {session}
POST   /v1/sessions/{id}/heartbeat -> 200 {session}
DELETE /v1/sessions/{id}         -> 204 (always 204 even if already gone = idempotent)
GET    /v1/ipam/stats            -> 200 {total, used, available}
GET    /healthz                  -> 200
GET    /metrics                  -> prometheus (Phase 8)
```

Session JSON example:
```json
{
  "id": "sess_01J...",
  "client_id": "laptop-1",
  "allocated_ip": "10.8.0.2",
  "status": "active",
  "expires_at": "2026-10-13T12:00:00Z"
}
```

**Tasks:**
- [ ] Router with `http.ServeMux` (Go 1.22+ pattern `POST /v1/sessions`).
- [ ] Validation: `client_id` required, 1-64 chars, `[a-zA-Z0-9-_]`; `Idempotency-Key` optional but if present cache it; malformed -> `400` with `{"error":"..."}`.
- [ ] Errors mapping: `ErrNoCapacity -> 503`, `ErrPolicyDenied -> 403`, `not found -> 404`, `already terminated DELETE -> 204`.
- [ ] Request logging middleware (method, path, latency, status).
- [ ] `curl` manual test script in `README` or `scripts/`.
- [ ] Test with `httptest`: create 201, duplicate POST with same Idempotency-Key returns same session (not new IP), DELETE twice both 204.

**Acceptance:** All handlers have tests, no business logic in handlers (handlers < 30 lines each).

**Pitfall:** Returning `500` for policy deny. Returning new session on retry — breaks VPN client retries.

---

## Phase 5: Persistence — Transactions + Unique Constraints

**Concept to learn:** This is the interview-heavy phase. Unique constraints as correctness guard, not just validation.

**Schema (`migrations/schema.sql`):**
```sql
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  client_id TEXT NOT NULL,
  allocated_ip TEXT NOT NULL UNIQUE, -- prevents double-allocate at DB level
  status TEXT NOT NULL CHECK (status IN ('active','expired','terminated','revoked')),
  idempotency_key TEXT UNIQUE, -- nullable, but unique when present
  created_at TIMESTAMP NOT NULL,
  last_seen_at TIMESTAMP NOT NULL,
  expires_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_sessions_client_status ON sessions(client_id, status);
CREATE INDEX idx_sessions_expires ON sessions(status, expires_at);
```

**Tasks:**
- [ ] Define `Store` interface: `Create, GetByID, GetByIdempotencyKey, ListActiveByClient, UpdateHeartbeat, MarkTerminated, ListExpired(limit)`.
- [ ] Implement with `database/sql`. Use transactions for `CreateSession`: `BEGIN; SELECT count active for client; INSERT; COMMIT`. Rely on `UNIQUE(allocated_ip)` to catch races — on conflict, retry with next IP or return 503.
- [ ] Replace in-memory maps in Manager with Store + keep IPAM as source of truth backed by DB on startup: on boot, `SELECT allocated_ip WHERE status='active'` -> preload `used` map. This prevents re-allocating after restart.
- [ ] Termination transaction: `UPDATE sessions SET status='terminated' WHERE id=? AND status='active'` + release IP. If `rows_affected==0`, check if already terminated -> return success (idempotent), else 404.
- [ ] Integration test with real SQLite file (not mock): concurrent creates, kill -9 simulation (restart -> no duplicate).

**Acceptance:** `INSERT` duplicate IP fails at DB. Restart test passes. `go test -race` still green.

**Self-check:** Why is application-level `if used[ip]` check insufficient? What does `UNIQUE` give you under concurrency? What isolation level are you using?

---

## Phase 6: Reaper — Expired / Disconnected Sessions

**Concept to learn:** Background goroutines, `context.Context`, ticker, graceful shutdown, batching.

**Tasks:**
- [ ] Implement `reaper.Run(ctx, interval)`:
```
every T (e.g. 10s):
  rows = store.ListExpired(now, limit=100)
  for each: store.MarkExpiredOrTerminated + ipam.Release in transaction
  emit metric reaped_total++
```
- [ ] Make termination **idempotent + crash-safe**: if process dies mid-loop, next tick picks it up. Never release IP before DB marks expired.
- [ ] Order matters: `UPDATE status='expired' WHERE status='active' AND expires_at < now` first, then Release. Or single transaction doing both.
- [ ] Add config: `REAP_INTERVAL, SESSION_TTL, BATCH_SIZE`.
- [ ] Test: create with 100ms TTL, wait 300ms, run reaper once, assert `GET` shows expired + `Stats().used` decreased + IP reusable.
- [ ] Wire graceful shutdown: `signal.NotifyContext` -> cancel reaper -> `srv.Shutdown(ctx)`.

**Acceptance:** No leak (`goroutine` exits on cancel). Expired IPs reclaimed within 2x interval.

**Pitfall:** Reaper and `TerminateSession` racing on same session — both must be safe (use `WHERE status='active'` conditional update).

---

## Phase 7: Testing, Race, Load

- [ ] `go test ./... -race -count=1` green.
- [ ] Load script: `scripts/load.sh` or small Go program: 50 concurrent clients x create/heartbeat/terminate loop for 30s. Assert: no duplicates, `used==0` at end, p99 latency logged.
- [ ] Chaos: kill -TERM mid-load, restart, assert DB consistent.
- [ ] Add `COVERAGE`: `go test -cover` > 70% on `ipam`, `session`, `policy`.

---

## Phase 8: Observability — Logging, Metrics

- [ ] Structured logging (`log/slog`): `session_id, client_id, ip` on create/terminate/expire/deny.
- [ ] Prometheus counters/gauges:
```
vpn_sessions_active gauge
vpn_allocations_total counter
vpn_reaped_total counter
vpn_policy_denied_total{reason} counter
http_requests_total{code,method,path} counter
```
- [ ] `/metrics` endpoint. Verify with `curl`.
- [ ] (Optional) `GET /v1/ipam/stats` includes fragmentation / reuse count.

---

## Phase 9: Hardening + Ship

- [ ] `Dockerfile` multistage + `docker-compose` with app (SQLite volume).
- [ ] `README.md`: how to run, API examples, design decisions (why mutex+unique, why idempotent delete, reaper interval tradeoff).
- [ ] `config.go`: env parsing + validation (CIDR must be private, TTL > 10s).
- [ ] Graceful shutdown test: `SIGTERM` drains in-flight POSTs.
- [ ] Final demo: allocate full /29 (6 IPs) to show 503, expire, reuse.

---

## Suggested Build Order (do not parallelize learning)

1. Phase 0+1 (day 1): you understand IP math + race.
2. Phase 2+3 (day 2): lifecycle + policy pure functions.
3. Phase 4 (day 3): API + httptest.
4. Phase 5 (day 4-5): SQL — hardest, go slow.
5. Phase 6+8 (day 6): reaper + metrics.
6. Phase 7+9 (day 7): load + docker + README.

## Interview Questions This Prepares You For

- How do you allocate IPs thread-safely? (mutex + free-list + DB UNIQUE)
- What happens if two requests allocate at same time? (serialize in mem, guard in DB)
- How do you make DELETE idempotent? (conditional update + return 204 if already done)
- How do you clean up expired sessions without missing any? (ticker + batch + index on expires_at)
- How do you handle restart without double-allocate? (reload active IPs on boot)

## Done Definition

- [ ] `make test race` passes
- [ ] Full pool -> 503, policy deny -> 403, double DELETE -> 204, retry POST same key -> same session
- [ ] Kill + restart -> no duplicate IP
- [ ] Reaper reclaims expired within interval
- [ ] README explains tradeoffs

Start with Phase 1. Tell me when you finish Allocator and show me the code for review.
