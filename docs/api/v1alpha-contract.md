# Looking Glass v1alpha API and client contract

Status: normative contract for the alpha API ([#3]). It resolves the open
questions in the [system architecture]. Types, schema, admission and clients
([#5]–[#10], [#16]–[#18]) implement this document. Changing a decision here
requires a revision of this document first.

Every example value is synthetic.

**Revision 2.** Diagnostics run through a backend that Looking Glass owns,
instead of through Galactic's fabric diagnostics. This revision:

- replaces the `FabricEdge` vantage point with `Router`;
- makes the route observation independent of any one routing stack;
- tightens visibility redaction;
- raises the execution deadline to 90 s;
- makes `LookingGlassLocation` the only location API a client needs.

None of the replaced values had shipped.

## Scope

This contract covers the project resource, its inputs, lifecycle and status,
the client connection and stream, location and capability discovery, error
mapping, retention, and permissions. Alpha offers one vantage point, `Router`,
and four diagnostics: route lookup, BGP summary, ping and traceroute. Detached
execution, stored diagnostic output and VPC vantage points are out of scope
([#1]).

Looking Glass owns the whole path: customer authentication, project IAM,
admission, placement, revocation, durable single-use claims, delivery, and
bounded execution, including snapshot membership, coverage arithmetic, budgets
and destination policy. Looking Glass never retries, replays or resumes an
execution attempt.

## Decisions at a glance

| Question | Decision |
| --- | --- |
| API group and version | `lookingglass.datumapis.com/v1alpha`, a dedicated Milo service |
| Resource | `LookingGlassSession`, namespaced in the project control plane, one location and one diagnostic each |
| Vantage point | `Router`: routers in Datum's network at the chosen location. Every diagnostic runs on them |
| Inputs | A discriminated union per diagnostic. Targets are numeric addresses only; Looking Glass resolves no DNS |
| Location discovery | `LookingGlassLocation` only. It carries the location's display data and capabilities. Where the platform sources locations from is not part of the contract |
| Admission vs. failure | Synchronous rejections create no object. Anything after the object exists ends as a terminal `Ready=False` reason |
| Stream | Datum Connect (iroh), then HTTP CONNECT to the gateway, then a WebSocket carrying JSON frames with subprotocol `v1alpha.lookingglass.datumapis.com` |
| Deadlines | Placement 30 s, connection window 60 s, execution 90 s from claim |
| Budgets | Enforced by the backend and advertised as capabilities. Exceeding one ends as `Throttled` |
| Retention | Terminal sessions are kept for 15 minutes so a disconnected client can read the outcome, then deleted |
| Permissions | `create` plus `probe` for ping and traceroute, and `create` plus `readroutes` for route lookup and BGP summary. No default role grants `readroutes` in alpha |
| Limits | 20 session objects per project (quota), and 0.5 creates per second with a burst of 5 per project (admission) |

## API group and registration

The resource is served as `lookingglass.datumapis.com/v1alpha`:

- **Not `network.datumapis.com`.** The architecture's illustrative group
  belonged to the former `network` repository, which Galactic absorbed into
  `galactic.datumapis.com`. Galactic's groups are platform-internal, and
  `networking.datumapis.com` belongs to network-services-operator.
- **A dedicated group** gives Looking Glass its own Milo service, its own
  `ProtectedResource` registrations and its own role names, independent of
  other services' release cycles.
- **`v1alpha`** matches Datum's project-API convention (for example
  `compute.datumapis.com/v1alpha`). Within `v1alpha`, changes are additive
  only; see [Compatibility](#compatibility).

| Kind | Scope | Writable by customers | Purpose |
| --- | --- | --- | --- |
| `LookingGlassSession` | Namespaced, project control plane | create, delete | One diagnostic at one location |
| `LookingGlassLocation` | Cluster-scoped, project control plane, read-only | none | A location that offers Looking Glass, its display data and its capabilities |

Both kinds register a Milo `ProtectedResource` whose parent is
`resourcemanager.miloapis.com/Project`. `LookingGlassSession` has a `status`
subresource that only controllers write.

## LookingGlassSession

### Spec

The whole spec is immutable (`self == oldSelf`). A different request is a new
session, and clients create sessions with `metadata.generateName`.

```yaml
apiVersion: lookingglass.datumapis.com/v1alpha
kind: LookingGlassSession
metadata:
  generateName: trace-
  namespace: default
spec:
  location: example-location
  vantagePoint: Router
  diagnostic:
    type: Traceroute
    traceroute:
      destination: 198.51.100.7
  clientPublicKey: 3b6a27bcceb6a42d62a3a8d02a6f0d73653215771de243a63ac048a18b59da29
```

| Field | Type | Rules |
| --- | --- | --- |
| `location` | string | Required. Name of a `LookingGlassLocation` visible in the project. |
| `vantagePoint` | enum | `Router`, the default and the only value in alpha. |
| `diagnostic.type` | enum | Required. `RouteLookup`, `BGPSummary`, `Ping` or `Traceroute`. Exactly the member named by `type` must be set (CEL). |
| `clientPublicKey` | string | Required. An ed25519 public key as 64 lowercase hex characters. It is also the client's iroh endpoint ID. |

The diagnostic members:

| Member | Fields | Rules and defaults |
| --- | --- | --- |
| `routeLookup` | `destination`, optional `addressFamily` | `destination` is a bare IPv4 or IPv6 address (longest match) or a prefix with no host bits (exact match). `addressFamily` is inferred from `destination`; if set, it must match. Platform infrastructure ranges are rejected. |
| `bgpSummary` | `addressFamily` | Required, `IPv4` or `IPv6`. There is no target. |
| `ping` | `destination` | A numeric unicast IPv4 or IPv6 address. Zones, IPv4-mapped IPv6, hostnames and special-use or platform ranges are rejected. |
| `traceroute` | `destination` | Same rules as `ping`. |

The following are fixed in alpha rather than tunable:

- **Ping** sends 3 ICMP echoes with a 1 s reply wait.
- **Traceroute** probes up to 20 hops, with up to 3 probes per hop and a 1 s
  reply wait. Its worst case on one router is 60 s.
- **Probe payloads** are at most 64 bytes.
- **Probe source.** Each router probes from a fixed source address per
  address family, the same for every session.
- **No name resolution.** Hop and reply addresses are never resolved to
  names.
- **Route output** is at most 8 paths per prefix and 64 communities of each
  kind per path.

**Router selection** is not exposed in alpha. A session runs on every eligible
router at the location, up to the location's advertised `maxRouters`. A
`routers` field may be added later, as an additive change.

**Size bounds** keep objects small: `location` is at most 63 bytes,
`destination` at most 43 bytes, and the whole spec under 1 KiB.

**DNS.** Looking Glass resolves no names. A client may resolve a hostname
locally, but it must show the user the address it submits.

### Status

Every status field is output only. A session waiting to connect:

```yaml
status:
  observedGeneration: 1
  cell: example-cell
  connection:
    endpointID: 9f2c4e1a0b7d3c5e8f6a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5a6
    relayURLs: ["https://relay.example.net"]
    target: lg-gateway.example.internal:8443
  connectBefore: "2026-11-04T12:01:00Z"
  conditions:
    - type: Ready
      status: "True"
      reason: SessionReady
      message: Connect before the deadline.
      observedGeneration: 1
      lastTransitionTime: "2026-11-04T12:00:00Z"
```

A terminal session:

```yaml
status:
  observedGeneration: 1
  cell: example-cell
  startedAt: "2026-11-04T12:00:05Z"
  finishedAt: "2026-11-04T12:00:09Z"
  expiresAt: "2026-11-04T12:15:09Z"
  coverage:
    complete: true
    expected: 3
    selected: 2
    successful: 1
    failed: 1
    omitted: 1
    truncated: 0
  conditions:
    - type: Ready
      status: "False"
      reason: PartialResults
      message: 1 of 2 selected routers answered; 1 router was not eligible.
      observedGeneration: 1
      lastTransitionTime: "2026-11-04T12:00:09Z"
```

| Field | Meaning |
| --- | --- |
| `cell` | The cell the control plane bound the session to, chosen from trusted placement and never from cell-written data. Status from any other cell is ignored. |
| `connection` | Where to connect: `endpointID`, `relayURLs` (nearest first) and `target`. It is set only while `Ready` is `SessionReady`, and it identifies the endpoint without granting access. |
| `connectBefore` | The connection deadline: 60 s after the session became `SessionReady`. |
| `startedAt` | When the gateway durably claimed the session. Execution starts after this. |
| `finishedAt` | When the session reached a terminal reason. |
| `expiresAt` | When the control plane deletes the terminal object: `finishedAt` plus 15 minutes. |
| `retryAfter` | For `Throttled` only: the backend's advisory delay before a *new* session. It is not a reservation. |
| `coverage` | See [Coverage](#coverage). Set once the gateway received the backend's snapshot. |

`Ready` is the only condition type. Its `reason` drives clients.

### Coverage

Coverage copies the backend's terminal `Complete` counts and adds two fields:

- `complete` says whether the backend delivered its terminal `Complete` event.
- `selected` is S, the routers the snapshot selected. It equals
  `successful + failed` only when `complete` is true.

| Field | Meaning |
| --- | --- |
| `expected` | Routers in the snapshot (E). |
| `selected` | Routers the snapshot selected (S). |
| `successful`, `failed` | Outcomes received, by kind. |
| `omitted` | Routers that were not selected (O); E = S + O. |
| `truncated` | Successful observations whose payload is incomplete. |
| `complete` | `true` only when a backend `Complete` was received. When `false`, the counts are the outcomes the gateway actually received; `selected - successful - failed` routers are unresolved, and nothing is inferred about them. |

The snapshot is the set of routers serving the location when the session
started. A router is omitted with reason `NotRequested` when the session did
not ask for it, or `Ineligible` when it is out of service. Unresolved routers
are never reported as failed or omitted.

## Lifecycle

```text
Pending ──► SessionReady ──► Connected ──► terminal (Ready=False, reason fixed)
   │              │               │
   └──────────────┴───────────────┴──► terminal
```

| `Ready` | Reason | Client behavior |
| --- | --- | --- |
| `Unknown` | `Pending` | Placement is in progress. Keep watching. |
| `True` | `SessionReady` | Connect before `connectBefore`. |
| `True` | `Connected` | The session is claimed. Show live frames. |
| `False` | a terminal reason (below) | Stop connection attempts. Show the outcome, keeping frames already received. |

A new session has no status: the API server drops status sent on create, and
the controller then writes `Unknown/Pending`. A client treats a missing
`Ready` condition as `Pending`. A terminal reason never changes. A client
ignores conditions whose `observedGeneration` is older than
`metadata.generation`.

### Terminal reasons

Results:

| Reason | Meaning |
| --- | --- |
| `Succeeded` | Every selected router produced an untruncated observation, and nothing was omitted except as not requested. |
| `PartialResults` | At least one router answered, but some failed, were truncated or were ineligible. |
| `ExecutionFailed` | Routers were selected, but none produced an observation. |
| `NoNodesAvailable` | The snapshot selected no router. |
| `OutputLimitExceeded` | The result exceeded the stream's output budget, or the location's router set exceeded the backend's bound (`InventoryTooLarge`). |

Session failures:

| Reason | Meaning |
| --- | --- |
| `Unavailable` | No cell took the session within 30 s, the cell has the service disabled, the backend was unavailable before execution, or the backend stopped the work itself during execution (service switched off or limits changed). |
| `Refused` | The cell refused a request that admission accepted, for example because cell policy changed after capabilities were published. |
| `Throttled` | A backend budget refused the attempt before execution (`RateLimited`, `QueueFull`). See `retryAfter`. A router that cannot take the work in time fails on its own with `Throttled` or `Timeout`, under `PartialResults` or `ExecutionFailed`. |
| `NotConnected` | No client connected before `connectBefore`. |
| `DeadlineExceeded` | Execution passed its 90 s deadline. |
| `Revoked` | The session was deleted, or the control plane revoked it, before it ended. |
| `Disconnected` | The client connection was lost without a close during execution. |
| `ClosedByUser` | The client closed the stream on purpose before the end. |
| `AgentLost` | The cell component running the session was lost mid-execution, or the bound cell stopped reporting (see [Deadlines](#deadlines)). |
| `BackendError` | The backend failed internally, or returned an unknown reason. |

When several reasons apply, they take precedence in this order:

1. `Revoked`.
2. `DeadlineExceeded`.
3. `AgentLost`.
4. `Disconnected`.
5. `ClosedByUser`, which the client reports and which therefore cannot hide
   an ending the platform started.
6. The backend's own terminal reason.

Once a terminal reason is written, it is never replaced, even by a reason
higher in this list that is detected later.

**Observations are not failures.** Packet loss, unreachable ICMP replies,
silent traceroute hops and an empty route lookup are successful observations.
They never change the terminal reason.

### Deadlines

| Deadline | Value | On expiry |
| --- | --- | --- |
| Placement (create to `SessionReady`) | 30 s | `Unavailable` |
| Connection window (`SessionReady` to claim) | 60 s | `NotConnected` |
| Execution (claim to backend end) | 90 s | `DeadlineExceeded` |
| Terminal retention | 15 min after `finishedAt` | Object deleted |

The execution deadline is passed to the backend as
`expires_at = startedAt + 90 s`. There is no implicit extension.

90 s fits a worst-case traceroute (60 s) on a router, with margin for queueing.
Sessions that target the same location at the same time may queue, and a
router that cannot start in time fails with `Timeout` (`QueueTimeout`), so the
session ends as `PartialResults`. This is expected, and it is reported, never
hidden.

Which component enforces each deadline:

- **Placement:** the control plane sets `Unavailable`.
- **Connection window and execution:** the cell gateway sets `NotConnected`
  and `DeadlineExceeded` on its copy of the session. That status reaches the
  control plane through Karmada.
- **A silent cell:** the control plane enforces a backstop, because a cell
  that stops reporting can never set a reason. The control plane ends a
  session as `AgentLost` if it is still not terminal at
  `connectBefore + 90 s + 60 s`, whatever state it last saw. That is the
  latest moment any claimed session can still be running, plus a 60 s
  margin. A session claimed just before `connectBefore` whose `Connected`
  status has not yet arrived through Karmada is therefore not cut short. The
  control plane does not wait
  for the cell's cleanup confirmation, so the session's retention and quota
  run from that moment.

## Admission vs. asynchronous failure

Every check that can run at create time does, so that the error reaches the
caller synchronously. **A rejected create stores no object, so there is no
status to watch.** Clients must handle both kinds of failure.

**Synchronous**: the API returns an error and creates no object.

| Check | HTTP status | Typical cause |
| --- | --- | --- |
| Schema and CEL validation | 422 Invalid | Malformed address, wrong union member, bad key |
| Project scope | 403 Forbidden | The request was not made in a project control plane |
| Create rate | 429 Too Many Requests, with `Retry-After` | More than the project's create rate |
| Authorization | 403 Forbidden | Missing `create`, `probe` or `readroutes` |
| Location and capability | 422 Invalid | Unknown or unavailable location, capabilities older than 5 minutes, or a vantage point, diagnostic or family the location does not offer |
| Destination policy | 422 Invalid | Special-use, private or platform range |
| Quota | 403 Forbidden (quota exceeded) | Too many session objects in the project |

The admission webhook runs every check above except the schema and quota,
in the order listed. It runs them in mutating admission, before Milo claims
quota, so a refused request never holds quota. It runs them again in
validating admission, except the rate, which is charged once per create.
Dry-run creates are not charged.

The admission check for location and capability reads `LookingGlassLocation`.
The destination check applies the same destination policy that the backend
enforces, so most `DestinationNotAllowed` refusals never reach a cell. For
ping and traceroute it rejects the special-use ranges, IPv6 outside
`2000::/3`, and the deployment's platform ranges. A deployment may also list
explicitly authorized exception ranges. These work only where the cell's
policy allows the same range. For route lookup it rejects platform
infrastructure ranges. BGP summary has no target.

**Create rate.** Each project may create 0.5 sessions per second, with a
burst of 5. The limit is held per webhook replica. It stops create and delete
churn that the object quota alone would allow. The backend's
[budgets](#budgets) still apply to execution and show up as `Throttled`.
Client libraries that honor `Retry-After` retry a 429 on their own.

**Asynchronous**: the object exists and ends with a terminal reason. These
are everything after admission, including cell-side refusals that admission
could not foresee. Such refusals end as `Refused`, `Unavailable` or
`Throttled`.

## Locations and capability discovery

Clients discover where they can run diagnostics by listing
`LookingGlassLocation`s in the project. Each one carries everything a client
needs to choose and show a location: its name, display data and capabilities.
Clients call no other location API.

```yaml
apiVersion: lookingglass.datumapis.com/v1alpha
kind: LookingGlassLocation
metadata:
  name: example-location
status:
  displayName: Example City
  topology:
    cityCode: XMP
    region: example-region
  coordinates:
    latitude: "0.0"
    longitude: "0.0"
  available: true
  vantagePoints:
    - type: Router
      diagnostics:
        - type: RouteLookup
          addressFamilies: [IPv4, IPv6]
        - type: BGPSummary
          addressFamilies: [IPv4, IPv6]
        - type: Ping
          addressFamilies: [IPv4, IPv6]
        - type: Traceroute
          addressFamilies: [IPv4, IPv6]
  limits:
    connectWindow: 60s
    executionTimeout: 90s
    maxRouters: 4
  observedAt: "2026-11-04T11:59:40Z"
```

| Field | Meaning |
| --- | --- |
| `metadata.name` | The location name a session's `spec.location` uses. It is stable for the life of the location. |
| `displayName` | A human-readable name for the location. |
| `topology` | Optional. `cityCode` and `region`, for grouping and filtering. |
| `coordinates` | Optional. `latitude` (-90 to 90) and `longitude` (-180 to 180) in decimal degrees, as strings so that no precision is lost, for maps. |
| `available`, `vantagePoints`, `limits`, `observedAt` | The location's capabilities, below. |

**Location providers.** The controller fills `LookingGlassLocation` from two
sources:

- a *location provider*, which says which locations exist, gives their
  display data, decides which projects see each one, and names the one cell
  that serves it;
- that cell's backend capabilities, which the cell caches for at most 30 s.

The provider is pluggable and not part of this contract. The default provider
reads Milo `locations.miloapis.com` `Location`s; with it, a
`LookingGlassLocation` has the same name and display data as its `Location`.
Other providers, such as a static configuration, may serve deployments without
Milo locations. A client never depends on which provider is in use.

A location without a `LookingGlassLocation`, or with `available: false`,
cannot be used. `available` is `false` when:

- the backend reports `enabled: false`;
- the backend serves no eligible router at the location;
- the backend's API major is not one the gateway implements; or
- capabilities are older than 5 minutes.

Capabilities advertise policy, not router health: a session admitted against
them can still end `Refused` or `NoNodesAvailable`. Customers can only read
`LookingGlassLocation` (`get`, `list`, `watch`), and anyone who may create
sessions may read it. It never contains router names, addresses or revision
strings.

## Connection and stream

### Transport

The client connects exactly as Compute's `InstanceConsoleSession` client does.
Both share one Datum Connect implementation:

1. **Open the tunnel.** Bind an iroh endpoint with the session's private key,
   using relays only, from `connection.relayURLs`. Connect to
   `connection.endpointID` with ALPN `iroh-http-proxy/1`.
2. **Reach the gateway.** Open a stream and send `CONNECT` to
   `connection.target`.
3. **Request the session stream.** Over that connection, send
   `GET /v1alpha/sessions/<metadata.uid>/stream`. The request is a WebSocket
   upgrade with subprotocol `v1alpha.lookingglass.datumapis.com` and these
   headers:
   - `X-Datum-Timestamp`: Unix seconds.
   - `X-Datum-Signature`: a standard base64 ed25519 signature over the
     bytes `datum-lookingglass-v1\n<session UID>\n<unix seconds>`. This is
     the same layout as Compute's `datum-exec-v1` message. The distinct
     prefix stops a console signature from being replayed here, and the
     reverse.

The gateway accepts a timestamp within 30 s of its own clock.

### Authentication and the single-use claim

Before upgrading, the gateway checks these in order:

1. The signature verifies against `spec.clientPublicKey`.
2. The session is bound to this cell.
3. `Ready` is `SessionReady` and `connectBefore` has not passed.
4. The gateway durably claims the session: a compare-and-swap on the cell
   copy's `resourceVersion` sets `startedAt` and `Connected`.

The backend is called only after the claim succeeds. A failed check is
answered before the upgrade, and only a successful claim consumes the session:

| Response | Cause | Session effect |
| --- | --- | --- |
| 401 | Bad signature or timestamp | None |
| 404 | Unknown session, or one not bound to this cell | None |
| 409 | Already claimed (also returned for a replay) | None |
| 410 | Terminal, or past `connectBefore` | None |
| 101 | Claimed | Now `Connected` |

A consumed session never accepts a second connection. A dropped stream is not
resumed: the client watches status for the final reason, and to run again it
creates a new session.

### Frames

Each WebSocket text message is one JSON object, with a `type` and a `seq`.
`seq` starts at 1 and is consecutive. The grammar is:

```text
hello (snapshot outcome*)? end?
```

The server sends all frames. The client sends none: to stop on purpose, it
sends a WebSocket close frame (`1000`), which ends the session as
`ClosedByUser`.

| `type` | Sent | Content |
| --- | --- | --- |
| `hello` | First, once | `session` (UID), `location`, `cell`, `diagnostic` (the spec echo), `deadline` (the execution deadline) |
| `snapshot` | Once, before outcomes | `routers`: each with `router`, `selected`, and `omittedReason` for an omitted router. Built from the backend's `Snapshot`. |
| `outcome` | Once per selected router, in completion order | `router`, `completedAt`, and exactly one of `observation` or `failure` |
| `end` | Last, at most once | `reason` (a terminal reason), `message`, `coverage`, `finishedAt`, and `retryAfter` when `Throttled` |

After `end`, the server closes with `1000`.

**A stream without `end` was not fully delivered**, whatever the close code.
The client keeps the frames it has, marks the remaining selected routers as
unresolved, and reads the terminal reason from status. Status may lag the
stream, because it returns through Karmada. Frames drive live output, and
status is the API outcome of record.

A session that ends before the backend delivers its snapshot is sent as
`hello` followed directly by `end`, with no `snapshot` and no `coverage`. This
covers every row of [the pre-snapshot table](#backend-mapping), not only
refusals.

### Router identity

`router` is an opaque identifier: `r-` followed by the first 16 hex characters
of SHA-256(`<location>/<router name>`). It is stable across sessions at the
same location, so results can be compared, and it never reveals router names
or addresses.

Each observation also carries the router's BGP `asn` and its own
`sampleTime`. `sampleTime` is when that router's read or probe started, not the
snapshot time. Observations from several routers are never merged into one
answer.

### Observations

Observations are Looking Glass types. They are independent of any router's
native output format, so that a change of router software cannot leak into
the customer contract. Durations are protobuf-JSON strings (`"0.012s"`).

Common fields: `asn`, `sampleTime`, `duration` and `truncated`.

| Diagnostic | Payload |
| --- | --- |
| `routeLookup` | `lookupKind` (`Exact` or `LongestMatch`) and `prefixes[]`. Each prefix has `prefix`, `totalPaths` and `paths[]`. |
| `bgpSummary` | `totalPeers`, `establishedPeers`, and `peers[]` for external sessions only. |
| `ping` | `source`, `destination`, `sent`, `received`, `rttMin`, `rttAvg`, `rttMax` and `replies[]` (`sequence`, `address`, `rtt`, `timeout`, `icmpType`, `icmpCode`). |
| `traceroute` | `source`, `destination`, `reached` and `hops[]` (`ttl`, `probes[]` shaped like ping replies). |

**Route paths.** A route lookup reads the router's global routing table, never
an internal routing instance. Paths come from every protocol, not only BGP.
Each path has:

| Field | Meaning |
| --- | --- |
| `protocol` | `BGP`, `Static`, `Connected`, `Local`, `Aggregate`, `IGP` or `Other`. |
| `active` | Whether this is the path the router selected for forwarding. |
| `preference` | The router's preference between protocols. Lower wins. |
| `metric` | The protocol metric, when the protocol has one. |
| `nextHops[]` | Each has `address` and `selected` (forwarding uses it). See the redaction rules below. |
| `bgp` | Set for BGP paths only: `asPath` (segments of `type` and `asns`), `origin`, `originASN`, `localPref`, `med`, `multipath`, `communities` and `largeCommunities` (each with its total). |

**BGP peers.** Each entry in `peers[]` has `remoteAS`, `state`,
`established`, `uptime`, `prefixesReceived` and `prefixesSent`. Internal
sessions are counted in `totalPeers` and `establishedPeers` and are not
listed.

**Probe timing.** Ping and traceroute packets start on the router itself.
Their round-trip times include the router's own processing time, which varies
with its load, so they are not a measure of forwarding latency. Clients say so
next to the results.

**Visibility redaction.** Results show what a customer needs for a diagnosis
and nothing about the platform's internal topology or its partners:

- Router names, router IDs, interface names, peer addresses, peer
  descriptions and hostnames are never returned.
- A next hop inside the platform's infrastructure ranges is returned as
  `internal: true` with no `address`.
- Only communities on Looking Glass's published allowlist are returned. The
  totals still count every community, so a client can show that some were
  withheld.
- Route lookups toward platform infrastructure ranges are refused, at
  admission and again by the backend.

**Empty results are observations.** An empty route lookup is a successful
observation with no prefixes.

### Failures

A router `failure` has these fields:

- `reason`, from the closed set below.
- `message`: the backend's sanitized message, at most 256 bytes. It never
  contains router output.
- `backendReason`: the backend's raw reason code, for support.
- `scope` and `retryAfter`, when the backend supplied them.

| `reason` | Backend reasons |
| --- | --- |
| `Unavailable` | `RouterUnreachable`, `RouterUnavailable` |
| `Refused` | `DestinationNotAllowed`, `InvalidQuery` (the backend enforces destination policy too) |
| `Timeout` | `Timeout`, `QueueTimeout`, `DeadlineExceeded` |
| `Throttled` | `RateLimited`, `QueueFull`, `RouterBusy` |
| `Unsupported` | `QueryTypeUnavailable`, `ProbeSourceUnavailable` |
| `ResponseTooLarge` | `ResponseTooLarge` |
| `Cancelled` | `Cancelled` |
| `BackendError` | `ParseError`, `Internal`, and **any unknown reason** |

Clients render an unknown `reason` as `BackendError`, never as packet loss.

## Backend mapping

In the bound cell, the gateway calls a Looking Glass backend, through the
internal gRPC service `lookingglass.backend.v1.DiagnosticService`. It has two
methods: `GetCapabilities`, which feeds `LookingGlassLocation`, and
`RunDiagnostic`, which streams one diagnostic. The backend runs the
diagnostic on the location's routers and enforces the [budgets](#budgets).
How the backend is deployed and how it reaches routers are not part of this
contract.

**The request.** For each claimed session, the gateway sends exactly one
`RunDiagnostic`:

| Field | Value |
| --- | --- |
| `api_major` | `1` |
| `cell` | `status.cell` |
| `location` | `spec.location` |
| `project_id` | The project UID |
| `request_id` | A fresh lowercase UUID per session |
| `expires_at` | `startedAt + 90 s` |
| `query` | The diagnostic, with the target and family canonicalized |

Application- and transport-level retries and hedging are disabled.

**The stream.** The backend sends one `snapshot`, then one `outcome` per
selected router in completion order, then one `complete`. These become the
frames above. `Complete.reason` maps onto the terminal reason of the same
name, with one exception: `Cancelled` becomes the cause the gateway knows:
`Revoked`, `Disconnected`, `ClosedByUser` or `DeadlineExceeded`. When the
gateway knows no cause, the backend cancelled the work itself (the service
was switched off or its limits changed), and the session ends `Unavailable`.

**Errors before the snapshot.** A gRPC error before the snapshot ends the
session as follows. Each row also notes what to look at, not something to do
automatically:

| Backend reason | Session reason | Look at |
| --- | --- | --- |
| `Unauthorized`, `VersionUnsupported`, `QueryTypeUnavailable`, `InvalidQuery`, `DestinationNotAllowed` | `Refused` | Admission and cell policy drift |
| `ServiceDisabled`, `LocationNotServed`, `RouterUnreachable`, `RouterUnavailable`, `ProbeSourceUnavailable`, and TLS or identity failures | `Unavailable` | Cell operations |
| `Cancelled` | The gateway-known cause, otherwise `Unavailable` | As after the snapshot |
| `RateLimited`, `QueueFull` | `Throttled` (with `retryAfter`) | Project or cell load |
| `InventoryTooLarge`, `ResponseTooLarge`, `OutputLimitExceeded` | `OutputLimitExceeded` | Cell configuration |
| `DeadlineExceeded`, `QueueTimeout` | `DeadlineExceeded` | Load and deadlines |
| `DuplicateRequest`, `ParseError`, `Internal`, and anything unknown | `BackendError` | A bug, to investigate |

**After the snapshot,** the backend reports failures as outcomes and
`complete`. If the transport fails before `complete`, the session ends with
the gateway-known cause (`Disconnected`, `Revoked`, `DeadlineExceeded`), or
else `AgentLost`, with `coverage.complete: false`.

## Retention and deletion

**Diagnostic output is never stored.** Frames go only to the connected
client. Status holds only the lifecycle, the terminal reason and the coverage
counts.

**Terminal sessions are retained for 15 minutes.** After `finishedAt`, the
object stays until `expiresAt` (`finishedAt + 15 m`). A client that lost its
stream, or a portal reloaded after the end, can read the final reason and
coverage during that time. After `expiresAt` the controller deletes the
object.

This deliberately differs from Compute, which deletes a console session as
soon as it ends. Project activity events outlive the object under the
project's audit retention:

- creation, by the requester;
- start and end, by the controller, with the reason and coverage.

**Delete before terminal is revocation.** The controller sets `Revoked` at
once, so watchers learn the outcome without waiting for the cell. It then
asks the cell to cancel. A finalizer waits up to 5 minutes for the cell to
confirm the cancel before it removes the object. This matches Compute's
finalizer bound.

**Delete after terminal** removes the object immediately. Clients may do this
early to release quota.

**Quota** counts session objects, including retained terminal ones. The
default allowance is 20 per project (`config/quota`). Milo claims one unit
when a session is admitted and frees it when the session is deleted. The
backend's [budgets](#budgets) separately limit live sessions and request
rates. Exceeding those shows up as `Throttled`, not as a quota error.

### Budgets

The backend limits live sessions and request rates per project and per cell,
and the work each router takes on at once. The packet counts per diagnostic
are fixed by the [spec](#spec). The limit values are deployment configuration,
not part of this contract.

A request over a project or cell limit ends as `Throttled` before execution,
with `retryAfter`. A router that cannot take the work in time fails on its own,
as `Throttled` or `Timeout`.

## Permissions

`LookingGlassSession` grants the standard verbs `get`, `list`, `watch`,
`create` and `delete`, plus two custom permissions. Admission checks the
custom permissions through a SubjectAccessReview on the session being
created. The review copies the requester's extras, so Milo evaluates it in the
same project. Milo's authorizer evaluates a custom verb as
`lookingglass.datumapis.com/lookingglasssessions.<verb>` when the session's
`ProtectedResource` declares it, and both are declared:

| Permission | Required to create |
| --- | --- |
| `probe` | `Ping` and `Traceroute`, which send traffic from Datum's network |
| `readroutes` | `RouteLookup` and `BGPSummary`, which reveal routing state and external peers |

The roles:

| Role | Grants |
| --- | --- |
| `lookingglass.datumapis.com-viewer` | `get`, `list` and `watch` on sessions and `LookingGlassLocation`s |
| `lookingglass.datumapis.com-prober` | viewer + `create`, `delete`, `probe` |
| `lookingglass.datumapis.com-route-viewer` | viewer + `create`, `delete`, `readroutes` |
| `lookingglass.datumapis.com-admin` | All of the above |

`viewer` can also read the project's quota allowance buckets, so a client can
show how many sessions remain before a create is refused. The roles are in
`config/iam/roles`.

Default project roles:

- **Project owner** inherits `prober` and `viewer`. Milo's only default
  project role is `owner`. The control plane deployment adds Looking Glass to
  it, as it does for other services.
- **No default role includes `readroutes` in alpha.** It is granted
  explicitly, for example to support staff and opted-in alpha projects. The
  product proposal makes customer exposure of global BGP state a launch
  decision. The [redaction rules](#observations) are designed so that a later
  default grant is possible; granting `readroutes` by default is a contract
  revision tracked under [#1]. Because `admin` includes `readroutes`, no
  default role inherits `admin` either.
- If Milo adds default editor or viewer project roles, editor inherits
  `prober` and viewer inherits `viewer`.

## Compatibility

These rules apply within `v1alpha`, both to the resource schema and to the
stream:

- **Changes are additive only.** New optional fields, new frame types, new
  terminal and failure reasons, and new diagnostics. A diagnostic reaches a
  client only when `LookingGlassLocation` advertises it.
- **Clients ignore unknown fields.**
- **An unknown frame `type`** means the client may not claim complete
  coverage, but it continues reading.
- **An unknown terminal reason** is shown as a failure, and **an unknown
  failure reason** as `BackendError`.
- **Breaking changes need a new major.** A change to request semantics,
  identity, counting or lifecycle needs a new subprotocol version (`v1beta`
  or `v1`), and the resource version moves with it.
- **Rollout.** The gateway accepts only subprotocols it implements. During a
  migration the server may accept two of them, and the client offers the
  newest one first.
- **The backend and location provider.** Both are internal. The backend API
  is versioned on its own `api_major`. A change of router platform, backend
  or location provider is absorbed inside Looking Glass without a
  customer-facing change wherever this contract can still be met.

[#1]: https://github.com/datum-cloud/looking-glass/issues/1
[#3]: https://github.com/datum-cloud/looking-glass/issues/3
[#5]: https://github.com/datum-cloud/looking-glass/issues/5
[#10]: https://github.com/datum-cloud/looking-glass/issues/10
[#16]: https://github.com/datum-cloud/looking-glass/issues/16
[#18]: https://github.com/datum-cloud/looking-glass/issues/18
[system architecture]: https://github.com/datum-cloud/enhancements/blob/main/architecture/networking/looking-glass.md
