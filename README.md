# gwctr — Docker network plugin with container-as-gateway

`gwctr` is a single-host, out-of-process Docker network plugin that manages
a plain Linux bridge per network and lets you point every container's default
route at a designated *gateway IP* on that network. The gateway IP is held by
a container the user runs separately (e.g. a WireGuard or router container).

The plugin is purely declarative: it implements the libnetwork remote-driver
API, returns the desired gateway in each `Join` response, and lets Docker's
own libnetwork install the route inside the container netns. No control plane,
no IPAM plugin, no Swarm, no overlay.

## How it works

1. You create the network with `de.acul21.gwctr.gateway_ip=<IPv4>` —
   this is the static address the gateway container will hold.
2. You start the gateway container pinned to that IP (`docker run --ip ...`
   or compose `ipv4_address`).
3. On every `Join`:
   - The endpoint whose IP equals `gateway_ip` is the gateway container; the
     plugin returns the **bridge's own gateway IP** as that container's
     default route, so the gateway container can reach upstream via the
     host's NAT.
   - Every other endpoint gets `gateway_ip` as its default route.
4. If the network is created with Docker's built-in `--internal` flag
   (compose `networks.<name>.internal: true`), NAT rules are skipped and the
   gateway container is **not** given any default route — everything stays
   on-bridge.

## Architecture

- `main.go` — thin entrypoint; serves the driver over
  `/run/docker/plugins/gwctr.sock`.
- `gwctr/` — driver package:
  - `driver.go` — `Driver`, `NetworkState`, all interface methods.
  - `bridge.go` — `netlink` bridge management + `iptables` NAT/FORWARD rules.
  - `endpoint.go` — veth pair create/attach/delete.
  - `options.go` — network-create options.

## Install

The plugin ships in two forms. Pick one.

### As a managed plugin (recommended)

A Docker [managed plugin][managed-plugins] is an OCI bundle the daemon owns
end-to-end: it's enabled/disabled via `docker plugin …` and survives daemon
restarts without a sidecar container.

```sh
scripts/build-plugin.sh                  # defaults to tag acul21/gwctr:latest
docker plugin enable acul21/gwctr:latest
docker plugin ls
# ID            NAME                          DESCRIPTION                ENABLED
# ...           acul21/gwctr:latest        Docker network plugin ...  true
```

Then use it by its **tag** as the driver name:

```sh
docker network create -d acul21/gwctr:latest \
  --subnet 10.99.0.0/24 --gateway 10.99.0.1 \
  -o de.acul21.gwctr.gateway_ip=10.99.0.2 \
  gwnet
```

To turn on debug logging:

```sh
docker plugin disable acul21/gwctr:latest
docker plugin set    acul21/gwctr:latest args="-debug"
docker plugin enable acul21/gwctr:latest
```

The managed-plugin manifest is `plugin/config.json`:
`docker.networkdriver/1.0` interface on socket `gwctr.sock`, run in the
host network namespace with `CAP_NET_ADMIN` + `CAP_NET_RAW`.

[managed-plugins]: https://docs.docker.com/engine/extend/

### As a legacy socket plugin (development)

Runs the plugin as a privileged container that drops a socket into
`/run/docker/plugins/`. Useful while iterating on the code — `docker compose
up` rebuilds and restarts in seconds.

```sh
./install.sh
ls /run/docker/plugins/gwctr.sock
```

The driver name in this form is `gwctr` (no tag):

```sh
docker network create -d gwctr --subnet 10.99.0.0/24 …
```

## Network options

| Key                                 | Default       | Description                                                                          |
| ----------------------------------- | ------------- | ------------------------------------------------------------------------------------ |
| `de.acul21.gwctr.bridge_name`    | `gwctr<id5>`  | Linux bridge name (≤15 bytes).                                                       |
| `de.acul21.gwctr.mtu`            | `1500`        | Bridge / veth MTU.                                                                   |
| `de.acul21.gwctr.mode`           | `nat`         | `nat` (MASQUERADE + FORWARD on the host) or `flat`.                                  |
| `de.acul21.gwctr.gateway_ip`     | *(unset)*     | IPv4 within the subnet that the gateway container will be pinned to.                 |

The plugin also honors Docker's built-in `--internal` flag (compose
`networks.<name>.internal: true`): NAT rules are skipped and the gateway
container is not given a default route. Workloads still route through it.

`--subnet` is required. `--gateway` is recommended (sets the bridge IP); if
unset, Docker picks the first usable address.

## Usage

```sh
docker network create -d gwctr \
  --subnet 10.99.0.0/24 --gateway 10.99.0.1 \
  -o de.acul21.gwctr.gateway_ip=10.99.0.2 \
  gwnet
```

Start the gateway container pinned to `10.99.0.2`:

```sh
docker run -d --name gw-router \
  --network gwnet --ip 10.99.0.2 \
  --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 \
  alpine:3.20 sh -c \
    'apk add --no-cache iptables >/dev/null && \
     iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE && \
     tail -f /dev/null'
```

Start workloads — no special options:

```sh
docker run -d --name workload-a --network gwnet alpine:3.20 sleep infinity
docker run -d --name workload-b --network gwnet alpine:3.20 sleep infinity
```

### Compose

The included `examples/docker-compose.yml`:

```yaml
services:
  gw-router:
    image: alpine:3.20
    networks:
      gwnet:
        ipv4_address: 10.99.0.2
  workload-a:
    image: alpine:3.20
    networks:
      - gwnet

networks:
  gwnet:
    driver: gwctr
    driver_opts:
      de.acul21.gwctr.gateway_ip: "10.99.0.2"
    ipam:
      config:
        - subnet: 10.99.0.0/24
          gateway: 10.99.0.1
```

Run it:

```sh
docker compose -f examples/docker-compose.yml up -d
```

## Verify

```sh
docker exec workload-a ip route
# default via 10.99.0.2 dev eth0
# 10.99.0.0/24 dev eth0 ...

docker exec workload-a ping -c2 1.1.1.1
# traverses gw-router (MASQUERADE on eth0)

docker exec gw-router ip route
# default via 10.99.0.1 dev eth0    ← bridge gateway, so gw-router has upstream
# 10.99.0.0/24 dev eth0 ...
```

## Internal mode

Pass `--internal` to `docker network create` (or `internal: true` in compose)
and the plugin will:

- skip the host-side NAT POSTROUTING + FORWARD rules, and
- return an empty gateway for the gateway container itself (no default
  route via the bridge IP).

Workloads still get `gateway_ip` as their default route, so all their traffic
goes through the gateway container — but the gateway container has no path
back to the host's upstream.

For internal mode to be useful, the gateway container must be **dual-homed**:
attach it to a second network whose driver does provide upstream (e.g. the
default `bridge` driver, or a separate `gwctr` non-internal network, or a
WireGuard/tunnel interface inside the container itself). The gateway
container then forwards/MASQUERADEs from the internal side to whatever
upstream it has.

A worked example is in [`examples/docker-compose.internal.yml`](examples/docker-compose.internal.yml):
the router is attached to both `gwnet-int` (internal, where workloads live)
and a default-bridge `uplink` network, and runs its own MASQUERADE on the
uplink interface.

## Boundary

The plugin only sets routes. It doesn't configure the gateway container —
that container owns its IP forwarding, NAT/MASQUERADE, upstream tunnel,
firewall rules, and (in internal mode) the dual-homing to a usable
secondary network.

## Operational assumption: "alive ⇒ trusted target"

The plugin does not health-check the gateway container. If the named
`gateway_ip` is in the subnet and a container holds it, every workload's
default route points at it — unconditionally.

This is deliberate. With a pinned static `gateway_ip` there is exactly one
gateway and nothing to fail over to, so detection earns no safety: a perfect
liveness probe would have nowhere to redirect traffic. "Alive ⇒ valid
target" is the design, not a shortcut.

The user-facing consequence is the honest one: if the gateway container is
running but **misconfigured** — `ip_forward` off, the `FORWARD` chain
dropping, or (in internal mode) no working secondary route — workload
traffic black-holes silently. There is no signal from the plugin that this
has happened; verification is the user's responsibility (the recipes in the
**Verify** section above, plus `tcpdump` on the gateway container's
interface, are the tools).

## Caveats

- We treat `JoinResponse.Gateway == ""` as "do not install a default route",
  which matches moby's libnetwork behaviour but is thinly specified in the
  remote-driver protocol.
- `flat` mode is a placeholder — sets up the bridge but does not bind a
  physical NIC.
- IPv6 is not implemented.

## Develop

```sh
go build ./...
go vet ./...
```

The plugin needs `CAP_NET_ADMIN`, `iptables`, and write access to
`/proc/sys/net/ipv4/ip_forward`. The included `docker-compose.yml` runs it
privileged with `network_mode: host`.
