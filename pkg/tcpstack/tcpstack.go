package tcpstack

import (
	"errors"
	"fmt"
	"iptcp/pkg/iptcp_utils"
	"iptcp/pkg/node"
	"math/rand"
	"net/netip"
	"sync"
	"time"

	"github.com/google/netstack/tcpip/header"
)

const (
	LISTEN = iota
	SYN_SENT
	SYN_RECEIVED
	ESTABLISHED
	SYNACK = uint8(header.TCPFlagSyn | header.TCPFlagAck)
)

const WINDOW_SIZE = 65535

// TESTING
// const WINDOW_SIZE = 5

var stateMap = map[uint8]string{
	LISTEN:       "LISTEN",
	SYN_SENT:     "SYN_SENT",
	SYN_RECEIVED: "SYN_RECEIVED",
	ESTABLISHED:  "ESTABLISHED",
}

type TCPStack struct {
	SocketTable map[node.SocketTableKey]Socket
	ip          netip.Addr
	SID_to_sk   map[uint16]node.SocketTableKey
	SID         uint16
}

type ReadBuffer struct {
	buffer   []byte
	readMtx  *sync.Mutex // protect pointers
	readCond *sync.Cond
	LBR      uint32
	NXT      uint32
	// add some sort of queue for out of order
}

type WriteBuffer struct {
	buffer    []byte
	writeMtx  *sync.Mutex // protect pointers
	writeCond *sync.Cond
	UNA       uint32
	NXT       uint32
	LBW       uint32
}

type Socket interface {
	// VClose()
	printSocket()
}

type ListenSocket struct {
	localPort  uint16
	SID        uint16
	listenChan chan node.TCPInfo
}

type NormalSocket struct {
	SID              uint16
	readBuffer       *ReadBuffer
	writeBuffer      *WriteBuffer
	state            uint8
	normalChan       chan node.TCPInfo
	baseSeq          uint32
	baseAck          uint32
	ClientWindowSize uint16
	chans            map[uint32]chan bool // ACK num -> channel between receiver thread and sliding window thread
	// earlyQueue []TCPPacket
	node.SocketTableKey
}

type TCPPacket struct {
	sourceIp   netip.Addr
	destIp     netip.Addr
	payload    []byte
	flags      uint8
	sourcePort uint16
	destPort   uint16
	seqNum     uint32
	ackNum     uint32
	window     uint16
}

type Window struct {
	head *WindowNode
	tail *WindowNode
}

type WindowNode struct {
	index  uint16
	next   *WindowNode
	acked  bool
	packet TCPPacket
	// lastSent time.Time
}

func Initialize(n *node.Node) TCPStack {
	tcpStack := &TCPStack{ip: n.IpAddr, SocketTable: make(map[node.SocketTableKey]Socket), SID_to_sk: make(map[uint16]node.SocketTableKey), SID: 0}

	// handle matching process
	go func() {
		for {
			received := <-n.TCPChan
			key := flipSocketKeyFields(received.SocketTableKey)
			sock, ok := tcpStack.SocketTable[key]
			if ok && sock.(*NormalSocket).state != SYN_RECEIVED {
				sock.(*NormalSocket).normalChan <- received
			} else {
				listenTuple := node.SocketTableKey{LocalPort: received.RemotePort}
				if sock, ok := tcpStack.SocketTable[listenTuple]; ok {
					sock.(*ListenSocket).listenChan <- received
				}
			}
		}
	}()
	return *tcpStack
}

func flipSocketKeyFields(sk node.SocketTableKey) node.SocketTableKey {
	return node.SocketTableKey{
		LocalAddr:  sk.RemoteAddr,
		LocalPort:  sk.RemotePort,
		RemoteAddr: sk.LocalAddr,
		RemotePort: sk.LocalPort,
	}
}

func (t *TCPStack) VListen(port uint16) (*ListenSocket, error) {
	SocketTableKey := new(node.SocketTableKey)
	SocketTableKey.LocalPort = port
	if _, ok := t.SocketTable[*SocketTableKey]; ok {
		// already listening on port
		return nil, fmt.Errorf("already listening on port %d", port)
	}
	lsock := &ListenSocket{localPort: port, listenChan: make(chan node.TCPInfo), SID: t.SID}
	t.SID++
	t.SocketTable[*SocketTableKey] = lsock
	return lsock, nil
}

func (t *TCPStack) VConnect(destAddr netip.Addr, destPort uint16, n *node.Node) (*NormalSocket, error) {
	// generate new random port
	var randSrcPort uint16
	for {
		randSrcPort = uint16(rand.Intn(65535-20000) + 20000)
		SocketTableKey := &node.SocketTableKey{LocalPort: randSrcPort}
		if _, ok := t.SocketTable[*SocketTableKey]; !ok {
			break
		}
	}
	// TESTING:
	// randSrcPort = 20000

	sk := node.SocketTableKey{LocalAddr: t.ip, LocalPort: randSrcPort, RemoteAddr: destAddr, RemotePort: destPort}
	newSocket := &NormalSocket{
		SID:            t.SID,
		state:          SYN_SENT,
		SocketTableKey: sk,
		normalChan:     make(chan node.TCPInfo),
	}
	t.SID_to_sk[t.SID] = sk
	t.SID++
	t.SocketTable[sk] = newSocket

	// seqNum := rand.Uint32()
	// TESTING
	seqNum := uint32(5000)
	tcpPacket := TCPPacket{
		sourceIp:   t.ip,
		destIp:     destAddr,
		payload:    nil,
		flags:      header.TCPFlagSyn,
		sourcePort: randSrcPort,
		destPort:   destPort,
		seqNum:     seqNum,
		ackNum:     0,
		window:     65535,
	}
	packet := tcpPacket.marshallTCPPacket()
	i := 0
	var ci node.TCPInfo
	timeout := make(chan bool)
	for {
		n.HandleSend(destAddr, packet, 6)
		go func(tmt chan bool) {
			// TESTING: change back to 3
			time.Sleep(30 * time.Second)
			tmt <- true
		}(timeout)
		select {
		case ci = <-newSocket.normalChan: // wait until protocol6 confirms
		case <-timeout:
			i++
			fmt.Printf("try num: %d\n", i)
			if i == 3 {
				return &NormalSocket{}, errors.New("could not connect")
			}
			continue
		}
		if ci.Flag == SYNACK {
			break
		}
		i++
		if i == 3 {
			return &NormalSocket{}, errors.New("could not connect")
		}
	}
	readMtx := sync.Mutex{}
	new_read := &ReadBuffer{
		buffer:   make([]byte, WINDOW_SIZE),
		readMtx:  &readMtx,
		readCond: sync.NewCond(&readMtx),
		LBR:      0,
		NXT:      1,
	}
	writeMtx := sync.Mutex{}
	new_send := &WriteBuffer{
		buffer:    make([]byte, WINDOW_SIZE),
		writeMtx:  &writeMtx,
		writeCond: sync.NewCond(&writeMtx),
		UNA:       1,
		NXT:       1,
		LBW:       0,
	}
	newSocket.readBuffer = new_read
	newSocket.writeBuffer = new_send
	newSocket.baseAck = ci.SeqNum
	newSocket.baseSeq = ci.AckNum - 1
	newSocket.ClientWindowSize = 65535
	newSocket.chans = make(map[uint32]chan bool)

	t.SocketTable[sk].(*NormalSocket).state = ESTABLISHED
	tcpPacket = TCPPacket{
		sourceIp:   t.ip,
		destIp:     destAddr,
		payload:    nil,
		flags:      header.TCPFlagAck,
		sourcePort: randSrcPort,
		destPort:   destPort,
		seqNum:     ci.AckNum,
		ackNum:     ci.SeqNum + 1,
		window:     65535,
	}
	packet = tcpPacket.marshallTCPPacket()
	// establish connection
	n.HandleSend(destAddr, packet, 6)

	return newSocket, nil
}

func (lsock *ListenSocket) VAccept(t *TCPStack, n *node.Node) (*NormalSocket, error) {
	// wait for SYN
	var ci node.TCPInfo
	for !(ci.Flag == header.TCPFlagSyn && ci.SocketTableKey.RemotePort == lsock.localPort) {
		ci = <-lsock.listenChan
	}
	sk := ci.SocketTableKey

	readMtx := sync.Mutex{}
	new_read := &ReadBuffer{
		buffer:   make([]byte, WINDOW_SIZE),
		readMtx:  &readMtx,
		readCond: sync.NewCond(&readMtx),
		LBR:      0,
		NXT:      1,
	}
	writeMtx := sync.Mutex{}
	new_send := &WriteBuffer{
		buffer:    make([]byte, WINDOW_SIZE),
		writeMtx:  &writeMtx,
		writeCond: sync.NewCond(&writeMtx),
		UNA:       1,
		NXT:       1,
		LBW:       0,
	}
	// create new normal socket
	newSK := flipSocketKeyFields(sk)
	newSocket := &NormalSocket{
		SID:            t.SID,
		readBuffer:     new_read,
		writeBuffer:    new_send,
		state:          SYN_RECEIVED,
		SocketTableKey: newSK,
		normalChan:     make(chan node.TCPInfo),
		chans:          make(map[uint32]chan bool),
	}
	t.SID_to_sk[t.SID] = newSK
	t.SID++
	t.SocketTable[newSK] = newSocket

	newSocket.baseAck = ci.SeqNum
	// newSocket.baseSeq = rand.Uint32()
	newSocket.ClientWindowSize = 65535
	// TESTING
	newSocket.baseSeq = uint32(6000)

	// send SYN+ACK back to client
	tcpPacket := TCPPacket{
		sourceIp:   sk.RemoteAddr,
		destIp:     sk.LocalAddr,
		payload:    nil,
		flags:      SYNACK,
		sourcePort: sk.RemotePort,
		destPort:   sk.LocalPort,
		seqNum:     newSocket.baseSeq,
		ackNum:     ci.SeqNum + 1,
		window:     65535,
	}
	packet := tcpPacket.marshallTCPPacket()

	n.HandleSend(sk.LocalAddr, packet, 6)

	// wait for final packet to establish TCP
	for !(ci.Flag == header.TCPFlagAck && ci.SocketTableKey.RemotePort == lsock.localPort) {
		ci = <-lsock.listenChan
	}
	newSocket.state = ESTABLISHED

	return newSocket, nil
}

func (p TCPPacket) marshallTCPPacket() []byte {

	tcpHdr := header.TCPFields{
		SrcPort:       p.sourcePort,
		DstPort:       p.destPort,
		SeqNum:        p.seqNum,
		AckNum:        p.ackNum,
		DataOffset:    20,
		Flags:         p.flags,
		WindowSize:    p.window,
		Checksum:      0,
		UrgentPointer: 0,
	}

	checksum := iptcp_utils.ComputeTCPChecksum(&tcpHdr, p.sourceIp, p.destIp, p.payload)
	tcpHdr.Checksum = checksum

	// Serialize the TCP header
	tcpHeaderBytes := make(header.TCP, iptcp_utils.TcpHeaderLen)
	tcpHeaderBytes.Encode(&tcpHdr)

	// Combine the TCP header + payload into one byte array, which
	// becomes the payload of the IP packet
	ipPacketPayload := make([]byte, 0, len(tcpHeaderBytes)+len(p.payload))
	ipPacketPayload = append(ipPacketPayload, tcpHeaderBytes...)
	ipPacketPayload = append(ipPacketPayload, []byte(p.payload)...)

	return ipPacketPayload
}

func (socket *NormalSocket) SenderThread(n *node.Node) {
	socket.writeBuffer.writeMtx.Lock()
	for {
		// wait for LBW/NXT fields to be updated by VWrite
		socket.writeBuffer.writeCond.Wait()
		if socket.writeBuffer.LBW >= socket.writeBuffer.NXT {
			// stuff to send
			// put this in separate goroutine for retransmission purposes

			amount_to_send := (socket.writeBuffer.LBW - socket.writeBuffer.NXT + 1 + WINDOW_SIZE) % WINDOW_SIZE
			amount_to_send = min(amount_to_send, uint32(socket.ClientWindowSize))

			// split payload into payloads of 1 byte for testing

			payload := socket.writeBuffer.buffer[socket.writeBuffer.NXT : socket.writeBuffer.NXT+amount_to_send]
			// socket.writeBuffer.writeMtx.Lock()
			socket.readBuffer.readMtx.Lock()
			packet := TCPPacket{
				sourceIp:   socket.LocalAddr,
				destIp:     socket.RemoteAddr,
				payload:    payload,
				flags:      header.TCPFlagAck,
				sourcePort: socket.LocalPort,
				destPort:   socket.RemotePort,
				seqNum:     socket.writeBuffer.UNA + socket.baseSeq,
				ackNum:     socket.readBuffer.NXT + socket.baseAck,
			}
			// socket.writeBuffer.writeMtx.Unlock()
			socket.readBuffer.readMtx.Unlock()

			// TODO: handle rest of this function with slidingWindow routine
			fmt.Printf("Sent %d bytes: %s\n", amount_to_send, string(payload))
			socket.slidingWindow(packet, n)
			/*
				socket.writeBuffer.NXT = (socket.writeBuffer.NXT + amount_to_send + WINDOW_SIZE) % WINDOW_SIZE

				sk := flipSocketKeyFields(socket.SocketTableKey)

				seq_num := socket.writeBuffer.UNA + socket.baseSeq
				ack_num := socket.readBuffer.NXT + socket.baseAck
				new_window := (socket.readBuffer.LBR - socket.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE

				tcpPacket := &TCPPacket{
					sourceIp:   sk.RemoteAddr,
					destIp:     sk.LocalAddr,
					payload:    payload,
					flags:      header.TCPFlagAck,
					sourcePort: sk.RemotePort,
					destPort:   sk.LocalPort,
					seqNum:     seq_num,
					ackNum:     ack_num,
					window:     uint16(new_window),
				}
				packet := tcpPacket.marshallTCPPacket()
				n.HandleSend(sk.LocalAddr, packet, 6)
			*/
		}
	}
	// socket.writeBuffer.writeMtx.Unlock()
	// TODO: need to unlock eventually?
}

func (s *NormalSocket) slidingWindow(packet TCPPacket, n *node.Node) {
	// for testing purposes, split total payload into segments of size 1
	// should eventually be MSS (max ip packet size - ip header - tcp header)
	bytesSent := 0
	// s.writeBuffer.writeMtx.Lock()
	tail := &WindowNode{
		index:  uint16(s.writeBuffer.UNA),
		next:   nil,
		acked:  false,
		packet: packet,
	}
	head := &WindowNode{
		index:  uint16(s.writeBuffer.UNA),
		next:   tail,
		acked:  true,
		packet: TCPPacket{},
	}
	window := Window{
		head: head,
		tail: tail,
	}
	// s.writeBuffer.writeMtx.Unlock()
	// TESTING
	MSS := uint16(1)

	for bytesSent < len(packet.payload) {
		for window.tail.index-window.head.index+1 < WINDOW_SIZE {
			// make deep copy of cur tail's packet, with diff payload, window
			curPayload := packet.payload[window.tail.index : window.tail.index+MSS] // TODO: account for last segment size <= MSS and wrap around
			curPacket := TCPPacket{
				sourceIp:   window.tail.packet.sourceIp,
				destIp:     window.tail.packet.destIp,
				payload:    curPayload,
				flags:      window.tail.packet.flags,
				sourcePort: window.tail.packet.sourcePort,
				destPort:   window.tail.packet.destPort,
				seqNum:     window.tail.packet.seqNum,
				ackNum:     window.tail.packet.ackNum,
				window:     window.tail.packet.window - MSS,
			}
			curPacket.payload = curPayload
			nextNode := &WindowNode{
				index:  window.tail.index + MSS,
				next:   nil,
				acked:  false,
				packet: curPacket,
			}
			tail.next = nextNode
			func(tcpPacket *TCPPacket, bytesSent int, wn *WindowNode) {
				// send, wait for ack, retransmit
				// s.writeBuffer.writeMtx.Lock()
				s.writeBuffer.NXT = max(s.writeBuffer.NXT, uint32(s.writeBuffer.NXT+uint32(MSS)+WINDOW_SIZE)) % WINDOW_SIZE
				// s.writeBuffer.writeMtx.Unlock()
				packet := tcpPacket.marshallTCPPacket()
				fmt.Printf("Sent %d bytes: %s\n", MSS, string(curPayload))

				i := 0
				timeout := make(chan bool)
				// expectedAck := (s.writeBuffer.NXT + uint32(MSS)) %
				// WINDOW_SIZE // TODO: should just be NXT?
				expectedAck := s.writeBuffer.NXT
				s.chans[expectedAck] = make(chan bool)
			out:
				for {
					n.HandleSend(s.RemoteAddr, packet, 6)
					go func(tmt chan bool) {
						time.Sleep(1 * time.Second) // TESTING make a lot shorter (see specs)
						tmt <- true
					}(timeout)
					select {
					case <-s.chans[expectedAck]: // wait for ack from receiver thread
						break out
					case <-timeout:
						i++
						fmt.Printf("try num: %d\n", i)
						if i == 3 {
							close(s.chans[expectedAck])
							delete(s.chans, expectedAck)
							// TODO: need to return from everything
							return
						}
						continue
					}
				}

				// successfully sent packet! if threads only increment, don't
				// need to use mutex right...
				bytesSent += int(MSS)
				wn.acked = true
				close(s.chans[expectedAck])
				delete(s.chans, expectedAck)

			}(&curPacket, bytesSent, tail)
			tail = tail.next
			// if bytesSent
		}
		for window.head != window.tail && window.head.acked {
			window.head = window.head.next
		}
	}
}

func (socket *NormalSocket) ReceiverThread(n *node.Node) {
	var received node.TCPInfo
	for {
		received = <-socket.normalChan
		if received.Flag == header.TCPFlagAck {
			socket.ClientWindowSize = received.WindowSize
			// ignore packets whose contents should already be acked by a
			// previous packet with higher ack
			// TODO: account for wrap around case where we don't want to ignore
			// this
			// if received.AckNum <= socket.writeBuffer.UNA ||
			// 	received.AckNum > socket.writeBuffer.NXT {
			// 	continue
			// }
			socket.writeBuffer.UNA = (received.AckNum - socket.baseSeq + WINDOW_SIZE) % WINDOW_SIZE
			// ack packet to what we sent
			// TODO: account for wrap around
			if (socket.baseSeq + socket.writeBuffer.UNA) <= received.AckNum {
				if c, ok := socket.chans[received.AckNum]; ok {
					c <- true
				}
				continue
			}
			// update read buffer pointers
			// write the payload to the read buffer
			// assume for now that bytes are guaranteed to be in order
			// TODO: wrap around case
			copy(socket.readBuffer.buffer[socket.readBuffer.NXT:socket.readBuffer.NXT+uint32(len(received.Payload))], received.Payload)

			// cond variable for blocking in VRead
			socket.readBuffer.readMtx.Lock()
			socket.readBuffer.NXT = (received.SeqNum + uint32(len(received.Payload)) - socket.baseAck + WINDOW_SIZE) % WINDOW_SIZE
			socket.readBuffer.readCond.Broadcast()
			socket.readBuffer.readMtx.Unlock()

			new_window := (socket.readBuffer.LBR - socket.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE
			// need to send packet
			sk := received.SocketTableKey
			new_ack_num := socket.readBuffer.NXT + socket.baseAck
			tcpPacket := TCPPacket{
				sourceIp:   sk.RemoteAddr,
				destIp:     sk.LocalAddr,
				payload:    nil,
				flags:      header.TCPFlagAck,
				sourcePort: sk.RemotePort,
				destPort:   sk.LocalPort,
				seqNum:     received.AckNum,
				ackNum:     new_ack_num,
				window:     uint16(new_window),
			}
			packet := tcpPacket.marshallTCPPacket()
			n.HandleSend(sk.LocalAddr, packet, 6)
		}
	}
}

func (socket *NormalSocket) VWrite(message []byte) error {
	// first get how much left to write: this is LBW to UNA (so writing from LBW
	// + 1 to UNA - 1)
	socket.writeBuffer.writeMtx.Lock()
	space := (socket.writeBuffer.UNA - 1 - socket.writeBuffer.LBW + WINDOW_SIZE) % WINDOW_SIZE
	if space == 0 {
		space = WINDOW_SIZE
	}
	to_write := min(uint32(len(message)), space)
	to_write = min(to_write, uint32(node.MaxMessageSize-40)) // max is maxmsg - ip header - tcp header

	first_seg := min(WINDOW_SIZE-socket.writeBuffer.LBW-1, to_write)
	second_seg := to_write - first_seg

	copy(socket.writeBuffer.buffer[socket.writeBuffer.LBW+1:socket.writeBuffer.LBW+1+first_seg], message[:first_seg])
	copy(socket.writeBuffer.buffer[:second_seg], message[first_seg:first_seg+second_seg])

	// update pointer
	socket.writeBuffer.LBW = (socket.writeBuffer.LBW + to_write) % WINDOW_SIZE
	// fmt.Printf("Read %d bytes: %s\n", to_write, string(toRead))
	socket.writeBuffer.writeCond.Signal()
	socket.writeBuffer.writeMtx.Unlock()
	return nil
}

func (socket *NormalSocket) VRead(numbytes uint16) error {
	num_buf := (socket.readBuffer.NXT - 1 - socket.readBuffer.LBR + WINDOW_SIZE) % WINDOW_SIZE

	// block if num_buf == 0
	socket.readBuffer.readMtx.Lock()
	for num_buf == 0 {
		socket.readBuffer.readCond.Wait()
		num_buf = (socket.readBuffer.NXT - 1 - socket.readBuffer.LBR + WINDOW_SIZE) % WINDOW_SIZE
		if num_buf > 0 {
			break
		}
	}

	num_read := min(num_buf, uint32(numbytes))

	// read up to this
	first_seg := min(socket.readBuffer.LBR+uint32(num_read), WINDOW_SIZE-1)
	second_seg := num_read - (first_seg - socket.readBuffer.LBR) // in case it wraps around

	// append the two parts (hopefully slice[0:0] is just empty)
	//fmt.Printf("lbr: %d, nxt: %d, first_seg: %d, buf: %s\n", socket.readBuffer.LBR, socket.readBuffer.NXT, first_seg, string(socket.readBuffer.buffer[:20]))
	toRead := append(socket.readBuffer.buffer[socket.readBuffer.LBR+1:first_seg+1], socket.readBuffer.buffer[0:second_seg]...)

	fmt.Printf("Read %d bytes: %s\n", num_read, string(toRead))

	// adjust LBR
	socket.readBuffer.LBR = (socket.readBuffer.LBR + uint32(num_read)) % WINDOW_SIZE
	socket.readBuffer.readMtx.Unlock()

	return nil
}

func (socket *NormalSocket) VClose() error {
	panic("todo")
}

func (socket *ListenSocket) VClose() error {
	panic("todo")
}

func (socket *NormalSocket) printSocket() {
	fmt.Printf("  %d   %8s %5d   %8s %5d %12s\n", socket.SID,
		socket.LocalAddr, socket.LocalPort, socket.RemoteAddr, socket.RemotePort, stateMap[socket.state])
}

func (socket *ListenSocket) printSocket() {
	fmt.Printf("  %d    0.0.0.0 %5d    0.0.0.0     0       LISTEN\n", socket.SID, socket.localPort)
}

func (t *TCPStack) PrintTable() {
	fmt.Println("SID      LAddr LPort      RAddr RPort       Status")
	for _, socket := range t.SocketTable {
		socket.printSocket()
	}
}
