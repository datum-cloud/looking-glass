# Router backend

`looking-glass backend` serves `lookingglass.backend.v1.DiagnosticService` for
one cell. The cell's gateway calls it once per claimed session. The backend
runs the diagnostic on the location's routers through their
[drivers](backend-drivers.md) and streams each router's redacted outcome. The
[v1alpha contract](api/v1alpha-contract.md) ("Backend mapping") defines the
request, the stream and the reason codes. This document covers how the backend
applies them and how to configure it.

## Configuration

`--config` names a YAML file. Unknown fields are rejected. The
`internal/config` package (`Backend`) documents every field and default.

```yaml
cell: cell-a
listen: 0.0.0.0:9090
tls:                          # required unless listen is a loopback address
  certFile: /etc/looking-glass/tls/tls.crt
  keyFile: /etc/looking-glass/tls/tls.key
  clientCAFile: /etc/looking-glass/tls/ca.crt
metrics:
  bindAddress: :8080          # "0" disables /metrics
limits:
  maxRequestLifetime: 120s
  maxRouters: 4               # routers one diagnostic runs on
  maxInventoryRouters: 16     # a larger location is refused (InventoryTooLarge)
  routerTimeout: 75s          # one router's diagnostic, from when it starts
budgets:
  requestsCell: {perSecond: 2, burst: 4}
  requestsProject: {perSecond: 0.2, burst: 2}
  liveDiagnosticsProject: 2
  routerConcurrency: 1
  routerQueueDepth: 4
  routerQueueWait: 10s
destinationPolicy:            # keep equal to the manager's admission policy
  deny: [192.0.2.0/24]        # platform ranges
  allow: []
redaction:
  communities: ["64496:*"]
  largeCommunities: ["64496:1:*"]
locations:
  - name: example-location
    routers:
      - name: edge-1
        platform: frr
        address: 192.0.2.10
        asn: 64496
        settings:
          user: looking-glass
          privateKeyFile: /etc/looking-glass/ssh/id_ed25519
          knownHostsFile: /etc/looking-glass/ssh/known_hosts
          probeSourceIPv4: 198.51.100.10
```

The backend builds every router's driver at startup. A router whose platform
has no driver, or whose settings the driver rejects, stops the backend from
starting. `GetCapabilities` reports the limits and budgets above. For each
location it reports the diagnostics and families that every router there
serves.

With TLS, the backend requires a client certificate that chains to
`clientCAFile`, and accepts TLS 1.3 only. It reads the files at startup, so a
rotated certificate takes effect on restart.

## Refusals before the snapshot

The backend checks a request in this order and refuses it, as a gRPC status
with the contract's reason, before it queries any router:

1. API major and cell (`VersionUnsupported`, `Unauthorized`), then
   `ServiceDisabled` and `LocationNotServed`.
2. The request fields and the query (`InvalidQuery`, `DeadlineExceeded`,
   `QueryTypeUnavailable`).
3. The destination policy (`DestinationNotAllowed`). Ping and traceroute
   destinations go through `admission.DestinationPolicy.Check`, the same code
   admission uses. A route lookup that overlaps a platform range is refused
   through `CheckRouteTarget`.
4. `InventoryTooLarge`, then a `request_id` an admitted request already used
   (`DuplicateRequest`).
5. Budgets (`RateLimited`, with a `google.rpc.RetryInfo` delay): the project's
   live diagnostics, then the cell's and the project's token buckets. A
   refused request takes no token, and its `request_id` stays unused.

## Running a diagnostic

The snapshot lists every router at the location, in configuration order.
Routers whose driver doesn't serve the diagnostic and family are omitted as
`Ineligible`. The others are selected up to `maxRouters`, and the rest are
omitted as `NotRequested`.

Each selected router runs at once, in its own goroutine:

- It waits for one of the router's `routerConcurrency` slots. A full queue
  fails at once with `QueueFull`. A wait longer than `routerQueueWait` fails
  with `QueueTimeout`. Both failures carry scope `router` and retry advice.
- It then runs with `routerTimeout`. A router that has not answered by then
  fails with `Timeout`, even if its driver ignores its context.
- At `expires_at` every unfinished router fails with `DeadlineExceeded`, and
  the stream completes with `DeadlineExceeded`.
- When the caller cancels, every driver call's context is cancelled and the
  stream ends without a complete event.

Ping sends 3 echoes, and traceroute probes up to 20 hops with 3 probes per hop.
Both wait 1 s per reply and use a 56-byte payload. Route lookups return at most
8 paths per prefix and 64 communities of each kind per path. An observation cut
by these bounds is marked `truncated`.

A driver failure keeps its reason when the contract knows it. Any other reason
is reported as `Internal`. The failure message is the driver's sanitized
message, cut to 256 bytes.

## Redaction

Drivers return what the router reports. The backend converts it, and:

- drops router names, router IDs, interface names, and BGP peer addresses and
  descriptions;
- returns a next hop as `internal: true` with no address when it is in a
  `destinationPolicy.deny` range, or is private, shared (100.64.0.0/10),
  loopback, link-local or unspecified;
- returns only communities that match a `redaction` entry, field by field,
  where `*` matches any value; the totals count every community;
- counts internal BGP sessions in the summary's totals and lists only
  external ones;
- fills in the router's `asn` as the origin AS of a path with an empty AS
  path, which the router's own AS originated.

## Metrics

Metrics are created through
[go.miloapis.com/instrumentation](https://github.com/milo-os/instrumentation)
and served at `/metrics` on `metrics.bindAddress`:

| Metric | Labels |
| --- | --- |
| `looking_glass_backend_requests_total` | `diagnostic`, `reason`: the refusal or complete reason |
| `looking_glass_backend_router_outcomes_total` | `location`, `router`, `diagnostic`, `reason`: `Succeeded` or the failure reason |
| `looking_glass_backend_router_call_duration_seconds` | `diagnostic` |
| `looking_glass_backend_live_diagnostics` | none |
