package dhcp4

import (
	"fmt"
	"net"
	"sync"

	"golang.org/x/net/ipv4"
)

type msg struct {
	n    int
	addr net.Addr
	err  error
	buf  []byte
}

type serveIfConn struct {
	ifIndex int
	conn    *ipv4.PacketConn
	cm      *ipv4.ControlMessage
}

type serveIfConnMulti struct {
	ifIndex int
	conn    *ipv4.PacketConn
	ch      chan msg
}

func (s *serveIfConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	n, s.cm, addr, err = s.conn.ReadFrom(b)
	if s.cm != nil && s.cm.IfIndex != s.ifIndex { // Filter all other interfaces
		n = 0 // Packets < 240 are filtered in Serve().
	}
	return
}

func (s *serveIfConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {

	// ipv4 docs state that Src is "specify only", however testing by tfheen
	// shows that Src IS populated.  Therefore, to reuse the control message,
	// we set Src to nil to avoid the error "write udp4: invalid argument"
	s.cm.Src = nil

	return s.conn.WriteTo(b, s.cm, addr)
}

func (s *serveIfConnMulti) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	m := <-s.ch

	n = m.n
	addr = m.addr
	err = m.err

	copy(b, m.buf)

	return
}

func (s *serveIfConnMulti) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	cm := new(ipv4.ControlMessage)
	cm.IfIndex = s.ifIndex

	return s.conn.WriteTo(b, cm, addr)
}

// ServeIf does the same job as Serve(), but listens and responds on the
// specified network interface (by index).  It also doubles as an example of
// how to leverage the dhcp4.ServeConn interface.
//
// If your target only has one interface, use Serve(). ServeIf() requires an
// import outside the std library.  Serving DHCP over multiple interfaces will
// require your own dhcp4.ServeConn, as listening to broadcasts utilises all
// interfaces (so you cannot have more than on listener).
func ServeIf(ifIndex int, conn net.PacketConn, handler Handler) error {
	p := ipv4.NewPacketConn(conn)
	if err := p.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return err
	}
	return Serve(&serveIfConn{ifIndex: ifIndex, conn: p}, handler)
}

// ListenAndServe listens on the UDP network address addr and then calls
// Serve with handler to handle requests on incoming packets.
// i.e. ListenAndServeIf("eth0",handler)
func ListenAndServeIf(interfaceName string, handler Handler) error {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return err
	}
	l, err := net.ListenPacket("udp4", ":67")
	if err != nil {
		return err
	}
	defer l.Close()
	return ServeIf(iface.Index, l, handler)
}

// Server is a multi-interface manager for serving DHCP requests.
type Server struct {
	sync.Mutex
	conn     *ipv4.PacketConn
	chans    map[int]chan msg
	listener net.PacketConn
	closed   bool
}

// NewServer creates a Server.
func NewServer() (*Server, error) {
	result := new(Server)
	l, err := net.ListenPacket("udp4", ":67")
	if err != nil {
		return nil, err
	}
	result.listener = l
	result.conn = ipv4.NewPacketConn(l)
	if err := result.conn.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return nil, err
	}
	result.chans = make(map[int]chan msg)
	go result.process()

	return result, nil
}

func (d *Server) Shutdown() {
	d.Lock()
	defer d.Unlock()
	d.closed = true
	for _, ch := range d.chans {
		ch <- msg{
			err: fmt.Errorf("Server shutdown"),
		}
	}
	d.listener.Close()
}

func (d *Server) process() {
	for {
		buffer := make([]byte, 1500)
		n, cm, addr, err := d.conn.ReadFrom(buffer)
		if cm != nil {
			func() {
				d.Lock()
				defer d.Unlock()
				if d.closed {
					return
				}
				if err != nil {
					for _, ch := range d.chans {
						ch <- msg{
							err: err,
						}
					}
					return
				}
				if ch, ok := d.chans[cm.IfIndex]; ok {
					ch <- msg{
						n:    n,
						addr: addr,
						err:  err,
						buf:  buffer,
					}
				}
			}()
		}
	}
}

// AddIf adds a new interface/handler pair to the Server.
func (d *Server) AddIf(ifIndex int, handler Handler) error {
	d.Lock()
	defer d.Unlock()
	if d.closed {
		return fmt.Errorf("Server shutdown.")
	}
	ch := make(chan msg, 10)
	d.chans[ifIndex] = ch
	go Serve(&serveIfConnMulti{ifIndex: ifIndex, conn: d.conn, ch: ch}, handler)
	return nil
}

// DelIf removes an interface from the Server.
func (d *Server) DelIf(ifIndex int) error {
	d.Lock()
	defer d.Unlock()
	if d.closed {
		return fmt.Errorf("Server shutdown.")
	}
	if ch, ok := d.chans[ifIndex]; ok {
		ch <- msg{
			err: fmt.Errorf("Client close requested"),
		}
		delete(d.chans, ifIndex)
	}
	return nil
}
