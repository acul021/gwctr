package gwbridge

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

// vethNames returns the host-side and container-side veth names for an
// endpoint. The host side is what we attach to the bridge; the peer is what
// libnetwork moves into the container netns and renames using DstPrefix.
func vethNames(endpointID string) (host, peer string) {
	id := endpointID
	if len(id) > 5 {
		id = id[:5]
	}
	return "gwh" + id, "gwc" + id
}

// createVethPair creates the veth pair, attaches host side to the bridge, and
// brings both ends up.
func createVethPair(bridgeName, hostName, peerName string, mtu int) (string, error) {
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return "", fmt.Errorf("bridge %q not found: %w", bridgeName, err)
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostName, MTU: mtu},
		PeerName:  peerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return "", fmt.Errorf("LinkAdd veth %q<->%q: %w", hostName, peerName, err)
	}

	host, err := netlink.LinkByName(hostName)
	if err != nil {
		_ = netlink.LinkDel(veth)
		return "", fmt.Errorf("LinkByName %q: %w", hostName, err)
	}
	if err := netlink.LinkSetMaster(host, br); err != nil {
		_ = netlink.LinkDel(host)
		return "", fmt.Errorf("LinkSetMaster %q -> %q: %w", hostName, bridgeName, err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		_ = netlink.LinkDel(host)
		return "", fmt.Errorf("LinkSetUp %q: %w", hostName, err)
	}
	return peerName, nil
}

func deleteVeth(hostName string) error {
	link, err := netlink.LinkByName(hostName)
	if err != nil {
		var nfe netlink.LinkNotFoundError
		if errAs(err, &nfe) {
			return nil
		}
		return fmt.Errorf("LinkByName %q: %w", hostName, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("LinkDel %q: %w", hostName, err)
	}
	return nil
}
