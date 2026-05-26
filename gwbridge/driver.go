package gwbridge

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/docker/go-plugins-helpers/network"
)

// NetworkState is the per-network configuration retained for the lifetime of
// a Docker network. GatewayIP, when set, is the static IP held by the
// user-run gateway container; all other endpoints get it as their default
// route. Internal disables NAT and suppresses the gateway container's own
// default route to the bridge.
type NetworkState struct {
	BridgeName string
	MTU        int
	Mode       string
	Internal   bool
	Subnet     *net.IPNet
	Gateway    *net.IPNet // bridge's own gateway IP
	GatewayIP  net.IP     // optional: static IP of the gateway container
}

// endpointState tracks per-endpoint info the driver needs from CreateEndpoint
// onward.
type endpointState struct {
	NetworkID string
	IP        net.IP
}

// Driver is the libnetwork remote-driver implementation.
type Driver struct {
	mu        sync.RWMutex
	networks  map[string]*NetworkState  // networkID -> state
	endpoints map[string]*endpointState // endpointID -> state
}

func NewDriver() (*Driver, error) {
	return &Driver{
		networks:  make(map[string]*NetworkState),
		endpoints: make(map[string]*endpointState),
	}, nil
}

// GetCapabilities advertises the driver scope. Local: single-host.
func (d *Driver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	return &network.CapabilitiesResponse{Scope: network.LocalScope}, nil
}

func (d *Driver) CreateNetwork(req *network.CreateNetworkRequest) error {
	slog.Info("CreateNetwork", "id", req.NetworkID)

	opts, err := parseOptions(req.Options, req.NetworkID)
	if err != nil {
		return err
	}

	subnet, gw, err := pickIPv4(req.IPv4Data)
	if err != nil {
		return err
	}

	if opts.GatewayIP != nil {
		if subnet == nil || !subnet.Contains(opts.GatewayIP) {
			return fmt.Errorf("%s %s is not within subnet %s",
				OptGatewayIP, opts.GatewayIP, subnet)
		}
		if gw != nil && gw.IP.Equal(opts.GatewayIP) {
			return fmt.Errorf("%s %s collides with the bridge gateway %s",
				OptGatewayIP, opts.GatewayIP, gw.IP)
		}
	}

	state := &NetworkState{
		BridgeName: opts.BridgeName,
		MTU:        opts.MTU,
		Mode:       opts.Mode,
		Internal:   opts.Internal,
		Subnet:     subnet,
		Gateway:    gw,
		GatewayIP:  opts.GatewayIP,
	}

	if err := createBridge(state.BridgeName, state.MTU, state.Gateway); err != nil {
		return err
	}
	if err := enableIPForwarding(); err != nil {
		slog.Warn("enable ip_forward failed", "err", err)
	}
	if state.Mode == ModeNAT && !state.Internal {
		if err := installNATRules(state.BridgeName, state.Subnet); err != nil {
			_ = deleteBridge(state.BridgeName)
			return err
		}
	}

	d.mu.Lock()
	d.networks[req.NetworkID] = state
	d.mu.Unlock()
	slog.Info("network created",
		"bridge", state.BridgeName, "mode", state.Mode,
		"internal", state.Internal,
		"subnet", state.Subnet, "gateway_ip", state.GatewayIP)
	return nil
}

func (d *Driver) DeleteNetwork(req *network.DeleteNetworkRequest) error {
	slog.Info("DeleteNetwork", "id", req.NetworkID)

	d.mu.Lock()
	state, ok := d.networks[req.NetworkID]
	delete(d.networks, req.NetworkID)
	d.mu.Unlock()
	if !ok {
		return nil
	}

	if state.Mode == ModeNAT && !state.Internal {
		_ = removeNATRules(state.BridgeName, state.Subnet)
	}
	return deleteBridge(state.BridgeName)
}

func (d *Driver) CreateEndpoint(req *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	slog.Info("CreateEndpoint", "network", req.NetworkID, "endpoint", req.EndpointID)

	if req.Interface == nil || req.Interface.Address == "" {
		return nil, fmt.Errorf("endpoint %s has no IPAM address; this driver requires Docker IPAM to assign IPs",
			req.EndpointID)
	}
	ip, _, err := net.ParseCIDR(req.Interface.Address)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint address %q: %w", req.Interface.Address, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("endpoint address %s is not IPv4", ip)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.networks[req.NetworkID]; !ok {
		return nil, fmt.Errorf("network %s not found", req.NetworkID)
	}
	d.endpoints[req.EndpointID] = &endpointState{
		NetworkID: req.NetworkID,
		IP:        ip4,
	}
	return &network.CreateEndpointResponse{}, nil
}

func (d *Driver) DeleteEndpoint(req *network.DeleteEndpointRequest) error {
	slog.Info("DeleteEndpoint", "endpoint", req.EndpointID)
	d.mu.Lock()
	delete(d.endpoints, req.EndpointID)
	d.mu.Unlock()
	return nil
}

func (d *Driver) EndpointInfo(req *network.InfoRequest) (*network.InfoResponse, error) {
	return &network.InfoResponse{Value: map[string]string{}}, nil
}

func (d *Driver) Join(req *network.JoinRequest) (*network.JoinResponse, error) {
	slog.Info("Join", "network", req.NetworkID, "endpoint", req.EndpointID)

	d.mu.RLock()
	state, ok := d.networks[req.NetworkID]
	ep := d.endpoints[req.EndpointID]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("network %s not found", req.NetworkID)
	}

	hostName, peerName := vethNames(req.EndpointID)
	if _, err := createVethPair(state.BridgeName, hostName, peerName, state.MTU); err != nil {
		return nil, err
	}

	resp := &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   peerName,
			DstPrefix: "eth",
		},
		Gateway: resolveGateway(state, ep),
	}

	var epIP net.IP
	if ep != nil {
		epIP = ep.IP
	}
	slog.Info("Join complete",
		"endpoint", req.EndpointID, "endpoint_ip", epIP,
		"gateway_returned", resp.Gateway)
	return resp, nil
}

func (d *Driver) Leave(req *network.LeaveRequest) error {
	slog.Info("Leave", "endpoint", req.EndpointID)
	hostName, _ := vethNames(req.EndpointID)
	return deleteVeth(hostName)
}

// --- Required by the interface but unused for a local single-host driver. ---

func (d *Driver) AllocateNetwork(*network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	return &network.AllocateNetworkResponse{}, nil
}

func (d *Driver) FreeNetwork(*network.FreeNetworkRequest) error { return nil }

func (d *Driver) DiscoverNew(*network.DiscoveryNotification) error { return nil }

func (d *Driver) DiscoverDelete(*network.DiscoveryNotification) error { return nil }

func (d *Driver) ProgramExternalConnectivity(*network.ProgramExternalConnectivityRequest) error {
	return nil
}

func (d *Driver) RevokeExternalConnectivity(*network.RevokeExternalConnectivityRequest) error {
	return nil
}

// resolveGateway picks the value for JoinResponse.Gateway:
//   - gateway_ip is set and the joining endpoint holds it → this endpoint IS
//     the gateway container. Give it the bridge IP as its own default route
//     (so it can reach upstream via the host's NAT), unless the network is
//     marked internal — in which case install no default route at all.
//   - gateway_ip is set, any other endpoint → return gateway_ip; workloads
//     route through the gateway container.
//   - gateway_ip unset → fall back to the bridge IP for everyone (standard
//     bridge behaviour).
func resolveGateway(state *NetworkState, ep *endpointState) string {
	bridgeIP := ""
	if state.Gateway != nil && state.Gateway.IP != nil {
		bridgeIP = state.Gateway.IP.String()
	}
	if state.GatewayIP == nil {
		return bridgeIP
	}
	if ep != nil && ep.IP != nil && ep.IP.Equal(state.GatewayIP) {
		if state.Internal {
			return ""
		}
		return bridgeIP
	}
	return state.GatewayIP.String()
}

// pickIPv4 selects the first IPv4 IPAM entry from a CreateNetworkRequest.
func pickIPv4(data []*network.IPAMData) (subnet *net.IPNet, gateway *net.IPNet, err error) {
	for _, d := range data {
		if d == nil || d.Pool == "" {
			continue
		}
		_, sn, err := net.ParseCIDR(d.Pool)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid IPAM pool %q: %w", d.Pool, err)
		}
		if sn.IP.To4() == nil {
			continue
		}
		var gw *net.IPNet
		if d.Gateway != "" {
			gip, _, err := net.ParseCIDR(d.Gateway)
			if err == nil {
				gw = &net.IPNet{IP: gip.To4(), Mask: sn.Mask}
			} else if ip := net.ParseIP(d.Gateway); ip != nil {
				gw = &net.IPNet{IP: ip.To4(), Mask: sn.Mask}
			}
		}
		return sn, gw, nil
	}
	return nil, nil, fmt.Errorf("no IPv4 IPAM data; pass --subnet to docker network create")
}

// errFileExists is true when netlink reports an existing object (we treat it
// as idempotent success).
func errFileExists(err error) bool {
	return errors.Is(err, os.ErrExist)
}

// errAs is the generic errors.As wrapper for inline use.
func errAs(err error, target interface{}) bool {
	return errors.As(err, target)
}
