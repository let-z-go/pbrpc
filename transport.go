package pbrpc

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/let-z-go/toolkit/byte_stream"
	"golang.org/x/net/websocket"
)

type TransportPolicy struct {
	InitialReadBufferSize int32
	MaxPacketPayloadSize  int32

	validateOnce sync.Once
}

func (self *TransportPolicy) Validate() *TransportPolicy {
	self.validateOnce.Do(func() {
		if self.InitialReadBufferSize == 0 {
			self.InitialReadBufferSize = defaultInitialReadBufferSizeOfTransport
		} else {
			if self.InitialReadBufferSize < minInitialReadBufferSizeOfTransport {
				self.InitialReadBufferSize = minInitialReadBufferSizeOfTransport
			} else if self.InitialReadBufferSize > maxInitialReadBufferSizeOfTransport {
				self.InitialReadBufferSize = maxInitialReadBufferSizeOfTransport
			}
		}

		if self.MaxPacketPayloadSize < minMaxPacketPayloadSize {
			self.MaxPacketPayloadSize = minMaxPacketPayloadSize
		}
	})

	return self
}

var TransportClosedError = errors.New("pbrpc: transport closed")
var PacketPayloadTooLargeError = errors.New("pbrpc: packet payload too large")

const defaultInitialReadBufferSizeOfTransport = 1 << 12
const minInitialReadBufferSizeOfTransport = 1 << 8
const maxInitialReadBufferSizeOfTransport = 1 << 16
const minMaxPacketPayloadSize = 1 << 16
const packetHeaderSize = 4

type transport struct {
	policy           *TransportPolicy
	connection       net.Conn
	inputByteStream  byte_stream.ByteStream
	outputByteStream byte_stream.ByteStream
	openness         int
}

func (self *transport) initialize(policy *TransportPolicy, connection net.Conn) *transport {
	if self.openness != 0 {
		panic(errors.New("pbrpc: transport already initialized"))
	}

	self.policy = policy.Validate()
	self.connection = connection
	self.inputByteStream.ReserveBuffer(int(policy.InitialReadBufferSize))
	self.openness = 1
	return self
}

func (self *transport) close(force bool) error {
	if self.isClosed() {
		return TransportClosedError
	}

	if force {
		if connection, ok := self.connection.(*net.TCPConn); ok {
			connection.SetLinger(0)
		}
	}

	e := self.connection.Close()
	self.policy = nil
	self.connection = nil
	self.inputByteStream.GC()
	self.outputByteStream.GC()
	self.openness = -1
	return e
}

func (self *transport) peek(context_ context.Context, timeout time.Duration) ([]byte, error) {
	packet, e := self.doPeek(context_, timeout)

	if e != nil {
		return nil, e
	}

	packetPayload := packet[packetHeaderSize:]
	return packetPayload, nil
}

func (self *transport) skip(packetPayload []byte) error {
	if self.isClosed() {
		return TransportClosedError
	}

	packetSize := packetHeaderSize + len(packetPayload)
	self.inputByteStream.Skip(packetSize)
	return nil
}

func (self *transport) peekInBatch(context_ context.Context, timeout time.Duration) ([][]byte, error) {
	packet, e := self.doPeek(context_, timeout)

	if e != nil {
		return nil, e
	}

	packetPayloads := [][]byte{packet[packetHeaderSize:]}
	dataOffset := len(packet)

	for {
		packet, ok := self.tryPeek(dataOffset)

		if !ok {
			break
		}

		packetPayloads = append(packetPayloads, packet[packetHeaderSize:])
		dataOffset += len(packet)
	}

	return packetPayloads, nil
}

func (self *transport) skipInBatch(packetPayloads [][]byte) error {
	if self.isClosed() {
		return TransportClosedError
	}

	totalPacketSize := 0

	for _, packetPayload := range packetPayloads {
		totalPacketSize += packetHeaderSize + len(packetPayload)
	}

	self.inputByteStream.Skip(totalPacketSize)
	return nil
}

func (self *transport) write(callback func(*byte_stream.ByteStream) error) error {
	if self.isClosed() {
		return TransportClosedError
	}

	i := self.outputByteStream.GetDataSize()

	self.outputByteStream.WriteDirectly(packetHeaderSize, func(buffer []byte) error {
		return nil
	})

	if e := callback(&self.outputByteStream); e != nil {
		self.outputByteStream.Unwrite(self.outputByteStream.GetDataSize() - i)
		return e
	}

	packet := self.outputByteStream.GetData()[i:]
	packetPayloadSize := len(packet) - packetHeaderSize

	if packetPayloadSize > int(self.policy.MaxPacketPayloadSize) {
		self.outputByteStream.Unwrite(len(packet))
		return PacketPayloadTooLargeError
	}

	binary.BigEndian.PutUint32(packet, uint32(packetPayloadSize))
	return nil
}

func (self *transport) flush(context_ context.Context, timeout time.Duration) error {
	if self.isClosed() {
		return TransportClosedError
	}

	deadline, e := makeDeadline(context_, timeout)

	if e != nil {
		return e
	}

	if e := self.connection.SetWriteDeadline(deadline); e != nil {
		return e
	}

	data := self.outputByteStream.GetData()
	var nn int

	switch connection := self.connection.(type) {
	case *websocket.Conn:
		i := 0
		nn = 0

		for {
			j := i + maxWebSocketFramePayloadSize
			var n int

			if j >= len(data) {
				n, e = connection.Write(data[i:])
				nn += n
				break
			}

			n, e = connection.Write(data[i:j])
			nn += n

			if e != nil {
				break
			}

			i = j
		}
	default:
		nn, e = self.connection.Write(data)
	}

	self.outputByteStream.Skip(nn)
	return e
}

func (self *transport) isClosed() bool {
	return self.openness != 1
}

func (self *transport) doPeek(context_ context.Context, timeout time.Duration) ([]byte, error) {
	if self.isClosed() {
		return nil, TransportClosedError
	}

	deadlineIsSet := false

	if self.inputByteStream.GetDataSize() < packetHeaderSize {
		deadline, e := makeDeadline(context_, timeout)

		if e != nil {
			return nil, e
		}

		if e := self.connection.SetReadDeadline(deadline); e != nil {
			return nil, e
		}

		deadlineIsSet = true

		for {
			dataSize, e := self.connection.Read(self.inputByteStream.GetBuffer())

			if dataSize == 0 {
				return nil, e
			}

			self.inputByteStream.CommitBuffer(dataSize)

			if self.inputByteStream.GetDataSize() >= packetHeaderSize {
				if self.inputByteStream.GetBufferSize() == 0 {
					self.inputByteStream.ReserveBuffer(1)
				}

				break
			}
		}
	}

	packetHeader := self.inputByteStream.GetData()[:packetHeaderSize]
	packetPayloadSize := int(binary.BigEndian.Uint32(packetHeader))

	if packetPayloadSize > int(self.policy.MaxPacketPayloadSize) {
		return nil, PacketPayloadTooLargeError
	}

	packetSize := packetHeaderSize + packetPayloadSize

	if bufferSize := packetSize - self.inputByteStream.GetDataSize(); bufferSize >= 1 {
		if !deadlineIsSet {
			deadline, e := makeDeadline(context_, timeout)

			if e != nil {
				return nil, e
			}

			if e := self.connection.SetReadDeadline(deadline); e != nil {
				return nil, e
			}
		}

		self.inputByteStream.ReserveBuffer(bufferSize)

		for {
			dataSize, e := self.connection.Read(self.inputByteStream.GetBuffer())

			if dataSize == 0 {
				return nil, e
			}

			self.inputByteStream.CommitBuffer(dataSize)

			if self.inputByteStream.GetDataSize() >= packetSize {
				if self.inputByteStream.GetBufferSize() == 0 {
					self.inputByteStream.ReserveBuffer(1)
				}

				break
			}
		}
	}

	packet := self.inputByteStream.GetData()[:packetSize]
	return packet, nil
}

func (self *transport) tryPeek(dataOffset int) ([]byte, bool) {
	if self.inputByteStream.GetDataSize()-dataOffset < packetHeaderSize {
		return nil, false
	}

	packetHeader := self.inputByteStream.GetData()[dataOffset : dataOffset+packetHeaderSize]
	packetPayloadSize := int(binary.BigEndian.Uint32(packetHeader))

	if packetPayloadSize > int(self.policy.MaxPacketPayloadSize) {
		return nil, false
	}

	packetSize := packetHeaderSize + packetPayloadSize

	if self.inputByteStream.GetDataSize()-dataOffset < packetSize {
		return nil, false
	}

	packet := self.inputByteStream.GetData()[dataOffset : dataOffset+packetSize]
	return packet, true
}
