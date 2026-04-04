# Spec: Server-side hop resolution at ingest — `resolved_path`

**Status:** Final  
**Issue:** [#555](https://github.com/Kpa-clawbot/CoreScope/issues/555)  
**Related:** [#482](https://github.com/Kpa-clawbot/CoreScope/issues/482), [#528](https://github.com/Kpa-clawbot/CoreScope/issues/528)

## Problem

Any place where 1, 2, or 3-byte prefixes must be resolved to actual full repeater public keys and friendly names should use affinity data first, geo data as fallback. Across frontend, backend, whatever. Efficiently — no 7-second waits, no recomputation, aggressive caching.

Currently, hop paths are stored as short uppercase hex prefixes in `path_json` (e.g. `["D6", "E3", "59"]`). Resolution to full pubkeys happens **client-side** via `HopResolver` (`public/hop-resolver.js`), which:

- Is slow — each page/component re-resolves independently
- Is inconsistent — different components may resolve the same prefix differently
- Cannot leverage the server's neighbor affinity graph, which has far richer context for disambiguation
- Causes redundant `/api/resolve-hops` calls from every client

## Solution

Resolve hop prefixes to full pubkeys **once at ingest time** on the server, using `resolveWithContext()` with 4-tier priority (affinity → geo → GPS → first match) and a **persisted neighbor graph**. Store the result as a new `resolved_path` field alongside `path_json`.

## Design decisions (locked)

1. **`path_json` stays unchanged** — raw firmware prefixes, uppercase hex. Ground truth.
2. **`resolved_path` is NEW** — `[]*string` on observations. Full 64-char lowercase hex pubkeys, `null` for unresolved.
3. **Resolved at ingest** using `resolveWithContext(hop, context, graph)` — 4-tier priority: affinity → geo → GPS → first match.
4. **Stored alongside `path_json`** — raw firmware prefixes stay unchanged.
5. **`null` = unresolved** — ambiguous prefixes store `null`. Frontend falls back to prefix display.
6. **Both fields coexist** — not interchangeable. Different consumers use different fields.

## Persisted neighbor graph

### SQLite table: `neighbor_edges`

```sql
CREATE TABLE IF NOT EXISTS neighbor_edges (
    node_a TEXT NOT NULL,
    node_b TEXT NOT NULL,
    count INTEGER DEFAULT 1,
    last_seen TEXT,
    PRIMARY KEY (node_a, node_b)
);
```

### Order of operations

1. **First run:** Built on startup from existing packet data, then persisted to SQLite.
2. **Subsequent runs:** Loaded from SQLite — instant startup, no 7-second rebuild.
3. **Available at ingest** — lives on `PacketStore`, not `Server`.
4. **Updated incrementally** — each new packet upserts edges.

This replaces the previous "in-memory only for v1" approach. The graph is always persisted, always fast to load.

## Data model

### Where does `resolved_path` live?

**On observations**, not transmissions.

Rationale: Each observer sees the packet from a different vantage point. The same 2-char prefix may resolve to different full pubkeys depending on which observer's neighborhood is considered. The observer's own pubkey provides critical context for `resolveWithContext` (tier 2: neighbor affinity). Storing on observations preserves this per-observer resolution.

`path_json` already lives on observations in the DB schema. `resolved_path` follows the same pattern.

### Field shape

```
resolved_path TEXT  -- JSON array: ["aabb...64chars", null, "ccdd...64chars"]
```

- Same length as the `path_json` array
- Each element is either a 64-char lowercase hex pubkey string, or `null`
- Stored as a JSON text column (same approach as `path_json`)
- Uses `omitempty` — absent from JSON when not set

## Ingest pipeline changes

### Where resolution happens

In `PacketStore.IngestNewFromDB()` and `PacketStore.IngestNewObservations()` in `cmd/server/store.go`. Resolution is added **after** the observation is read from DB and **before** it's stored in memory and broadcast.

Resolution flow per observation:
1. Parse `path_json` into hop prefixes
2. Build context pubkeys from the observation (observer pubkey, source/dest from decoded packet)
3. Call `resolveWithContext(hop, contextPubkeys, neighborGraph)` for each hop
4. Store result as `resolved_path` on the observation
5. Upsert neighbor edges into the graph (incremental update)

### Performance

`resolveWithContext` does:
- Prefix map lookup (map access, O(1))
- Optional neighbor graph check (small map lookups)
- No DB queries, no network calls

Per-hop cost: ~1–5μs. A typical packet has 0–5 hops. At 100 packets/second ingest rate, this adds <0.5ms total overhead per second. **Negligible.**

## All consumers use `resolved_path`

| Consumer | Before | After |
|---|---|---|
| Packets detail path names | Client HopResolver (naive) | Read `resolved_path` |
| Map Show Route | Client HopResolver (naive) | Read `resolved_path` |
| Live map animated paths | Client HopResolver (naive) | Read `resolved_path` |
| Node detail paths | Client HopResolver (naive) | Read `resolved_path` |
| Analytics topology/subpaths | Server `prefixMap.resolve()` (naive) | Read `resolved_path` from observations |
| Analytics hop distances | Server `resolveWithContext` | Already correct |
| Show Neighbors | Server neighbors API | Already correct |
| `/api/resolve-hops` | Server `resolveWithContext` | Already correct |
| Hex breakdown display | `path_json` raw | Unchanged — shows raw bytes |

## WebSocket broadcast

Include `resolved_path` in broadcast messages. Resolution happens before broadcast assembly — negligible latency impact. The WS broadcast already includes `path_json`; `resolved_path` is added alongside it.

## API changes

### Endpoints that return `resolved_path`

All endpoints that currently return `path_json` also return `resolved_path`:

- `GET /api/packets` — transmission-level (use best observation's `resolved_path`)
- `GET /api/packets/:hash` — per-observation detail
- `GET /api/packets/:hash/observations` — each observation includes its own `resolved_path`
- WebSocket broadcast messages — per-observation

### `/api/resolve-hops`

**Kept.** Useful for ad-hoc resolution of arbitrary prefixes (debug tools, clients resolving prefixes not associated with a packet). Not deprecated.

## Pubkey case convention

- **DB/API:** lowercase
- **`path_json` display prefixes:** uppercase (raw firmware)
- **`resolved_path`:** lowercase full pubkeys
- **Comparison code:** normalizes to lowercase
- **No case standardization crusade** — document the convention, don't change existing behavior

## Backward compatibility

- Old observations without `resolved_path`: frontend falls back to client-side HopResolver (same as today).
- `resolved_path` field uses `omitempty` — absent from JSON when not set.

### Fallback pattern (frontend)

```javascript
function getResolvedHops(packet) {
  if (packet.resolved_path) return packet.resolved_path;
  // Fall back to client-side resolution for old packets
  return resolveHopsClientSide(packet.path_json);
}
```

## Implementation milestones

### M1: Persist graph to SQLite + load on startup + incremental updates at ingest
- Create `neighbor_edges` table in SQLite
- Build graph from existing packet data on first run
- Load from SQLite on subsequent runs (instant startup)
- Upsert edges incrementally during packet ingest
- Graph lives on `PacketStore`, not `Server`
- Tests: graph persistence, load, incremental update

### M2: Add `resolved_path` to observations at ingest time
- Add `ResolvedPath []*string` to `Observation` struct
- Resolve during `IngestNewFromDB`/`IngestNewObservations`
- Write `resolved_path` back to SQLite observations table
- Schema migration to add `resolved_path` column
- Tests: unit test resolution at ingest, verify stored values

### M3: Update all API responses to include `resolved_path`
- Include `resolved_path` in all packet/observation API responses
- Include in WebSocket broadcast messages
- Tests: verify API response shape, WS broadcast shape

### M4: Update frontend consumers to prefer `resolved_path`
- Update `packets.js`, `map.js`, `live.js`, `analytics.js`, `nodes.js`, `traces.js`
- Add fallback to `path_json` + `HopResolver` for old packets
- `hop-resolver.js` becomes fallback only
- Tests: Playwright tests for path display
