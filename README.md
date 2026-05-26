# gwbridge — Docker network plugin with container-as-gateway

`gwbridge` is a single-host, out-of-process Docker network plugin that manages
a plain Linux bridge per network and lets you point every container's default
route at a designated *gateway IP* on that network. The gateway IP is held by
a container the user runs separately (e.g. a WireGuard or router container).

The plugin is purely declarative: it implements the libnetwork remote-driver
API, returns the desired gateway in each `Join` response, and lets Docker's
own libnetwork install the route inside the container netns. No control plane,
no IPAM plugin, no Swarm, no overlay.

## How it works

1. You create the network with `de.acul21.gwbridge.gateway_ip=<IPv4>` —
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
  `/run/docker/plugins/gwbridge.sock`.
- `gwbridge/` — driver package:
  - `driver.go` — `Driver`, `NetworkState`, all interface methods.
  - `bridge.go` — `netlink` bridge management + `iptables` NAT/FORWARD rules.
  - `endpoint.go` — veth pair create/attach/delete.
  - `options.go` — network-create options.

## Install

```sh
./install.sh
```

Builds the plugin image and runs it privileged on the host with
`/run/docker/plugins` mounted in.

```sh
ls /run/docker/plugins/gwbridge.sock
```

## Network options

| Key                                 | Default       | Description                                                                          |
| ----------------------------------- | ------------- | ------------------------------------------------------------------------------------ |
| `de.acul21.gwbridge.bridge_name`    | `gwb<id5>`    | Linux bridge name (≤15 bytes).                                                       |
| `de.acul21.gwbridge.mtu`            | `1500`        | Bridge / veth MTU.                                                                   |
| `de.acul21.gwbridge.mode`           | `nat`         | `nat` (MASQUERADE + FORWARD on the host) or `flat`.                                  |
| `de.acul21.gwbridge.gateway_ip`     | *(unset)*     | IPv4 within the subnet that the gateway container will be pinned to.                 |

The plugin also honors Docker's built-in `--internal` flag (compose
`networks.<name>.internal: true`): NAT rules are skipped and the gateway
container is not given a default route. Workloads still route through it.

`--subnet` is required. `--gateway` is recommended (sets the bridge IP); if
unset, Docker picks the first usable address.

## Usage

```sh
docker network create -d gwbridge \
  --subnet 10.99.0.0/24 --gateway 10.99.0.1 \
  -o de.acul21.gwbridge.gateway_ip=10.99.0.2 \
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
    driver: gwbridge
    driver_opts:
      de.acul21.gwbridge.gateway_ip: "10.99.0.2"
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

Pass `--internal` to `docker network create` (or `internal: true` in compose)
and `gw-router` will not get a default route — useful for offline / lab
networks.

## Boundary

The plugin only sets routes. It doesn't configure the gateway container —
that container is responsible for its own IP forwarding, MASQUERADE / upstream
tunnel, firewall rules, etc.

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
