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

// SortURLsByProximity moves the URLs that share a subnet with the client to
// the front. A phone on the WiFi cannot reach the wired address of the same
// machine, so the link handed to it has to be the one on its own subnet.
func SortURLsByProximity(urls []string, clientAddr string) []string {
	host, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		host = clientAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return urls
	}
	out := append([]string(nil), urls...)
	sort.SliceStable(out, func(i, j int) bool {
		return urlRank(out[i], ip) < urlRank(out[j], ip)
	})
	return out
}

// urlRank scores a URL: 0 when it lives in the client's own /24, 1 otherwise.
func urlRank(u string, client net.IP) int {
	host := u
	if i := strings.Index(u, "://"); i >= 0 {
		host = u[i+3:]
	}
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil || client == nil {
		return 1
	}
	c4, i4 := client.To4(), ip.To4()
	if c4 == nil || i4 == nil {
		return 1
	}
	if c4[0] == i4[0] && c4[1] == i4[1] && c4[2] == i4[2] {
		return 0
	}
	return 1
}
