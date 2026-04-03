# First-Hop Neighbor Affinity Graph

**Issue:** [#482](https://github.com/Kpa-clawbot/CoreScope/issues/482)
**Status:** Draft Spec
**Date:** 2026-04-03

---

## Overview

### What is the first-hop neighbor affinity graph?

A weighted, undirected graph where nodes are MeshCore devices and edges represent observed first-hop neighbor relationships. Each edge carries a weight (observation count), recency data, and optional signal quality metrics. The graph is built by analyzing `path_json` data from received packets — specifically, the first hop in each path reveals which node is the direct RF neighbor of the originating node.

### Why is it needed?

MeshCore uses short hash prefixes (typically 1–2 bytes) to identify nodes in routing paths. When multiple nodes share the same short prefix (hash collision), the current "Show Neighbors" feature on the map page cannot reliably determine which physical node a hop refers to. This causes:

1. **Show Neighbors returns zero results** when the hop resolver picks the wrong collision candidate (#484)
2. **Incorrect topology display** — paths appear to route through the wrong node
3. **hash_size==0 nodes inflate collision counts** (#441), compounding the problem

### What problem does it solve?

By aggregating first-hop observations over time, we build a stable model of which nodes are physically adjacent. This model serves as a disambiguation signal: when a 1-byte prefix like `"C0"` appears as a hop, and we know from hundreds of prior observations that `c0dedad4...` is a neighbor of the adjacent hop but `c0dedad9...` is not, we can resolve the ambiguity with high confidence.

This is especially effective for **repeaters and routers** which are stationary — their neighbor relationships are stable and predictable.

---

## Data Model

### How neighbor relationships are derived

Every packet stored in CoreScope has a `path_json` field containing the route hops as a JSON array of hex strings, e.g. `["A3","B7","C0"]`. The path represents the route from the originating node to the observer, where:

- The **first element** is the first hop after the origin (the origin's direct RF neighbor)
- The **last element** is the hop closest to the observer (the observer's direct RF neighbor)
- The originating node's pubkey is in the transmission record (`from_node`)

Therefore, a packet from node `X` with path `["A3", "B7"]` observed by observer `O` tells us:
- `X` has a direct neighbor matching prefix `A3`
- The node matching `A3` has a direct neighbor matching `B7`
- The node matching `B7` has a direct neighbor that is observer `O`

For **first-hop neighbor extraction**, we focus on:
- `from_node` ↔ `path[0]` (origin's direct neighbor)

This is the highest-confidence relationship because we know `from_node` exactly (full pubkey) and only need to resolve `path[0]`.

### What constitutes a "first-hop neighbor"

A first-hop neighbor of node `X` is any node that appears as `path[0]` in packets originating from `X`, OR any node `Y` where `X` appears as `path[0]` in packets originating from `Y`. The relationship is bidirectional in physical space (RF range is symmetric), though observations may be asymmetric (node A's packets may be observed more often than node B's).

### Handling hash collisions

When `path[0]` is a short prefix like `"A3"`, multiple nodes may match. The system must:

1. **Record all candidates** — do not discard ambiguous observations. Store the raw prefix alongside resolved candidates.
2. **Score candidates by context** — if node `X` has sent 500 packets with `path[0] = "A3"`, and candidate `a3xxxx` appears as a known neighbor of `X`'s other known neighbors but `a3yyyy` does not, `a3xxxx` scores higher.
3. **Use observation count as weight** — a candidate seen 500 times is more likely correct than one seen 2 times.
4. **Flag ambiguity** — edges where multiple candidates exist carry an `ambiguous` flag and a `candidates` list.

### Affinity scoring

Each edge `(A, B)` carries:

| Field | Type | Description |
|-------|------|-------------|
| `count` | int | Total observations of this neighbor relationship |
| `first_seen` | timestamp | Earliest observation |
| `last_seen` | timestamp | Most recent observation |
| `score` | float | Affinity score (0.0–1.0), computed from count + recency |
| `avg_snr` | float | Average SNR across observations (nullable) |
| `observers` | []string | Which observers witnessed this relationship |
| `ambiguous` | bool | Whether the hop prefix had multiple candidates |
| `candidates` | []string | All candidate pubkeys for ambiguous hops |

**Score formula:**

```
score = min(1.0, count / SATURATION_COUNT) × time_decay(last_seen)
```

Where:
- `SATURATION_COUNT = 100` — after 100 observations, count contributes max weight (configurable)
- `time_decay(t) = exp(-λ × hours_since(t))` where `λ = ln(2) / HALF_LIFE_HOURS`
- `HALF_LIFE_HOURS = 168` (7 days) — an edge unseen for 7 days decays to 50% score (configurable)

This means:
- A relationship seen 100+ times recently scores ~1.0
- A relationship seen 100+ times but not in 7 days scores ~0.5
- A relationship seen 5 times 30 days ago scores near 0

### Time decay

Time decay serves two purposes:

1. **Mobile nodes** — clients move; their neighbor relationships change. Decaying old edges prevents stale neighbors from polluting the graph.
2. **Network changes** — repeaters are occasionally moved or decommissioned. Decay ensures the graph converges to current topology.

Decay is applied at **query time**, not stored. The raw `count`, `first_seen`, and `last_seen` are stored; the score is computed when the API responds. This avoids background maintenance jobs and ensures freshness.

---

## Algorithm

### Input

- **Packet store** (`PacketStore`) — in-memory, contains all recent transmissions with observations
- **Node registry** — maps pubkeys to node metadata (name, role, hash_size)

### Processing

```
for each transmission T in packet store:
    from_pubkey = T.from_node (full pubkey, known)
    for each observation O of T:
        path = parsePathJSON(O.path_json)
        if len(path) == 0:
            continue  // direct reception, no hops — no neighbor data
        
        first_hop_prefix = path[0]
        candidates = resolve_prefix(first_hop_prefix)
        
        upsert_edge(from_pubkey, first_hop_prefix, candidates, O.observer_id, O.snr, O.timestamp)
```

**`resolve_prefix(prefix)`** looks up all nodes whose pubkey starts with the given prefix (case-insensitive). Returns a list of `(pubkey, name)` tuples.

**`upsert_edge(from, prefix, candidates, observer, snr, timestamp)`**:
- Key: `(from_pubkey, neighbor_prefix, observer_id)`
- If single candidate: set `neighbor_pubkey = candidates[0]`, `ambiguous = false`
- If multiple candidates: set `neighbor_pubkey = null`, `ambiguous = true`, `candidates = [...]`
- Increment `count`, update `last_seen`, running average `avg_snr`

### Disambiguation via graph structure

After building the initial graph, ambiguous edges can be resolved by cross-referencing:

```
for each ambiguous edge E(from=X, prefix="A3", candidates=[a3xx, a3yy]):
    for each candidate C in candidates:
        # How many of X's OTHER known neighbors are also neighbors of C?
        mutual = count(neighbors(X) ∩ neighbors(C))
        C.mutual_score = mutual
    
    if exactly one candidate has mutual_score > 0:
        resolve E → that candidate
    elif max(mutual_scores) >> second_max:
        resolve E → best candidate (with confidence note)
```

This exploits graph transitivity: nodes that share many neighbors are likely in the same physical area and thus likely the correct resolution.

### Output

A neighbor graph with:
- **Nodes**: all MeshCore nodes (pubkey, name, role)
- **Edges**: weighted, undirected relationships with metadata
- **Clusters**: connected components (optional, for analytics)

---

## API Design

### `GET /api/nodes/{pubkey}/neighbors`

Returns the neighbor list for a specific node.

**Query parameters:**
| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `min_count` | int | 1 | Minimum observation count to include |
| `min_score` | float | 0.0 | Minimum affinity score to include |
| `include_ambiguous` | bool | true | Include edges with unresolved collisions |

**Response:**
```json
{
  "node": "c0dedad4208acb6cbe44b848943fc6d3c5d43cf38a21e48b43826a70862980e4",
  "neighbors": [
    {
      "pubkey": "b7e8f9a0...",
      "prefix": "B7",
      "name": "Ridge-Repeater",
      "role": "repeater",
      "count": 847,
      "score": 0.95,
      "first_seen": "2026-03-01T10:00:00Z",
      "last_seen": "2026-04-02T22:15:00Z",
      "avg_snr": -8.2,
      "observers": ["obs-sjc-1", "obs-sjc-2"],
      "ambiguous": false
    },
    {
      "pubkey": null,
      "prefix": "A3",
      "name": null,
      "role": null,
      "count": 12,
      "score": 0.08,
      "first_seen": "2026-03-15T...",
      "last_seen": "2026-03-20T...",
      "avg_snr": -14.1,
      "observers": ["obs-sjc-1"],
      "ambiguous": true,
      "candidates": [
        {"pubkey": "a3b4c5...", "name": "Node-Alpha", "role": "companion"},
        {"pubkey": "a3f0e1...", "name": "Node-Beta", "role": "companion"}
      ]
    }
  ],
  "total_observations": 2341
}
```

### `GET /api/analytics/neighbor-graph`

Returns the full neighbor graph for analytics/visualization.

**Query parameters:**
| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `min_count` | int | 5 | Minimum edge weight |
| `min_score` | float | 0.1 | Minimum affinity score |
| `region` | string | "" | Filter by observer region |
| `role` | string | "" | Filter nodes by role |

**Response:**
```json
{
  "nodes": [
    {
      "pubkey": "c0dedad4...",
      "name": "Kpa-Roof",
      "role": "repeater",
      "neighbor_count": 5
    }
  ],
  "edges": [
    {
      "source": "c0dedad4...",
      "target": "b7e8f9a0...",
      "weight": 847,
      "score": 0.95,
      "bidirectional": true,
      "avg_snr": -8.2,
      "ambiguous": false
    }
  ],
  "stats": {
    "total_nodes": 42,
    "total_edges": 87,
    "ambiguous_edges": 3,
    "avg_cluster_size": 4.2
  }
}
```

### Enhanced hop resolution

Modify the existing `/api/resolve-hops` endpoint (or add a `context` parameter) to accept neighbor-affinity context:

**Request:**
```json
POST /api/resolve-hops
{
  "hops": ["A3", "B7", "C0"],
  "from_node": "deadbeef...",
  "observer": "obs-sjc-1"
}
```

**Enhanced response (new fields):**
```json
{
  "resolved": [
    {
      "prefix": "A3",
      "pubkey": "a3b4c5...",
      "name": "Node-Alpha",
      "confidence": "neighbor_affinity",
      "affinity_score": 0.95,
      "alternatives": []
    },
    {
      "prefix": "B7",
      "pubkey": "b7e8f9...",
      "name": "Ridge-Repeater",
      "confidence": "unique_prefix",
      "affinity_score": null,
      "alternatives": []
    }
  ]
}
```

**Confidence levels:**
- `unique_prefix` — only one node matches the prefix (no collision)
- `neighbor_affinity` — resolved via neighbor graph (score > threshold)
- `ambiguous` — multiple candidates, no clear winner

---

## Storage

### Approach: In-memory graph, computed from packet store

The neighbor graph is **computed in-memory** from the existing `PacketStore` data. No new database table is needed for the initial implementation.

**Rationale:**
- The packet store already holds all path data in memory
- The graph is a derived view — it doesn't contain data that isn't already in the store
- Computing on demand avoids schema migrations and ingestor changes
- The packet store is the single source of truth; the graph stays consistent automatically

### Data structure

```go
type NeighborGraph struct {
    mu    sync.RWMutex
    edges map[edgeKey]*NeighborEdge  // (pubkeyA, pubkeyB) → edge
    byNode map[string][]*NeighborEdge // pubkey → edges involving this node
}

type edgeKey struct {
    A, B string // canonical order: A < B lexicographically
}

type NeighborEdge struct {
    NodeA       string
    NodeB       string    // may be "" if ambiguous (prefix only)
    Prefix      string    // the raw hop prefix that established this edge
    Count       int
    FirstSeen   time.Time
    LastSeen    time.Time
    SNRSum      float64   // for running average
    Observers   map[string]bool
    Ambiguous   bool
    Candidates  []string  // pubkeys, if ambiguous
}
```

### Cache strategy

- **Build**: Computed on first access after server start, or after packet store reload
- **Invalidation**: Rebuild when packet store ingests new data (piggyback on existing `PacketStore.Ingest()`)
- **TTL**: Cache the graph for 60 seconds (configurable). Neighbor relationships change slowly; sub-minute freshness is unnecessary
- **Cost**: Building the graph iterates all transmissions once. With 30K packets, this takes <100ms. Acceptable for a 60s cache

### Future: persistent storage

If the packet store window is too short to build reliable affinity data, a future milestone can add a `node_neighbors` SQLite table (as described in #482) to accumulate observations across server restarts. The API shape stays the same — only the data source changes from in-memory computation to DB query.

---

## Frontend Integration

### Replacing "Show Neighbors" on the map

The current implementation in `map.js` (`selectReferenceNode()`, lines ~748–770):
1. Fetches `/api/nodes/{pubkey}/transmissions`
2. Client-side walks paths to find hops adjacent to the selected node
3. Compares by full pubkey — **fails on collisions** (#484)

**New implementation:**
1. Fetch `GET /api/nodes/{pubkey}/neighbors?min_count=3`
2. Build `neighborPubkeys` set directly from the response
3. No client-side path walking needed
4. Collision disambiguation is handled server-side

```javascript
async function selectReferenceNode(pubkey, name) {
  selectedReferenceNode = pubkey;
  neighborPubkeys = new Set();
  try {
    const resp = await fetch(`/api/nodes/${pubkey}/neighbors?min_count=3`);
    const data = await resp.json();
    for (const n of data.neighbors) {
      if (n.pubkey) neighborPubkeys.add(n.pubkey);
      // For ambiguous edges, add all candidates (better to show extra than miss)
      if (n.candidates) n.candidates.forEach(c => neighborPubkeys.add(c.pubkey));
    }
  } catch (e) {
    console.warn('Failed to fetch neighbors:', e);
    neighborPubkeys = new Set();
  }
  // ... update UI
}
```

This directly fixes #484.

### Node detail page enhancement

Add a "Neighbors" section to the node detail view showing:
- Table of known neighbors with columns: Name, Role, Observations, Last Seen, SNR, Score
- Rows sorted by score descending
- Ambiguous entries shown with a ⚠️ icon and candidate list on hover
- Each neighbor name links to its detail page

### Analytics integration

Add a "Neighbor Graph" sub-tab in the Analytics section:
- Force-directed graph visualization using the `/api/analytics/neighbor-graph` endpoint
- Nodes colored by role (using existing `ROLE_COLORS`)
- Edge thickness proportional to `score`
- Collision groups highlighted (nodes sharing a prefix get matching border colors)
- Click a node to see its neighbors + disambiguation status

This is a later milestone — the API and "Show Neighbors" fix come first.

---

## Testing

### Unit tests for the algorithm

**Go tests** (`cmd/server/neighbor_test.go`):

```
TestBuildNeighborGraph_EmptyStore
    → empty graph, no edges

TestBuildNeighborGraph_SingleHopPath
    → from_node=X, path=["A3"] → edge(X, A3-resolved)

TestBuildNeighborGraph_MultiHopPath
    → from_node=X, path=["A3","B7"] → edge(X, A3-resolved), edge(A3, B7) only if extracting beyond first hop (out of scope — verify NOT created)

TestBuildNeighborGraph_NoPath
    → from_node=X, path=[] → no edges (direct reception)

TestBuildNeighborGraph_HashCollision
    → two nodes share prefix "A3" → edge marked ambiguous with both candidates

TestBuildNeighborGraph_AmbiguityResolution
    → node X has known neighbor Y; Y has known neighbor a3xx but not a3yy → disambiguation resolves to a3xx

TestBuildNeighborGraph_CountAccumulation
    → same edge observed 50 times → count=50, last_seen=latest

TestBuildNeighborGraph_MultipleObservers
    → same edge seen by obs1 and obs2 → observers=["obs1","obs2"]

TestAffinityScore_Fresh
    → count=100, last_seen=now → score ≈ 1.0

TestAffinityScore_Decayed
    → count=100, last_seen=7 days ago → score ≈ 0.5

TestAffinityScore_LowCount
    → count=5, last_seen=now → score ≈ 0.05

TestAffinityScore_StaleAndLow
    → count=5, last_seen=30 days ago → score ≈ 0.0
```

### API tests

```
TestNeighborAPI_ValidNode → 200, returns neighbor list
TestNeighborAPI_UnknownNode → 200, empty neighbors
TestNeighborAPI_MinCountFilter → only edges with count >= min_count
TestNeighborAPI_MinScoreFilter → only edges with score >= min_score
TestNeighborGraphAPI_RegionFilter → only edges from filtered observers
```

### Edge cases

| Case | Expected behavior |
|------|-------------------|
| Single-hop network (all nodes 1 hop from observer) | Every node is a neighbor of the observer; no inter-node edges |
| Hash collision on first hop | Edge marked ambiguous, candidates listed |
| `hash_size == 0` node in path | Still processed (prefix matching works regardless of hash_size) |
| Zero-hop advert (direct reception) | No neighbor edge created; skip gracefully |
| Stale data (node not seen in 30+ days) | Score decays to ~0; filtered out by `min_score` |
| Self-referencing path (`from_node` matches `path[0]`) | Skip — a node cannot be its own neighbor |
| Very long paths (10+ hops) | Only extract first hop; ignore deeper hops |
| Duplicate observations (same observer, same path, same timestamp) | Deduplicated by existing `PacketStore` logic |

---

## What's NOT in scope

- **Full mesh topology visualization** — this spec covers first-hop neighbors only, not multi-hop routing topology
- **Multi-hop path analysis beyond first hop** — extracting `path[1]` ↔ `path[2]` relationships is a natural extension but adds complexity (both endpoints are prefixes, not full pubkeys). Defer to a future issue
- **Real-time graph updates via WebSocket** — the graph is cached and served via REST. WebSocket push for graph changes is unnecessary given the slow rate of topology change
- **Persistent storage in SQLite** — initial implementation is in-memory only. A `node_neighbors` table can be added later if the in-memory window is insufficient
- **Geographic clustering** — while the `neighbor-graph` API response includes a `stats` field, actual geographic cluster detection (e.g., community detection algorithms) is deferred
- **Automatic hop rewriting** — the system provides disambiguation data; it does not retroactively rewrite stored `path_json` values

---

## Implementation Order

1. **Graph builder** — `neighbor_graph.go` with `NeighborGraph` struct, `BuildFromStore()`, scoring functions
2. **Unit tests** — `neighbor_graph_test.go` covering all cases above
3. **API endpoints** — `/api/nodes/{pubkey}/neighbors` and `/api/analytics/neighbor-graph` in `routes.go`
4. **API tests** — route-level tests
5. **Frontend: Show Neighbors fix** — replace client-side path walking with `/neighbors` API call in `map.js`
6. **Frontend: Node detail neighbors section** — add neighbor table to node detail view
7. **Frontend: Analytics graph** (later milestone) — force-directed visualization

Milestones 1–5 fix #484 and deliver the core value. Milestones 6–7 are polish.
