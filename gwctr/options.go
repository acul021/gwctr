package gwctr

import (
	"fmt"
	"net"
	"strconv"
)

const (
	optPrefix     = "de.acul21.gwctr."
	OptBridgeName = optPrefix + "bridge_name"
	OptMTU        = optPrefix + "mtu"
	OptMode       = optPrefix + "mode"
	// OptGatewayIP names a static IPv4 address in the subnet that is held by
	// a container the user runs separately (the "gateway container").
	// All other endpoints on the network get this IP as their default route.
	OptGatewayIP = optPrefix + "gateway_ip"

	// labelInternal is the well-known top-level option key libnetwork sets
	// when a network is created with `--internal` (compose
	// `networks.<name>.internal: true`).
	labelInternal = "com.docker.network.internal"

	ModeNAT  = "nat"
	ModeFlat = "flat"

	defaultMTU         = 1500
	defaultMode        = ModeNAT
	bridgeNamePrefix   = "gwctr"
	bridgeNameMaxBytes = 15
)

type networkOptions struct {
	BridgeName string
	MTU        int
	Mode       string
	GatewayIP  net.IP // optional; nil if unset
	Internal   bool
}

// parseOptions extracts driver-specific options from a CreateNetworkRequest's
// Options map. Docker nests them under "com.docker.network.generic".
func parseOptions(reqOptions map[string]any, networkID string) (*networkOptions, error) {
	opts := &networkOptions{
		BridgeName: defaultBridgeName(networkID),
		MTU:        defaultMTU,
		Mode:       defaultMode,
	}

	raw := flattenGeneric(reqOptions)

	if v, ok := raw[OptBridgeName]; ok && v != "" {
		if len(v) > bridgeNameMaxBytes {
			return nil, fmt.Errorf("bridge name %q exceeds %d bytes", v, bridgeNameMaxBytes)
		}
		opts.BridgeName = v
	}
	if v, ok := raw[OptMTU]; ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid mtu %q", v)
		}
		opts.MTU = n
	}
	if v, ok := raw[OptMode]; ok && v != "" {
		if v != ModeNAT && v != ModeFlat {
			return nil, fmt.Errorf("invalid mode %q (want %q or %q)", v, ModeNAT, ModeFlat)
		}
		opts.Mode = v
	}
	if v, ok := raw[OptGatewayIP]; ok && v != "" {
		ip := net.ParseIP(v)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("invalid %s %q (need IPv4)", OptGatewayIP, v)
		}
		opts.GatewayIP = ip.To4()
	}
	opts.Internal = readBoolOption(reqOptions[labelInternal])

	return opts, nil
}

// readBoolOption coerces the values libnetwork sends for boolean top-level
// options — they arrive as either bool or string depending on the codepath.
func readBoolOption(v any) bool {
	switch vv := v.(type) {
	case bool:
		return vv
	case string:
		switch vv {
		case "true", "1", "yes":
			return true
		}
	}
	return false
}

func flattenGeneric(in map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		switch vv := v.(type) {
		case string:
			out[k] = vv
		case map[string]any:
			for gk, gv := range vv {
				if s, ok := gv.(string); ok {
					out[gk] = s
				}
			}
		case map[string]string:
			for gk, gv := range vv {
				out[gk] = gv
			}
		}
	}
	return out
}

func defaultBridgeName(networkID string) string {
	id := networkID
	if len(id) > 5 {
		id = id[:5]
	}
	return bridgeNamePrefix + id
}
