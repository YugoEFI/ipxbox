//
// A standalone DOSbox IPX-over-UDP server.
//

package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"
)

type IPXAddr [6]byte

type Client struct {
	addr            *net.UDPAddr
	ipxAddr         IPXAddr
	lastReceiveTime time.Time
	lastSendTime    time.Time
}

type IPXHeaderAddr struct {
	network [4]byte
	addr    IPXAddr
	socket  uint16
}

type IPXHeader struct {
	checksum     uint16
	length       uint16
	transControl byte
	packetType   byte
	dest, src    IPXHeaderAddr
}

type IPXServer struct {
	socket           *net.UDPConn
	clients          map[string]*Client
	clientsByIPX     map[IPXAddr]*Client
	timeoutCheckTime time.Time
}

// Clients time out after 10 minutes of inactivity.
const CLIENT_TIMEOUT = 10 * 60 * time.Second

// We always send at least one packet every few seconds to keep the UDP
// connection open. Some NAT networks and firewalls can be very aggressive
// about closing off the ability for clients to receive packets on particular
// ports if nothing is received for a while.
const CLIENT_KEEPALIVE = 5 * time.Second

var ADDR_NULL = [6]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
var ADDR_BROADCAST = [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
var ADDR_PINGREPLY = [6]byte{0xff, 0xff, 0xff, 0xff, 0x00, 0x00}

func (addr *IPXHeaderAddr) Decode(data []byte) {
	copy(addr.network[0:], data[0:4])
	copy(addr.addr[0:], data[4:10])
	addr.socket = uint16((data[10] << 8) | data[11])
}

func (header *IPXHeader) Decode(packet []byte) bool {
	if len(packet) < 30 {
		return false
	}

	header.checksum = uint16((packet[0] << 8) | packet[1])
	header.length = uint16((packet[2] << 8) | packet[3])
	header.transControl = packet[4]
	header.packetType = packet[5]

	header.dest.Decode(packet[6:18])
	header.src.Decode(packet[18:30])

	return true
}

func (addr *IPXHeaderAddr) Encode(data []byte) {
	copy(data[0:4], addr.network[0:4])
	copy(data[4:10], addr.addr[0:])
	data[10] = byte(addr.socket >> 8)
	data[11] = byte(addr.socket & 0xff)
}

func (header *IPXHeader) Encode() []byte {
	result := make([]byte, 30)
	result[0] = byte(header.checksum >> 8)
	result[1] = byte(header.checksum & 0xff)
	result[2] = byte(header.length >> 8)
	result[3] = byte(header.length & 0xff)
	result[4] = header.transControl
	result[5] = header.packetType

	header.dest.Encode(result[6:18])
	header.src.Encode(result[18:30])

	return result
}

func (header IPXHeader) IsRegistrationPacket() bool {
	return header.dest.socket == 2 &&
		bytes.Compare(header.dest.addr[:], ADDR_NULL[:]) == 0
}

func (header IPXHeader) IsBroadcast() bool {
	return bytes.Compare(header.dest.addr[:], ADDR_BROADCAST[:]) == 0
}

// Allocate a new random address that does not share an address with
// an existing client.
func (server *IPXServer) NewAddress() IPXAddr {
	var result IPXAddr

	// Repeatedly generate a new IPX address until we generate
	// one that is not already in use.
	for {
		for i := 0; i < len(result); i++ {
			result[i] = byte(rand.Intn(255))
		}

		if _, ok := server.clientsByIPX[result]; !ok {
			break
		}
	}

	return result
}

// Process a registration packet, adding a new client if necessary.
func (server *IPXServer) NewClient(header *IPXHeader, addr *net.UDPAddr) {
	addrStr := addr.String()
	client, ok := server.clients[addrStr]

	if !ok {
		now := time.Now()
		//fmt.Printf("%s: %s: New client\n", now, addr)

		client = new(Client)
		client.addr = addr
		client.ipxAddr = server.NewAddress()
		client.lastReceiveTime = now

		server.clients[addrStr] = client
		server.clientsByIPX[client.ipxAddr] = client
	}

	// Send a reply back to the client
	reply := new(IPXHeader)
	reply.checksum = 0xffff
	reply.length = 30
	reply.transControl = 0

	copy(reply.dest.network[0:], []byte{0, 0, 0, 0})
	copy(reply.dest.addr[0:], client.ipxAddr[0:])
	reply.dest.socket = 2

	copy(reply.src.network[0:], []byte{0, 0, 0, 1})
	copy(reply.src.addr[0:], ADDR_BROADCAST[0:])
	reply.src.socket = 2

	client.lastSendTime = time.Now()
	server.socket.WriteToUDP(reply.Encode(), client.addr)
}

// Having received a packet, forward it on to another client.
func (server *IPXServer) ForwardPacket(header *IPXHeader, packet []byte) {
	if client, ok := server.clientsByIPX[header.dest.addr]; ok {
		client.lastSendTime = time.Now()
		server.socket.WriteToUDP(packet, client.addr)
	}
}

// Having received a broadcast packet, forward it to all clients.
func (server *IPXServer) ForwardBroadcastPacket(header *IPXHeader,
	packet []byte) {

	for _, client := range server.clients {
		if client.ipxAddr != header.src.addr {
			client.lastSendTime = time.Now()
			server.socket.WriteToUDP(packet, client.addr)
		}
	}
}

// Process a received UDP packet.
func (server *IPXServer) ProcessPacket(packet []byte, addr *net.UDPAddr) {
	var header IPXHeader
	if !header.Decode(packet) {
		return
	}

	if header.IsRegistrationPacket() {
		server.NewClient(&header, addr)
		return
	}

	srcClient, ok := server.clients[addr.String()]
	if !ok {
		return
	}

	// Clients can only send from their own address.
	if bytes.Compare(header.src.addr[0:], srcClient.ipxAddr[0:]) != 0 {
		return
	}

	srcClient.lastReceiveTime = time.Now()

	if header.IsBroadcast() {
		server.ForwardBroadcastPacket(&header, packet)
	} else {
		server.ForwardPacket(&header, packet)
	}
}

func (server *IPXServer) Listen(addr string) bool {
	udp4Addr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve address: ",
			err.Error())
		return false
	}

	socket, err := net.ListenUDP("udp", udp4Addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open socket: ",
			err.Error())
		return false
	}

	server.socket = socket
	server.clients = map[string]*Client{}
	server.clientsByIPX = map[IPXAddr]*Client{}
	server.timeoutCheckTime = time.Now().Add(10e9)

	return true
}

// SendPing transmits a ping packet to the given client. The DOSbox IPX client
// code recognizes broadcast packets sent to socket=2 and will send a reply to
// the source address that we provide.
func (server *IPXServer) SendPing(client *Client) {
	header := &IPXHeader{
		dest: IPXHeaderAddr{
			addr: ADDR_BROADCAST,
			socket: 2,
		},
		// We "send" the pings from an imaginary "ping reply" address
		// because if we used ADDR_NULL the reply would be
		// indistinguishable from a registration packet.
		src: IPXHeaderAddr{
			addr: ADDR_PINGREPLY,
			socket: 0,
		},
	}

	client.lastSendTime = time.Now()
	server.socket.WriteToUDP(header.Encode(), client.addr)
}

func (server *IPXServer) CheckClientTimeouts() {
	now := time.Now()
	for _, client := range server.clients {
		// Nothing sent in a while? Send a keepalive.
		// This is important because some types of game use a
		// client/server type arrangement where the server does not
		// broadcast anything but listens for broadcasts from clients.
		// An example is Warcraft 2. If there is no activity between
		// the client and server in a long time, some NAT gateways or
		// firewalls can drop the association.
		if now.After(client.lastSendTime.Add(CLIENT_KEEPALIVE)) {
			// We send a keepalive in the form of a ping packet
			// that the client should respond to, thus keeping us
			// from timing out the client from our own table if it
			// really is still there.
			server.SendPing(client)
		}

		// Nothing received in a long time? Time out the connection.
		if now.After(client.lastReceiveTime.Add(CLIENT_TIMEOUT)) {
			//fmt.Printf("%s: %s: Client timed out\n",
			//	now, client.addr)
			delete(server.clients, client.addr.String())
			delete(server.clientsByIPX, client.ipxAddr)
		}
	}
}

func (server *IPXServer) Poll() bool {
	var buf [1500]byte

	server.socket.SetReadDeadline(server.timeoutCheckTime)

	packetLen, addr, err := server.socket.ReadFromUDP(buf[0:])

	if err == nil {
		server.ProcessPacket(buf[0:packetLen], addr)
	} else if nerr, ok := err.(net.Error); ok && !nerr.Timeout() {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		return false
	}

	if time.Now().After(server.timeoutCheckTime) {
		server.CheckClientTimeouts()
		server.timeoutCheckTime = server.timeoutCheckTime.Add(10e9)
	}

	return true
}

func main() {
	var server IPXServer
	if !server.Listen(":10000") {
		os.Exit(1)
	}

	for {
		if !server.Poll() {
			os.Exit(1)
		}
	}
}
