# Looking Glass v1alpha API and client contract

Status: normative contract for the alpha API ([#3]). It resolves the open
questions in the [system architecture] and maps the customer-facing contract
onto Galactic's [`LookingGlassService`][galactic-proto] (contract:
[looking-glass-service.md][galactic-contract], inspected at Galactic
`6725505f`). Types, schema, admission and clients ([#5]–[#10], [#16]–[#18])
implement this document. Changing a decision here requires a revision of this
document first.

Every example value is synthetic.

## Scope

This contract covers the project resource, its inputs, lifecycle and status,
the client connection and stream, location and capability discovery, error
mapping from Galactic, retention, and permissions. Alpha offers one vantage
point, `FabricEdge`, and four diagnostics: route lookup, BGP summary, ping and
traceroute. Detached execution, stored diagnostic output and VPC vantage points
are out of scope ([#1]).

Responsibilities split as in the architecture. Looking Glass owns customer
authentication, project IAM, admission, placement, revocation, durable
single-use claims and delivery. Galactic owns bounded execution on fabric
routers, snapshot membership, coverage arithmetic and cell-wide rate and
packet budgets. Looking Glass never retries, replays or resumes a backend
attempt.

## Decisions at a glance

| Question | Decision |
| --- | --- |
| API group and version | `lookingglass.datumapis.com/v1alpha`, a dedicated Milo service |
| Resource | `LookingGlassSession`, namespaced in the project control plane, one location and one diagnostic each |
| Inputs | A discriminated union per diagnostic. Targets are numeric addresses only; Looking Glass resolves no DNS |
| Location discovery | Milo `locations.miloapis.com` `Location`, plus a read-only `LookingGlassLocation` per offered location carrying capabilities |
| Admission vs. failure | Synchronous rejections create no object. Anything after the object exists ends as a terminal `Ready=False` reason |
| Stream | Datum Connect (iroh), then HTTP CONNECT to the gateway, then a WebSocket carrying JSON frames with subprotocol `v1alpha.lookingglass.datumapis.com` |
| Deadlines | Placement 30 s, connection window 60 s, execution 45 s from claim |
| Retention | Terminal sessions are kept for 15 minutes so a disconnected client can read the outcome, then deleted |
| Permissions | `create` plus `probe` for ping and traceroute, and `create` plus `readroutes` for route lookup and BGP summary. No default role grants `readroutes` in alpha |

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
| `LookingGlassLocation` | Cluster-scoped, project control plane, read-only | none | Capabilities a location offers |

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
  vantagePoint: FabricEdge
  diagnostic:
    type: Traceroute
    traceroute:
      destination: 198.51.100.7
  clientPublicKey: 3b6a27bcceb6a42d62a3a8d02a6f0d73653215771de243a63ac048a18b59da29
```

| Field | Type | Rules |
| --- | --- | --- |
| `location` | string | Required. Name of a `Location` visible in the project that has a `LookingGlassLocation` of the same name. |
| `vantagePoint` | enum | `FabricEdge`, the default and the only value in alpha. |
| `diagnostic.type` | enum | Required. `RouteLookup`, `BGPSummary`, `Ping` or `Traceroute`. Exactly the member named by `type` must be set (CEL). |
| `clientPublicKey` | string | Required. An ed25519 public key as 64 lowercase hex characters. It is also the client's iroh endpoint ID. |

The diagnostic members:

| Member | Fields | Rules and defaults |
| --- | --- | --- |
| `routeLookup` | `destination`, optional `addressFamily` | `destination` is a bare IPv4 or IPv6 address (longest match) or a prefix with no host bits (exact match). `addressFamily` is inferred from `destination`; if set, it must match. |
| `bgpSummary` | `addressFamily` | Required, `IPv4` or `IPv6`. There is no target. |
| `ping` | `destination` | A numeric unicast IPv4 or IPv6 address. Zones, IPv4-mapped IPv6, hostnames and special-use or platform ranges are rejected. |
| `traceroute` | `destination` | Same rules as `ping`. |

The following are fixed in alpha rather than tunable, because they are fixed
by Galactic's policy:

- **Ping** sends 3 ICMP echoes with a 1 s reply wait.
- **Traceroute** sends one ICMP probe per hop, up to 20 hops, with a 1 s reply
  wait.
- **Probe payloads** are at most 64 bytes.
- **Route output** is at most 8 paths per prefix and 64 communities of each
  kind per path.

**Router selection** is not exposed in alpha. A session runs on every eligible
router at the location, up to Galactic's 32-router fan-out. A `routers` field
may be added later, as an additive change.

**Size bounds** keep objects small: `location` is at most 63 bytes,
`destination` at most 43 bytes, and the whole spec under 1 KiB.

**DNS.** Looking Glass resolves no names, matching Galactic, which resolves
none either. A client may resolve a hostname locally, but it must show the
user the address it submits.

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
| `retryAfter` | For `Throttled` only: Galactic's advisory delay before a *new* session. It is not a reservation. |
| `coverage` | See [Coverage](#coverage). Set once the gateway received the backend's snapshot. |

`Ready` is the only condition type. Its `reason` drives clients.

### Coverage

Coverage copies Galactic's terminal `Complete` counts and adds two fields:

- `complete` says whether Galactic delivered its terminal `Complete` event.
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

Coverage describes routers in the cell's fabric-router inventory, never "all
routers at the location". Unresolved routers are never reported as failed or
omitted.

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
`Ready` condition as `Pending`. A terminal reason never changes. A client ignores conditions whose `observedGeneration` is older than
`metadata.generation`.

### Terminal reasons

Results:

| Reason | Meaning |
| --- | --- |
| `Succeeded` | Every selected router produced an untruncated observation, and nothing was omitted except as not requested. |
| `PartialResults` | At least one router answered, but some failed, were truncated or were ineligible. |
| `ExecutionFailed` | Routers were selected, but none produced an observation. |
| `NoNodesAvailable` | The snapshot selected no router. |
| `OutputLimitExceeded` | The result exceeded the stream's output budget, or the location's router inventory exceeded Galactic's 256-router bound (`InventoryTooLarge`). |

Session failures:

| Reason | Meaning |
| --- | --- |
| `Unavailable` | No cell took the session within 30 s, the cell has the service disabled, the backend was unavailable before execution, or the cell stopped the work itself during execution (service switched off or limits changed). |
| `Refused` | The cell refused a request that admission accepted, for example because cell policy changed after capabilities were published. |
| `Throttled` | The cell's request-token, live-stream or queue budgets (`Throttled`, `QueueFull`, `NodeBusy`) refused the attempt before execution. See `retryAfter`. A packet-budget shortfall is charged per router at dispatch, so it shows up as a router `Throttled` failure instead, under `PartialResults` or `ExecutionFailed`. |
| `NotConnected` | No client connected before `connectBefore`. |
| `DeadlineExceeded` | Execution passed its 45 s deadline. |
| `Revoked` | The session was deleted, or the control plane revoked it, before it ended. |
| `Disconnected` | The client connection was lost without a close during execution. |
| `ClosedByUser` | The client closed the stream on purpose before the end. |
| `AgentLost` | The cell, gateway or backend authority running the session was lost mid-execution (including Galactic `OwnershipLost`), or the bound cell stopped reporting (see [Deadlines](#deadlines)). |
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
| Execution (claim to backend end) | 45 s | `DeadlineExceeded` |
| Terminal retention | 15 min after `finishedAt` | Object deleted |

The execution deadline is passed to Galactic as `expires_at = startedAt + 45 s`,
within Galactic's 120 s ceiling. There is no implicit extension.

On one router, 45 s fits a traceroute of 20 one-second hops plus Galactic's
10 s queue wait. Alpha fans out to every eligible router, but Galactic gives a
project only 2 node slots. At a location with more than two routers, a slow
traceroute will therefore regularly fail some routers with `Timeout`
(`QueueTimeout`) and end as `PartialResults`. This is expected, and it is
reported, never hidden.

Which component enforces each deadline:

- **Placement:** the control plane sets `Unavailable`.
- **Connection window and execution:** the cell gateway sets `NotConnected`
  and `DeadlineExceeded` on its copy of the session. That status reaches the
  control plane through Karmada.
- **A silent cell:** the control plane enforces a backstop, because a cell
  that stops reporting can never set a reason. The control plane ends a
  session as `AgentLost` if it is still not terminal at
  `connectBefore + 45 s + 60 s`, whatever state it last saw. That is the
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
| Authorization | 403 Forbidden | Missing `create`, `probe` or `readroutes` |
| Location and capability | 422 Invalid | Unknown location, or a diagnostic or family the location does not offer |
| Destination policy | 422 Invalid | Special-use, private or platform range |
| Quota | 403 Forbidden (quota exceeded) | Too many session objects in the project |

The admission check for location and capability reads `LookingGlassLocation`.
The destination check applies the same public destination policy that Galactic
enforces, so most `DestinationNotAllowed` refusals never reach a cell.

**Asynchronous**: the object exists and ends with a terminal reason. These
are everything after admission, including cell-side refusals that admission
could not foresee. Such refusals end as `Refused`, `Unavailable` or
`Throttled`.

## Locations and capability discovery

Clients discover where they can run diagnostics in two steps:

1. **List `Location`s** (`locations.miloapis.com`) projected into the
   project. This is the existing location API: names, topology (city code,
   region) and coordinates.
2. **List `LookingGlassLocation`s.** Each one is named after a `Location` that
   offers Looking Glass. The controller keeps it current from each bound
   cell's Galactic `GetCapabilities`, which the cell caches for at most 30 s.

```yaml
apiVersion: lookingglass.datumapis.com/v1alpha
kind: LookingGlassLocation
metadata:
  name: example-location
status:
  available: true
  vantagePoints:
    - type: FabricEdge
      diagnostics:
        - type: RouteLookup
          addressFamilies: [IPv4, IPv6]
        - type: BGPSummary
          addressFamilies: [IPv4, IPv6]
        - type: Ping
          addressFamilies: [IPv4, IPv6]
        - type: Traceroute
          addressFamilies: [IPv4]
  limits:
    connectWindow: 60s
    executionTimeout: 45s
    maxRouters: 32
  observedAt: "2026-11-04T11:59:40Z"
```

A location without a `LookingGlassLocation`, or with `available: false`,
cannot be used. `available` is `false` when:

- the cell reports `enabled: false`;
- the cell does not serve all three of `router-outcomes`, `terminal-coverage`
  and `aggregate-probe-budgets`; or
- capabilities are older than 5 minutes.

Capabilities advertise policy, not router health: a session admitted against
them can still end `Refused` or `NoNodesAvailable`. Customers can only read
`LookingGlassLocation` (`get`, `list`, `watch`), and anyone who may create
sessions may read it. It never contains router names, addresses or Galactic
revision strings.

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

Galactic is called only after the claim succeeds. A failed check is answered
before the upgrade, and only a successful claim consumes the session:

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
| `snapshot` | Once, before outcomes | `routers`: each with `router`, `selected`, and `omittedReason` for an omitted router. Built from Galactic's `Snapshot`. |
| `outcome` | Once per selected router, in completion order | `router`, `completedAt`, and exactly one of `observation` or `failure` |
| `end` | Last, at most once | `reason` (a terminal reason), `message`, `coverage`, `finishedAt`, and `retryAfter` when `Throttled` |

After `end`, the server closes with `1000`.

**A stream without `end` was not fully delivered**, whatever the close code.
The client keeps the frames it has, marks the remaining selected routers as
unresolved, and reads the terminal reason from status. Status may lag the
stream, because it returns through Karmada. Frames drive live output, and
status is the API outcome of record.

A session that ends before Galactic delivers its snapshot is sent as `hello`
followed directly by `end`, with no `snapshot` and no `coverage`. This covers
every row of [the pre-snapshot table](#galactic-mapping), not only refusals.

### Router identity

`router` is an opaque identifier: `r-` followed by the first 16 hex characters
of SHA-256(`<location>/<node name>`). It is stable across sessions at the same
location, so results can be compared, and it never reveals node or pod names.
Galactic's pod UIDs are not exposed.

Each observation also carries the router's BGP `routerID` and `asn`, and its
own `sampleTime`. `sampleTime` is when that router's read or probe started,
not the snapshot time. Observations from several routers are never merged into
one answer.

### Observations

Observations are Looking Glass types, not Galactic's `fabric.v1` messages
passed through. They copy the fields below, so that a Galactic schema change
cannot leak into the customer contract. Durations are protobuf-JSON strings
(`"0.012s"`).

Common fields: `routerID`, `asn`, `sampleTime`, `duration` and `truncated`.

| Diagnostic | Payload |
| --- | --- |
| `routeLookup` | `lookupKind` (`Exact` or `LongestMatch`) and `prefixes[]`. Each prefix has `prefix`, `totalPaths` and `paths[]`. Each path has `best`, `multipath`, `valid`, `asPath` (segments of `type` and `asns`), `origin`, `originASN`, `localPref`, `med`, `communities`, `largeCommunities` (each with its total) and `nextHops[]` (`address`, `accessible`, `used`). Each prefix also has `installation` (`present`, `routes[]` with `protocol`, `selected`, `installed`, `distance`, `metric` and next hops; plus `error`). |
| `bgpSummary` | `totalPeers`, `establishedPeers` and `peers[]`. Each peer has `address`, `remoteAS`, `localAS`, `state`, `established`, `uptime`, `prefixesReceived`, `prefixesSent`, `messagesReceived`, `messagesSent`, `connectionsEstablished` and `connectionsDropped`. |
| `ping` | `source`, `destination`, `sent`, `received`, `rttMin`, `rttAvg`, `rttMax` and `replies[]` (`sequence`, `address`, `rtt`, `timeout`, `icmpType`, `icmpCode`). |
| `traceroute` | `source`, `destination`, `reached` and `hops[]` (`ttl`, `probes[]` shaped like ping replies). |

**Visibility redaction.** Hostnames, peer descriptions, peer router IDs and
peer hostnames from Galactic are dropped. They name platform and partner
equipment and add nothing a customer needs for a diagnosis.

**Empty results are observations.** An empty route lookup is a successful
observation with no prefixes. When a route lookup's installation read fails,
the path data is still kept and the installation carries an `error`.

### Failures

A router `failure` has these fields:

- `reason`, from the closed set below.
- `message`: Galactic's sanitized message, at most 256 bytes.
- `backendReason`: Galactic's raw reason code, for support.
- `scope` and `retryAfter`, when Galactic supplied them.

| `reason` | Galactic reasons |
| --- | --- |
| `Unavailable` | `NodeUnreachable`, `FRRUnavailable`, `LimiterUnavailable` |
| `Refused` | `DestinationNotAllowed`, `InvalidQuery` (the node enforces destination policy too) |
| `Timeout` | `Timeout`, `QueueTimeout`, `DeadlineExceeded` |
| `Throttled` | `Throttled`, `QueueFull`, `NodeBusy` |
| `Unsupported` | `QueryTypeUnavailable`, `ProbeSourceUnavailable`, `FRRVersionUnsupported` |
| `ResponseTooLarge` | `ResponseTooLarge` |
| `Cancelled` | `Cancelled` |
| `BackendError` | `MalformedResponse`, `Internal`, `ProbeFailed`, and **any unknown reason** |

Clients render an unknown `reason` as `BackendError`, never as packet loss.

## Galactic mapping

The cell gateway is the only Looking Glass caller of
`fabric.service.v1.LookingGlassService`. It authenticates with the cell
service identity
`spiffe://fabric-api.datumapis.com/cell/<cell>/service/looking-glass`.

**The request.** For each claimed session, the gateway sends exactly one
`RunDiagnostic`:

| Field | Value |
| --- | --- |
| `api_major` | `1` |
| `cell` | `status.cell` |
| `project_id` | The project UID |
| `request_id` | A fresh lowercase UUID per session |
| `expires_at` | `startedAt + 45 s` |
| `query` | The diagnostic, with the target and family canonicalized |
| `nodes` | Empty |
| `budgets` | Unset |
| `required_features` | `router-outcomes`, `terminal-coverage`, `aggregate-probe-budgets` |

Application- and transport-level retries and hedging are disabled.

**The stream.** Galactic's `snapshot`, `outcome` and `complete` events become
the frames above. `Complete.reason` maps onto the terminal reason of the same
name, with two exceptions:

- `OwnershipLost` becomes `AgentLost`.
- `Cancelled` becomes the cause the gateway knows: `Revoked`, `Disconnected`,
  `ClosedByUser` or `DeadlineExceeded`. When the gateway knows no cause,
  Galactic cancelled the work itself (the service was switched off or its
  limits changed), and the session ends `Unavailable`.

**Errors before the snapshot.** A gRPC error before the snapshot ends the
session as follows. Each row also notes what to look at, not something to do
automatically:

| Galactic reason | Session reason | Look at |
| --- | --- | --- |
| `Unauthorized`, `VersionUnsupported`, `QueryTypeUnavailable`, `InvalidQuery`, `DestinationNotAllowed` | `Refused` | Admission and cell policy drift |
| `ServiceDisabled`, `LimiterUnavailable`, `NodeUnreachable`, `FRRUnavailable`, `ProbeSourceUnavailable`, `FRRVersionUnsupported`, and TLS or identity failures | `Unavailable` | Cell operations |
| `Cancelled` | The gateway-known cause, otherwise `Unavailable` | As after the snapshot |
| `Throttled`, `QueueFull`, `NodeBusy` | `Throttled` (with `retryAfter`) | Project or cell load |
| `InventoryTooLarge`, `ResponseTooLarge`, `OutputLimitExceeded` | `OutputLimitExceeded` | Cell configuration |
| `DeadlineExceeded`, `QueueTimeout` | `DeadlineExceeded` | Load and deadlines |
| `OwnershipLost` | `AgentLost` | Gateway leadership |
| `DuplicateRequest`, `DuplicateConflict`, `MalformedResponse`, `Internal`, `ProbeFailed`, and anything unknown | `BackendError` | A bug, to investigate |

**After the snapshot,** Galactic reports failures as outcomes and `complete`.
If the transport fails before `complete`, the session ends with the
gateway-known cause (`Disconnected`, `Revoked`, `DeadlineExceeded`), or else
`AgentLost`, with `coverage.complete: false`.

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
default allowance is 20 per project. Galactic separately limits each project
to 2 live streams per cell, and to 0.2 requests per second with a burst of 2.
Exceeding those limits shows up as `Throttled`, not as a quota error.

## Permissions

`LookingGlassSession` grants the standard verbs `get`, `list`, `watch`,
`create` and `delete`, plus two custom permissions. Admission checks the
custom permissions through a SubjectAccessReview on the session being
created:

| Permission | Required to create |
| --- | --- |
| `probe` | `Ping` and `Traceroute`, which send traffic from Datum's network |
| `readroutes` | `RouteLookup` and `BGPSummary`, which reveal routing state and fabric peers |

The roles:

| Role | Grants |
| --- | --- |
| `lookingglass.datumapis.com-viewer` | `get`, `list` and `watch` on sessions and `LookingGlassLocation`s |
| `lookingglass.datumapis.com-prober` | viewer + `create`, `delete`, `probe` |
| `lookingglass.datumapis.com-route-viewer` | viewer + `create`, `delete`, `readroutes` |
| `lookingglass.datumapis.com-admin` | All of the above |

Default project roles:

- **Project editor and owner** include `prober`.
- **No default role includes `readroutes` in alpha.** It is granted
  explicitly, for example to support staff and opted-in alpha projects. The
  product proposal makes customer exposure of global BGP state a launch
  decision; granting `readroutes` by default is a contract revision tracked
  under [#1].
- **Project viewer** includes `viewer`.

Implementation check for [#6]: confirm that Milo's `ProtectedResource` and
authorization webhook evaluate custom permission names in a
SubjectAccessReview. If they do not, model the two classes as separate
subresources (`sessions/probe` and `sessions/routes`) with `create`.

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
- **Galactic.** The gateway pins `go.datum.net/galactic` by tag and requires
  `api_major` 1. A Galactic major change is absorbed in the gateway without a
  customer-facing change wherever this contract can still be met.

[#1]: https://github.com/datum-cloud/looking-glass/issues/1
[#3]: https://github.com/datum-cloud/looking-glass/issues/3
[#5]: https://github.com/datum-cloud/looking-glass/issues/5
[#6]: https://github.com/datum-cloud/looking-glass/issues/6
[#10]: https://github.com/datum-cloud/looking-glass/issues/10
[#16]: https://github.com/datum-cloud/looking-glass/issues/16
[#18]: https://github.com/datum-cloud/looking-glass/issues/18
[system architecture]: https://github.com/datum-cloud/enhancements/blob/main/architecture/networking/looking-glass.md
[galactic-proto]: https://github.com/datum-cloud/galactic/blob/6725505f1a1b9fd19bd8b985ad11798663c779d6/api/fabric/service/v1/service.proto
[galactic-contract]: https://github.com/datum-cloud/galactic/blob/6725505f1a1b9fd19bd8b985ad11798663c779d6/docs/designs/looking-glass-service.md
