package mockdns

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"time"

	"github.com/miekg/dns"
)

// Server is the wrapper that binds Resolver to the DNS server implementation
// from github.com/miekg/dns. This allows it to be used as a replacement
// resolver for testing code that doesn't support DNS callbacks. See PatchNet.
type Server struct {
	r       Resolver
	stopped bool
	tcpServ dns.Server
	udpServ dns.Server

	Log Logger
}

type Logger interface {
	Printf(f string, args ...interface{})
}

func NewServer(zones map[string]Zone) (*Server, error) {
	s := &Server{
		r: Resolver{
			Zones: zones,
		},
		tcpServ: dns.Server{Addr: "127.0.0.1:0", Net: "tcp"},
		udpServ: dns.Server{Addr: "127.0.0.1:0", Net: "udp"},
	}

	pconn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	// Use same endpoint for TCP for simplicity.
	tcpL, err := net.Listen("tcp4", pconn.LocalAddr().String())
	if err != nil {
		return nil, err
	}

	s.tcpServ.Listener = tcpL
	s.tcpServ.Handler = s
	s.udpServ.PacketConn = pconn
	s.udpServ.Handler = s

	go s.tcpServ.ActivateAndServe()
	go s.udpServ.ActivateAndServe()

	s.Log = log.New(os.Stderr, "mockdns server: ", log.LstdFlags)

	return s, nil
}

func (s *Server) writeErr(w dns.ResponseWriter, reply *dns.Msg, err error) {
	reply.Rcode = dns.RcodeServerFailure
	reply.RecursionAvailable = false
	reply.Answer = nil
	reply.Extra = nil

	if dnsErr, ok := err.(*net.DNSError); ok {
		if isNotFound(dnsErr) {
			reply.Rcode = dns.RcodeNameError
			reply.RecursionAvailable = true
			reply.Ns = []dns.RR{
				&dns.SOA{
					Hdr: dns.RR_Header{
						Name:   dnsErr.Name,
						Rrtype: dns.TypeSOA,
						Class:  dns.ClassINET,
						Ttl:    9999,
					},
					Ns:      "localhost.",
					Mbox:    "hostmaster.localhost.",
					Serial:  1,
					Refresh: 900,
					Retry:   900,
					Expire:  1800,
					Minttl:  60,
				},
			}
		}
	} else {
		s.Log.Printf("lookup error: %v", err)
	}

	w.WriteMsg(reply)
}

func mkCname(name, cname string) *dns.CNAME {
	return &dns.CNAME{
		Hdr: dns.RR_Header{
			Name:   name,
			Rrtype: dns.TypeCNAME,
			Class:  dns.ClassINET,
			Ttl:    9999,
		},
		Target: cname,
	}
}

func splitTXT(s string) []string {
	const maxLen = 255

	parts := make([]string, 0, len(s)/maxLen+1)

	min := func(i, j int) int {
		if i < j {
			return i
		}
		return j
	}

	for i := 0; i < len(s)/maxLen+1; i++ {
		p := s[i*maxLen : min(len(s), (i+1)*maxLen)]
		parts = append(parts, p)
	}

	return parts
}

// ServerDNS implements miekg/dns.Handler. It responds with values from underlying
// Resolver object.
func (s *Server) ServeDNS(w dns.ResponseWriter, m *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetReply(m)
	reply.RecursionAvailable = true

	q := m.Question[0]

	// This does the lookup twice (including lookup* below).
	// TODO: Avoid this.
	_, rzone, err := s.r.targetZone(q.Name)
	if err != nil {
		s.writeErr(w, reply, err)
		return
	}
	if rzone.AD {
		reply.AuthenticatedData = true
	}

	switch q.Qtype {
	case dns.TypeA:
		cname, addrs, err := s.r.lookupA(context.Background(), q.Name)
		if err != nil {
			s.writeErr(w, reply, err)
			return
		}

		if cname != "" {
			reply.Answer = append(reply.Answer, mkCname(q.Name, cname))
		}
		for _, addr := range addrs {
			parsed := net.ParseIP(addr)
			if parsed == nil {
				panic("ServeDNS: malformed IP in records")
			}
			reply.Answer = append(reply.Answer, &dns.A{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    9999,
				},
				A: parsed,
			})
		}
	case dns.TypeAAAA:
		cname, addrs, err := s.r.lookupAAAA(context.Background(), q.Name)
		if err != nil {
			s.writeErr(w, reply, err)
			return
		}

		if cname != "" {
			reply.Answer = append(reply.Answer, mkCname(q.Name, cname))
		}
		for _, addr := range addrs {
			parsed := net.ParseIP(addr)
			if parsed == nil {
				panic("ServeDNS: malformed IP in records")
			}
			reply.Answer = append(reply.Answer, &dns.AAAA{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeAAAA,
					Class:  dns.ClassINET,
					Ttl:    9999,
				},
				AAAA: parsed,
			})
		}
	case dns.TypeMX:
		cname, mxs, err := s.r.lookupMX(context.Background(), q.Name)
		if err != nil {
			s.writeErr(w, reply, err)
			return
		}

		if cname != "" {
			reply.Answer = append(reply.Answer, mkCname(q.Name, cname))
		}
		for _, mx := range mxs {
			reply.Answer = append(reply.Answer, &dns.MX{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeMX,
					Class:  dns.ClassINET,
					Ttl:    9999,
				},
				Preference: mx.Pref,
				Mx:         mx.Host,
			})
		}
	case dns.TypeNS:
		cname, nss, err := s.r.lookupNS(context.Background(), q.Name)
		if err != nil {
			s.writeErr(w, reply, err)
			return
		}

		if cname != "" {
			reply.Answer = append(reply.Answer, mkCname(q.Name, cname))
		}
		for _, ns := range nss {
			reply.Answer = append(reply.Answer, &dns.NS{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeNS,
					Class:  dns.ClassINET,
					Ttl:    9999,
				},
				Ns: ns.Host,
			})
		}
	case dns.TypeSRV:
		cname, srvs, err := s.r.lookupSRV(context.Background(), q.Name)
		if err != nil {
			s.writeErr(w, reply, err)
			return
		}

		if cname != "" {
			reply.Answer = append(reply.Answer, mkCname(q.Name, cname))
		}
		for _, srv := range srvs {
			reply.Answer = append(reply.Answer, &dns.SRV{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeSRV,
					Class:  dns.ClassINET,
					Ttl:    9999,
				},
				Priority: srv.Priority,
				Port:     srv.Port,
				Target:   srv.Target,
			})
		}
	case dns.TypeCNAME:
		cname, err := s.r.LookupCNAME(context.Background(), q.Name)
		if err != nil {
			s.writeErr(w, reply, err)
			return
		}

		if cname != "" {
			reply.Answer = append(reply.Answer, mkCname(q.Name, cname))
		}
	case dns.TypeTXT:
		cname, txts, err := s.r.lookupTXT(context.Background(), q.Name)
		if err != nil {
			s.writeErr(w, reply, err)
			return
		}

		if cname != "" {
			reply.Answer = append(reply.Answer, mkCname(q.Name, cname))
		}
		for _, txt := range txts {
			reply.Answer = append(reply.Answer, &dns.TXT{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeTXT,
					Class:  dns.ClassINET,
					Ttl:    9999,
				},
				Txt: splitTXT(txt),
			})
		}
	case dns.TypePTR:
		rzone, ok := s.r.Zones[q.Name]
		if !ok {
			s.writeErr(w, reply, notFound(q.Name))
			return
		}

		for _, name := range rzone.PTR {
			reply.Answer = append(reply.Answer, &dns.PTR{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypePTR,
					Class:  dns.ClassINET,
					Ttl:    9999,
				},
				Ptr: name,
			})
		}
	case dns.TypeSOA:
		reply.Answer = []dns.RR{
			&dns.SOA{
				Hdr: dns.RR_Header{
					Name:   q.Name,
					Rrtype: dns.TypeSOA,
					Class:  dns.ClassINET,
					Ttl:    9999,
				},
				Ns:      "localhost.",
				Mbox:    "hostmaster.localhost.",
				Serial:  1,
				Refresh: 900,
				Retry:   900,
				Expire:  1800,
				Minttl:  60,
			},
		}
	default:
		rzone, ok := s.r.Zones[q.Name]
		if !ok {
			s.writeErr(w, reply, notFound(q.Name))
			return
		}

		reply.Answer = append(reply.Answer, rzone.Misc[dns.Type(q.Qtype)]...)
	}

	if err := w.WriteMsg(reply); err != nil {
		s.Log.Printf("WriteMsg: %v", err)
	}
}

// LocalAddr returns the local endpoint used by the server. It will always be
// *net.UDPAddr, however it is also usable for TCP connections.
func (s *Server) LocalAddr() net.Addr {
	return s.udpServ.PacketConn.LocalAddr()
}

// PatchNet configures net.Resolver instance to use this Server object.
//
// Use UnpatchNet to revert changes.
func (s *Server) PatchNet(r *net.Resolver) {
	r.PreferGo = true
	r.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if s.stopped {
			return nil, errors.New("Patched resolver is used after Server.Close")
		}

		dialer := net.Dialer{
			// This is localhost, it is either running or not. Fail quickly if
			// we can't connect.
			Timeout: 1 * time.Second,
		}

		switch network {
		case "udp", "udp4", "udp6":
			return dialer.DialContext(ctx, "udp4", s.udpServ.PacketConn.LocalAddr().String())
		case "tcp", "tcp4", "tcp6":
			return dialer.DialContext(ctx, "tcp4", s.tcpServ.Listener.Addr().String())
		default:
			panic("PatchNet.Dial: unknown network")
		}
	}
}

func UnpatchNet(r *net.Resolver) {
	r.PreferGo = false
	r.Dial = nil
}

// Resolver returns the underlying Resolver object that can be used directly
// to access Zones content.
func (s *Server) Resolver() *Resolver {
	return &s.r
}

func (s *Server) Close() error {
	s.tcpServ.Shutdown()
	s.udpServ.Shutdown()
	s.stopped = true
	return nil
}
