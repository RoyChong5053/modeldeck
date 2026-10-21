package runtime

import (
	"net"
	"sort"
	"strings"
)

// LocalIPs returns the machine's usable IPv4 addresses: every up, non-loopback
// interface except the virtual bridges (docker, libvirt, veth) that nobody
// outside the host can reach.
//
// The list is what the deck uses to print URLs a phone can actually open —
// LAN addresses as well as Tailscale's 100.x ones. It is computed on demand:
// a DHCP lease or a Tailscale reconnect changes the answer.
func LocalIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if virtualIface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}

// virtualIface reports whether an interface name belongs to a virtual bridge
// that only exists inside the host.
func virtualIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "vmnet", "tun", "tap"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ListenURLs expands a listen list into URLs a human can open.
//
// A wildcard bind (0.0.0.0) becomes one URL per local address, so the links
// printed at startup work from a phone on the LAN and over Tailscale, and keep
// working after a DHCP lease change. A specific bind stays as it is.
//
// The token is appended when set, so the link opens the WebUI already
// authenticated.
func ListenURLs(listen, token string) []string {
	var out []string
	for _, addr := range strings.Split(listen, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		switch host {
		case "0.0.0.0", "::", "[::]", "":
			out = append(out, listenURL("127.0.0.1", port, token))
			for _, ip := range LocalIPs() {
				out = append(out, listenURL(ip, port, token))
			}
		default:
			out = append(out, listenURL(host, port, token))
		}
	}
	return out
}

func listenURL(host, port, token string) string {
	u := "http://" + host + ":" + port + "/"
	if token != "" {
		u += "?token=" + token
	}
	return u
}
