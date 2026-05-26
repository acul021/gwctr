package gwbridge

import (
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
)

// createBridge creates the Linux bridge, assigns the gateway IP from IPAM, and
// brings it up.
func createBridge(name string, mtu int, gatewayCIDR *net.IPNet) error {
	if existing, err := netlink.LinkByName(name); err == nil {
		if _, ok := existing.(*netlink.Bridge); !ok {
			return fmt.Errorf("link %q exists and is not a bridge", name)
		}
		slog.Debug("bridge already exists, reusing", "bridge", name)
	} else {
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu}}
		if err := netlink.LinkAdd(br); err != nil {
			return fmt.Errorf("LinkAdd bridge %q: %w", name, err)
		}
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("LinkByName %q: %w", name, err)
	}

	if gatewayCIDR != nil && gatewayCIDR.IP != nil {
		addr := &netlink.Addr{IPNet: gatewayCIDR}
		if err := netlink.AddrAdd(link, addr); err != nil && !errFileExists(err) {
			return fmt.Errorf("AddrAdd %s on %q: %w", gatewayCIDR, name, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("LinkSetUp %q: %w", name, err)
	}
	return nil
}

func deleteBridge(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var nfe netlink.LinkNotFoundError
		if errAs(err, &nfe) {
			return nil
		}
		return fmt.Errorf("LinkByName %q: %w", name, err)
	}
	if err := netlink.LinkSetDown(link); err != nil {
		slog.Warn("LinkSetDown failed", "bridge", name, "err", err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("LinkDel %q: %w", name, err)
	}
	return nil
}

// enableIPForwarding sets net.ipv4.ip_forward=1. Idempotent.
func enableIPForwarding() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	b, err := os.ReadFile(path)
	if err == nil && len(b) > 0 && b[0] == '1' {
		return nil
	}
	if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	return nil
}

// natRules describes the iptables rules installed for a NAT-mode network.
type natRules struct {
	bridge string
	cidr   string
}

func (r natRules) postroutingArgs() []string {
	return []string{"-s", r.cidr, "!", "-o", r.bridge, "-j", "MASQUERADE"}
}

func (r natRules) forwardSets() [][]string {
	return [][]string{
		{"-i", r.bridge, "-o", r.bridge, "-j", "ACCEPT"},
		{"-i", r.bridge, "!", "-o", r.bridge, "-j", "ACCEPT"},
		{"-o", r.bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
}

func installNATRules(bridge string, subnet *net.IPNet) error {
	if subnet == nil {
		return fmt.Errorf("subnet required for NAT mode")
	}
	ipt, err := iptables.New()
	if err != nil {
		return fmt.Errorf("iptables.New: %w", err)
	}
	r := natRules{bridge: bridge, cidr: subnet.String()}
	if err := ipt.AppendUnique("nat", "POSTROUTING", r.postroutingArgs()...); err != nil {
		return fmt.Errorf("install POSTROUTING MASQUERADE: %w", err)
	}
	for _, args := range r.forwardSets() {
		if err := ipt.AppendUnique("filter", "FORWARD", args...); err != nil {
			return fmt.Errorf("install FORWARD %v: %w", args, err)
		}
	}
	return nil
}

func removeNATRules(bridge string, subnet *net.IPNet) error {
	if subnet == nil {
		return nil
	}
	ipt, err := iptables.New()
	if err != nil {
		return fmt.Errorf("iptables.New: %w", err)
	}
	r := natRules{bridge: bridge, cidr: subnet.String()}
	if err := ipt.DeleteIfExists("nat", "POSTROUTING", r.postroutingArgs()...); err != nil {
		slog.Warn("delete POSTROUTING failed", "err", err)
	}
	for _, args := range r.forwardSets() {
		if err := ipt.DeleteIfExists("filter", "FORWARD", args...); err != nil {
			slog.Warn("delete FORWARD failed", "args", args, "err", err)
		}
	}
	return nil
}
