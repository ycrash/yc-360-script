//go:build windows || linux
// +build windows linux

package capture

import (
	"fmt"
	"github.com/cakturk/go-netstat/netstat"
	"io"
	"net"
	"os"
)

func netStat(udp, tcp, ipv4, ipv6, resolve, all, listening bool, writer io.Writer) (err error) {
	if os.Geteuid() != 0 {
		_, err = fmt.Fprintln(writer, "Not all processes could be identified, you would have to be root to see it all.")
		if err != nil {
			return
		}
	}
	_, err = fmt.Fprintf(writer, "Proto %-23s %-23s %-12s %-16s\n", "Local Addr", "Foreign Addr", "State", "PID/Program name")
	if err != nil {
		return
	}

	if udp {
		if ipv4 {
			tabs, err := netstat.UDPSocks(netstat.NoopFilter)
			if err == nil {
				err = displaySockInfo("udp", tabs, resolve, writer)
				if err != nil {
					return err
				}
			}
		}
		if ipv6 {
			tabs, err := netstat.UDP6Socks(netstat.NoopFilter)
			if err == nil {
				err := displaySockInfo("udp6", tabs, resolve, writer)
				if err != nil {
					return err
				}
			}
		}
	} else {
		tcp = true
	}

	if tcp {
		var fn netstat.AcceptFn

		switch {
		case all:
			fn = func(*netstat.SockTabEntry) bool { return true }
		case listening:
			fn = func(s *netstat.SockTabEntry) bool {
				return s.State == netstat.Listen
			}
		default:
			fn = func(s *netstat.SockTabEntry) bool {
				return s.State != netstat.Listen
			}
		}

		if ipv4 {
			tabs, err := netstat.TCPSocks(fn)
			if err == nil {
				err := displaySockInfo("tcp", tabs, resolve, writer)
				if err != nil {
					return err
				}
			}
		}
		if ipv6 {
			tabs, err := netstat.TCP6Socks(fn)
			if err == nil {
				err := displaySockInfo("tcp6", tabs, resolve, writer)
				if err != nil {
					return err
				}
			}
		}
	}
	return
}

func displaySockInfo(proto string, s []netstat.SockTabEntry, resolve bool, writer io.Writer) (err error) {
	lookup := func(skaddr *netstat.SockAddr) string {
		const IPv4Strlen = 17
		addr := skaddr.IP.String()
		if resolve {
			names, err := net.LookupAddr(addr)
			if err == nil && len(names) > 0 {
				addr = names[0]
			}
		}
		if len(addr) > IPv4Strlen {
			addr = addr[:IPv4Strlen]
		}
		return fmt.Sprintf("%s:%d", addr, skaddr.Port)
	}

	for _, e := range s {
		p := "-"
		if e.Process != nil {
			pn := e.Process.String()
			if len(pn) > 0 {
				p = pn
			}
		}
		saddr := lookup(e.LocalAddr)
		daddr := lookup(e.RemoteAddr)
		state := e.State.String()
		if len(state) <= 0 {
			state = "CLOSE"
		}
		_, err = fmt.Fprintf(writer, "%-5s %-23.23s %-23.23s %-12s %-16s\n", proto, saddr, daddr, state, p)
		if err != nil {
			return
		}
	}
	return
}
