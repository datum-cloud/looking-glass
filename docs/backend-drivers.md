# Backend router drivers

The backend runs each diagnostic on a location's routers through a driver: a
Go implementation of [`driver.Driver`](../pkg/driver/driver.go) for one
router platform. This document describes what a driver must do, how the
backend selects one for each router, and how drivers are packaged.

## What a driver does

A driver runs one diagnostic on one router and returns what the router
reported:

- **Route lookup** reads the router's global routing table: the longest match
  for an address, or the exact prefix for a prefix. It returns every protocol's
  paths, with next hops, protocol preference and metric, and BGP attributes
  for BGP paths.
- **BGP summary** lists the router's sessions for one address family, and says
  which ones are internal to the platform's network.
- **Ping** and **traceroute** probe from the router itself, with the count,
  hop limit, wait and payload size the request gives.

Results are unredacted: peer addresses, the router ID, interface names and
every community. The backend converts each result into the API's observation
types and applies the contract's redaction rules
([v1alpha contract](api/v1alpha-contract.md), "Visibility redaction"). A driver
never decides what a customer may see.

A driver must also:

- **Do no I/O in its factory.** `driver.Factory` validates the router's
  address and settings only, so the backend can reject a bad inventory before
  it contacts any router.
- **Honor the context.** Each call returns promptly once its context is done,
  and `driver.ReasonOf` classifies that error as `Timeout` or `Cancelled`.
- **Return typed failures.** A failure is a `*driver.Error` whose `Reason` is
  one of the backend reason codes, for example `RouterUnreachable` or
  `ParseError`. Its message is safe to show an operator: no credentials and no
  raw router output. Any other error counts as `Internal`.
- **Be safe for concurrent use.** The backend may run several diagnostics on
  one router at once, up to its configured router concurrency.
- **Report its capabilities** without I/O: the diagnostics and address families
  the router serves. The backend advertises only what every eligible router at
  a location serves.

## Selecting a driver

Each router in the backend's inventory names a `platform`, and may carry
platform-specific `settings`. The backend calls `driver.New` for every router
before it serves. `driver.New` looks up the factory registered for the
router's platform. An unregistered platform, or settings the factory rejects,
is a configuration error: the backend refuses to start rather than serve a
location with a router it cannot query.

## Packaging

A driver registers its factory from its package's `init` function:

```go
func init() {
	driver.Register("example-platform", New)
}
```

A binary includes a driver by importing its package for that side effect.

**In-tree drivers** live under `pkg/driver/<platform>`. The backend binary's
main package (`cmd/looking-glass`) imports each of them, so every image built
from this repository includes them:

- [`frr`](../pkg/driver/frr) is the reference driver, for
  [FRRouting](https://frrouting.org) on Linux. It runs `vtysh` JSON commands,
  iputils `ping` and Linux `traceroute` over SSH. Its package documentation
  lists its settings. It refuses a host whose key isn't in the configured
  `known_hosts` file under the router's address as configured, and offers
  ping and traceroute only for a family with a configured probe source. Its
  tests replay output recorded from FRR containers by
  `pkg/driver/frr/testdata/record.sh`, and `pkg/driver/frr/frrtest` lets
  other packages' tests run it the same way.

**Drivers in a separate module** import `go.datum.net/looking-glass/pkg/driver`
and register the same way. Use this for a driver that cannot be published with
this repository. A build that includes one adds the import to the main package
at build time, without changing this repository:

1. A Go workspace (`go.work`) uses this module and the driver's module.
2. A build overlay adds one file to `cmd/looking-glass` that imports the
   driver's package:

   ```go
   package main

   import _ "example.com/drivers/exampleplatform"
   ```

3. The build passes the overlay:

   ```sh
   go build -overlay overlay.json -o looking-glass go.datum.net/looking-glass/cmd/looking-glass
   ```

   where `overlay.json` maps a new file path inside `cmd/looking-glass` to the
   import file:

   ```json
   {"Replace": {"<checkout>/cmd/looking-glass/zz_drivers.go": "<build dir>/drivers.go"}}
   ```

The resulting binary registers the separate module's driver alongside the
in-tree ones. Images built from this repository alone contain only the
in-tree drivers. A separate module's driver must not register a platform
name that an in-tree driver already uses: `driver.Register` panics on a
duplicate at startup.
