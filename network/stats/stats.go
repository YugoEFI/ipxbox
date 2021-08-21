// Package stats implements a Network that wraps another Network but also
// counts statistics on the packets that are sent and received.
package stats

import (
	"fmt"
	"time"

	"github.com/fragglet/ipxbox/ipx"
	"github.com/fragglet/ipxbox/network"
)

var (
	_ = (network.Network)(&statsNetwork{})
	_ = (network.Node)(&node{})
)

type Statistics struct {
	rxPackets, txPackets uint64
	rxBytes, txBytes     uint64
	connectTime          time.Time
}

func (s *Statistics) String() string {
	result := fmt.Sprintf("connected for %s; ", time.Since(s.connectTime))
	result += fmt.Sprintf("received %d packets (%d bytes), ",
		s.rxPackets, s.rxBytes)
	result += fmt.Sprintf("sent %d packets (%d bytes)",
		s.txPackets, s.txBytes)
	return result
}

type statsNetwork struct {
	inner network.Network
}

func (n *statsNetwork) NewNode() network.Node {
	return &node{
		inner: n.inner.NewNode(),
		stats: Statistics{
			connectTime: time.Now(),
		},
	}
}

type node struct {
	inner network.Node
	stats Statistics
}

func (n *node) ReadPacket() (*ipx.Packet, error) {
	packet, err := n.inner.ReadPacket()
	if err != nil {
		return nil, err
	}
	// This might be slightly counterintuitive: when a client *reads*
	// a packet, it's because we want to transmit to them, while when
	// we *write* a packet it's because we've received from them.
	n.stats.txPackets++
	n.stats.txBytes += uint64(len(packet.Payload) + ipx.HeaderLength)
	return packet, nil
}

func (n *node) WritePacket(packet *ipx.Packet) error {
	if err := n.inner.WritePacket(packet); err != nil {
		return err
	}
	n.stats.rxPackets++
	n.stats.rxBytes += uint64(len(packet.Payload) + ipx.HeaderLength)
	return nil
}

func (n *node) Close() error {
	return n.inner.Close()
}

func (n *node) GetProperty(x interface{}) bool {
	switch x.(type) {
	case *Statistics:
		*x.(*Statistics) = n.stats
		return true
	default:
		return n.inner.GetProperty(x)
	}
}

// New creates a network that wraps the given network but gathers statistics
// about packets that are sent and received.
func New(n network.Network) network.Network {
	return &statsNetwork{inner: n}
}

// Summary returns a string describing statistics for the given Node, if
// any can be fetched. Otherwise an empty string is returned.
func Summary(node network.Node) string {
	var s Statistics
	if !node.GetProperty(&s) {
		return ""
	}
	return s.String()
}