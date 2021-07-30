// Package lcp contains a gopacket Layer that implements the PPP Link Control
// Protocol (LCP).
package lcp

import (
	"encoding"
	"encoding/binary"
	"errors"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

var (
	MessageTooShort = errors.New("LCP message too short")
)

var LayerTypeLCP = gopacket.RegisterLayerType(1818, gopacket.LayerTypeMetadata{
	Name:    "LCP",
	Decoder: gopacket.DecodeFunc(decodeLCP),
})

// TODO: Implement SerializeTo and make this SerializableLayer.
var _ = gopacket.Layer(&LCP{})

type OptionType uint8

// TODO: constants for common option types

type Option struct {
	Type OptionType
	Data []byte
}

type MessageType uint8

const (
	ConfigureRequest MessageType = iota + 1
	ConfigureAck
	ConfigureNak
	ConfigureReject
	TerminateRequest
	TerminateAck
	CodeReject
	ProtocolReject
	EchoRequest
	EchoReply
	DiscardRequest
)

// PerTypeData specifies a common interface that is implemented by other types
// that represent per-message-type data.
type PerTypeData interface {
	encoding.BinaryUnmarshaler
}

// ConfigureData contains the data that is specific to Configure-* messages.
type ConfigureData struct {
	Options []Option
}

func (d *ConfigureData) UnmarshalBinary(data []byte) error {
	result := []Option{}
	for len(data) > 0 {
		if len(data) < 3 {
			return MessageTooShort
		}
		optType := OptionType(data[0])
		optLen := binary.BigEndian.Uint16(data[1:3])
		if int(optLen) > len(data) {
			return MessageTooShort
		}
		result = append(result, Option{
			Type: optType,
			Data: data[3:optLen],
		})
		data = data[optLen:]
	}
	d.Options = result
	return nil
}

// TerminateData contains the data that is specific to Terminate-* messages.
type TerminateData struct {
	Data []byte
}

func (d *TerminateData) UnmarshalBinary(data []byte) error {
	d.Data = data
	return nil
}

// EchoData contains the data that is specific to echo-* messages.
type EchoData struct {
	MagicNumber uint32
	Data        []byte
}

func (d *EchoData) UnmarshalBinary(data []byte) error {
	if len(data) < 4 {
		return MessageTooShort
	}
	d.MagicNumber = binary.BigEndian.Uint32(data[:4])
	d.Data = data[4:]
	return nil
}

// LCP is a gopacket layer for the Link Control Protocol.
type LCP struct {
	layers.BaseLayer
	Type       MessageType
	Identifier uint8
	Data       PerTypeData
}

func (l *LCP) LayerType() gopacket.LayerType {
	return LayerTypeLCP
}

func decodeLCP(data []byte, p gopacket.PacketBuilder) error {
	lcp := &LCP{}
	if len(data) < 4 {
		return MessageTooShort
	}
	lcp.Type = MessageType(data[0])
	lcp.Identifier = data[1]
	lenField := binary.BigEndian.Uint16(data[2:4])
	if int(lenField) > len(data) {
		return MessageTooShort
	}

	switch lcp.Type {
	case ConfigureRequest, ConfigureAck, ConfigureNak, ConfigureReject:
		lcp.Data = &ConfigureData{}
	case TerminateRequest, TerminateAck:
		lcp.Data = &TerminateData{}
	case EchoRequest, EchoReply:
		lcp.Data = &EchoData{}
		// TODO: Other message types.
	}
	if lcp.Data != nil {
		if err := lcp.Data.UnmarshalBinary(data[4:]); err != nil {
			return err
		}
	}
	lcp.Contents = data
	lcp.Payload = nil
	p.AddLayer(lcp)
	return nil
}
