// Package server implements the server side of the DOSBox IPX protocol.
package server

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/fragglet/ipxbox/ipx"
	"github.com/fragglet/ipxbox/network"
)

// Config contains configuration parameters for an IPX server.
type Config struct {
	// Clients time out if nothing is received for this amount of time.
	ClientTimeout time.Duration

	// Always send at least one packet every few seconds to keep the
	// UDP connection open. Some NAT networks and firewalls can be very
	// aggressive about closing off the ability for clients to receive
	// packets on particular ports if nothing is received for a while.
	// This controls the time for keepalives.
	KeepaliveTime time.Duration
}

// client represents a client that is connected to an IPX server.
type client struct {
	addr            *net.UDPAddr
	node            network.Node
	lastReceiveTime time.Time
	lastSendTime    time.Time
}

// Server is the top-level struct representing an IPX server that listens
// on a UDP port.
type Server struct {
	net              network.Network
	mu               sync.Mutex
	config           *Config
	socket           *net.UDPConn
	clients          map[string]*client
	timeoutCheckTime time.Time
}

var (
	// UnknownClientError is returned by Server.Write() if the destination
	// MAC address is not associated with any known client.
	UnknownClientError = errors.New("unknown destination address")

	DefaultConfig = &Config{
		ClientTimeout: 10 * time.Minute,
		KeepaliveTime: 5 * time.Second,
	}

	// Server-initiated pings come from this address.
	addrPingReply = [6]byte{0x02, 0xff, 0xff, 0xff, 0x00, 0x00}

	_ = (io.Closer)(&Server{})
)

// New creates a new Server, listening on the given address.
func New(addr string, n network.Network, c *Config) (*Server, error) {
	udp4Addr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	socket, err := net.ListenUDP("udp", udp4Addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		net:              n,
		config:           c,
		socket:           socket,
		clients:          map[string]*client{},
		timeoutCheckTime: time.Now().Add(10e9),
	}
	return s, nil
}

// runClient continually copies packets from the client's node and sends them
// to the connected UDP client. The function will only return when the client's
// network node is Close()d.
func (s *Server) runClient(c *client) {
	var buf [1500]byte
	for {
		packetLen, err := c.node.Read(buf[:])
		switch {
		case err == nil:
			s.socket.WriteToUDP(buf[0:packetLen], c.addr)
		case err == io.EOF:
			return
		default:
			// Other errors are ignored.
		}
	}
}

// newClient processes a registration packet, adding a new client if necessary.
func (s *Server) newClient(header *ipx.Header, addr *net.UDPAddr) {
	addrStr := addr.String()
	c, ok := s.clients[addrStr]

	if !ok {
		c = &client{
			addr:            addr,
			lastReceiveTime: time.Now(),
			node:            s.net.NewNode(),
		}

		s.clients[addrStr] = c
		go s.runClient(c)
	}

	// Send a reply back to the client
	reply := &ipx.Header{
		Checksum:     0xffff,
		Length:       30,
		TransControl: 0,
		Dest: ipx.HeaderAddr{
			Network: [4]byte{0, 0, 0, 0},
			Addr:    c.node.Address(),
			Socket:  2,
		},
		Src: ipx.HeaderAddr{
			Network: [4]byte{0, 0, 0, 1},
			Addr:    ipx.AddrBroadcast,
			Socket:  2,
		},
	}

	c.lastSendTime = time.Now()
	encodedReply, err := reply.MarshalBinary()
	if err == nil {
		s.socket.WriteToUDP(encodedReply, c.addr)
	}
}

// processPacket decodes and processes a received UDP packet, sending responses
// and forwarding the packet on to other clients as appropriate.
func (s *Server) processPacket(packet []byte, addr *net.UDPAddr) {
	var header ipx.Header
	if err := header.UnmarshalBinary(packet); err != nil {
		return
	}

	if header.IsRegistrationPacket() {
		s.newClient(&header, addr)
		return
	}

	// Find which client sent it; it must be a registered client sending
	// from their own IPX address.
	srcClient, ok := s.clients[addr.String()]
	if !ok {
		return
	}
	if header.Src.Addr != srcClient.node.Address() {
		return
	}
	// Deliver packet to the network.
	srcClient.lastReceiveTime = time.Now()
	srcClient.node.Write(packet)
}

// sendPing transmits a ping packet to the given client. The DOSbox IPX client
// code recognizes broadcast packets sent to socket=2 and will send a reply to
// the source address that we provide.
func (s *Server) sendPing(c *client) {
	header := &ipx.Header{
		Dest: ipx.HeaderAddr{
			Addr:   ipx.AddrBroadcast,
			Socket: 2,
		},
		// We "send" the pings from an imaginary "ping reply" address
		// because if we used ipx.AddrNull the reply would be
		// indistinguishable from a registration packet.
		Src: ipx.HeaderAddr{
			Addr:   addrPingReply,
			Socket: 0,
		},
	}

	c.lastSendTime = time.Now()
	encodedHeader, err := header.MarshalBinary()
	if err == nil {
		s.socket.WriteToUDP(encodedHeader, c.addr)
	}
}

// checkClientTimeouts checks all clients that are connected to the server and
// handles idle clients to which we have no sent data or from which we have not
// received data recently. This function should be called regularly; it returns
// the time that it should next be invoked.
func (s *Server) checkClientTimeouts() time.Time {
	now := time.Now()

	// At absolute max we should check again in 10 seconds, as a new client
	// might connect in the mean time.
	nextCheckTime := now.Add(10 * time.Second)

	for _, c := range s.clients {
		// Nothing sent in a while? Send a keepalive.
		// This is important because some types of game use a
		// client/server type arrangement where the server does not
		// broadcast anything but listens for broadcasts from clients.
		// An example is Warcraft 2. If there is no activity between
		// the client and server in a long time, some NAT gateways or
		// firewalls can drop the association.
		keepaliveTime := c.lastSendTime.Add(s.config.KeepaliveTime)
		if now.After(keepaliveTime) {
			// We send a keepalive in the form of a ping packet
			// that the client should respond to, thus keeping us
			// from timing out the client from our own table if it
			// really is still there.
			s.sendPing(c)
			keepaliveTime = c.lastSendTime.Add(s.config.KeepaliveTime)
		}

		// Nothing received in a long time? Time out the connection.
		timeoutTime := c.lastReceiveTime.Add(s.config.ClientTimeout)
		if now.After(timeoutTime) {
			delete(s.clients, c.addr.String())
			c.node.Close()
		}

		if keepaliveTime.Before(nextCheckTime) {
			nextCheckTime = keepaliveTime
		}
		if timeoutTime.Before(nextCheckTime) {
			nextCheckTime = timeoutTime
		}
	}

	return nextCheckTime
}

// poll listens for new packets, blocking until one is received, or until
// a timeout is reached.
func (s *Server) poll() error {
	var buf [1500]byte

	s.socket.SetReadDeadline(s.timeoutCheckTime)
	packetLen, addr, err := s.socket.ReadFromUDP(buf[:])

	if err == nil {
		s.processPacket(buf[0:packetLen], addr)
	} else if nerr, ok := err.(net.Error); ok && !nerr.Timeout() {
		return err
	}

	// We must regularly call checkClientTimeouts(); when we do, update
	// server.timeoutCheckTime with the next time it should be invoked.
	if time.Now().After(s.timeoutCheckTime) {
		s.timeoutCheckTime = s.checkClientTimeouts()
	}

	return nil
}

// Run runs the server, blocking until the socket is closed or an error occurs.
func (s *Server) Run() {
	for {
		if err := s.poll(); err != nil {
			return
		}
	}
}

// Close closes the socket associated with the server to shut it down.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, client := range s.clients {
		client.node.Close()
	}
	return s.socket.Close()
}
