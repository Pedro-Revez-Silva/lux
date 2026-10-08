package egress

import (
	"context"
	"io"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/spec"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

// stub is one Run's DNS server, on its gateway address. It answers A
// queries for allowed names with the addresses the firewall allows, and
// refuses everything else: DNS is not a way out (no tunnelling through
// lookups of arbitrary names).
type stub struct {
	udp *net.UDPConn
	tcp net.Listener
}

// maxTCP caps a Run's concurrent TCP queries; tcpTimeout bounds each.
const (
	maxTCP     = 16
	tcpTimeout = 10 * time.Second
)

// freebind lets the stub bind the gateway address before Podman gives it
// to the bridge (at container start), so the Run's first lookup is served.
var freebind = net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1) })
	if err != nil {
		return err
	}
	return serr
}}

func startStub(gw netip.Addr, f *Firewall, iface string) (*stub, error) {
	addr := netip.AddrPortFrom(gw, StubPort).String()
	pc, err := freebind.ListenPacket(context.Background(), "udp", addr)
	if err != nil {
		return nil, err
	}
	udp := pc.(*net.UDPConn)
	tcp, err := freebind.Listen(context.Background(), "tcp", addr)
	if err != nil {
		udp.Close()
		return nil, err
	}
	s := &stub{udp: udp, tcp: tcp}
	go func() {
		for {
			buf := make([]byte, 1500)
			n, from, err := udp.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			// One goroutine per query: a slow first resolution of one name
			// does not hold up the Run's other lookups.
			go func() {
				if resp := answer(buf[:n], f, iface); resp != nil {
					udp.WriteToUDPAddrPort(resp, from)
				}
			}()
		}
	}()
	go func() {
		slots := make(chan struct{}, maxTCP)
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				go func() { defer func() { <-slots }(); serveTCP(c, f, iface) }()
			default:
				c.Close()
			}
		}
	}()
	return s, nil
}

func serveTCP(c net.Conn, f *Firewall, iface string) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(tcpTimeout))
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return
	}
	msg := make([]byte, int(l[0])<<8|int(l[1]))
	if _, err := io.ReadFull(c, msg); err != nil {
		return
	}
	if resp := answer(msg, f, iface); resp != nil {
		c.Write(append([]byte{byte(len(resp) >> 8), byte(len(resp))}, resp...))
	}
}

func (s *stub) close() {
	s.udp.Close()
	s.tcp.Close()
}

func answer(query []byte, f *Firewall, iface string) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	name := spec.NormalHost(q.Name.String())
	resp := dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true, RecursionDesired: h.RecursionDesired}
	allowed, addrs := f.answerFor(iface, name, q.Type)
	if q.Type != dnsmessage.TypeA {
		// AAAA and others: never an answer (egress is IPv4), but an allowed
		// name exists, and is reported by its A lookup.
		addrs = nil
	}
	switch {
	case !allowed:
		resp.RCode = dnsmessage.RCodeRefused
	case q.Type == dnsmessage.TypeA && len(addrs) == 0:
		resp.RCode = dnsmessage.RCodeNameError
	}
	if q.Type == dnsmessage.TypeA || !allowed {
		l := Lookup{Name: name, Allowed: allowed}
		for _, a := range addrs {
			l.Answers = append(l.Answers, a.String())
		}
		f.report(iface, l)
	}
	b := dnsmessage.NewBuilder(nil, resp)
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	for _, a := range addrs {
		_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: a.As4()})
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}
