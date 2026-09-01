package cloudrun

import (
	"net"
	"net/url"
	"strings"
)

// A published container port lives on the machine running the daemon, which is
// not always this one: DOCKER_HOST can name a VM or another host entirely.
// Dialling 127.0.0.1 then reaches the wrong machine, and the readiness wait
// times out on a container that is serving perfectly well.

// daemonHost reads the host a daemon endpoint names, empty when the daemon is
// on this machine.
func daemonHost(endpoint string) string {
	switch {
	case endpoint == "",
		strings.HasPrefix(endpoint, "unix://"),
		strings.HasPrefix(endpoint, "npipe://"):
		return ""
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	// A TCP daemon on loopback is still this machine.
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return ""
	}
	return host
}

// bindIP is the interface a container's port is published on. A remote daemon
// must publish beyond its own loopback or nothing here can reach it; a local
// one stays on loopback so a container is not exposed to the network.
func bindIP(endpoint string) string {
	if daemonHost(endpoint) == "" {
		return "127.0.0.1"
	}
	return "0.0.0.0"
}

// dialHost is where to reach a published port, given what the daemon reported
// as the binding address.
func dialHost(endpoint, bound string) string {
	if host := daemonHost(endpoint); host != "" {
		return host
	}
	if bound == "" || bound == "0.0.0.0" || bound == "::" {
		return "127.0.0.1"
	}
	return bound
}

// hostPort joins a host and port, bracketing an IPv6 address.
func hostPort(host, port string) string { return net.JoinHostPort(host, port) }
