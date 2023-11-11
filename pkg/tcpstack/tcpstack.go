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

const WINDOW_SIZE = 1 << 16

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
	// chansMtx         sync.Mutex
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

func (sock *NormalSocket) SenderThread(n *node.Node) {
	sock.writeBuffer.writeMtx.Lock()
	for {
		// wait for LBW/NXT fields to be updated by VWrite
		sock.writeBuffer.writeCond.Wait()
		if sock.writeBuffer.LBW >= sock.writeBuffer.NXT {
			// stuff to send
			amount_to_send := (sock.writeBuffer.LBW - sock.writeBuffer.NXT + 1 + WINDOW_SIZE) % WINDOW_SIZE
			amount_to_send = min(amount_to_send, uint32(sock.ClientWindowSize))

			payload := sock.writeBuffer.buffer[sock.writeBuffer.NXT : sock.writeBuffer.NXT+amount_to_send]
			// sock.writeBuffer.writeMtx.Lock()
			sock.readBuffer.readMtx.Lock()
			packet := TCPPacket{
				sourceIp:   sock.LocalAddr,
				destIp:     sock.RemoteAddr,
				payload:    payload,
				flags:      header.TCPFlagAck,
				sourcePort: sock.LocalPort,
				destPort:   sock.RemotePort,
				seqNum:     sock.writeBuffer.UNA + sock.baseSeq,
				ackNum:     sock.readBuffer.NXT + sock.baseAck,
				window:     uint16((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE),
			}
			// sock.writeBuffer.writeMtx.Unlock()
			sock.readBuffer.readMtx.Unlock()

			// TODO: handle rest of this function with slidingWindow routine
			fmt.Printf("Sent %d bytes: %s\n", amount_to_send, string(payload))
			sock.slidingWindow(packet, n) // MAKE GO ROUTINE
		}
	}
	// sock.writeBuffer.writeMtx.Unlock()
	// TODO: need to unlock eventually?
}

func (sock *NormalSocket) slidingWindow(packet TCPPacket, n *node.Node) {
	// for testing purposes, split total payload into segments of size 1
	// should eventually be MSS (max ip packet size - ip header - tcp header)
	bytesSent := 0
	// sock.writeBuffer.writeMtx.Lock()
	tail := &WindowNode{
		// index:  uint16(s.writeBuffer.UNA),
		index:  0,
		next:   nil,
		acked:  false,
		packet: packet,
	}
	head := &WindowNode{
		index:  0,
		next:   tail,
		acked:  true,
		packet: TCPPacket{},
	}
	window := Window{
		head: head,
		tail: tail,
	}
	// sock.writeBuffer.writeMtx.Unlock()
	// TESTING
	MSS := uint16(1)

	for bytesSent < len(packet.payload) {
		segment := 0
		numBytesChan := make(chan int, 5) // chan buffer length should be smth like WINDOW_SIZE / MSS == (max num of segments we will ever send)
		for window.tail.index-window.head.index < min(uint16(len(packet.payload)), WINDOW_SIZE-1) {
			// make deep copy of cur tail's packet, with diff payload, window
			curPayload := packet.payload[window.tail.index : window.tail.index+MSS] // TODO: account for last segment size <= MSS and wrap around
			curPacket := TCPPacket{
				sourceIp:   window.tail.packet.sourceIp,
				destIp:     window.tail.packet.destIp,
				payload:    curPayload,
				flags:      window.tail.packet.flags,
				sourcePort: window.tail.packet.sourcePort,
				destPort:   window.tail.packet.destPort,
				seqNum:     sock.writeBuffer.UNA + sock.baseSeq,
				ackNum:     sock.readBuffer.NXT + sock.baseAck,
				window:     uint16((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE),
			}
			curPacket.payload = curPayload
			nextNode := &WindowNode{
				index:  window.tail.index + MSS,
				next:   nil,
				acked:  false,
				packet: curPacket,
			}
			tail.next = nextNode
			go func(tcpPacket *TCPPacket, wn *WindowNode, numBytesChan chan int) { // MAKE GO ROUTINE
				// send, wait for ack, retransmit
				// sock.writeBuffer.writeMtx.Lock()
				bytesToSend := MSS // TODO: last segment will be smaller than MSS
				sock.writeBuffer.NXT += uint32(bytesToSend)
				// sock.writeBuffer.writeMtx.Unlock()
				packet := tcpPacket.marshallTCPPacket()
				fmt.Printf("segment %d: %d bytes: %s\n", segment, MSS, string(curPayload))

				i := 0
				timeout := make(chan bool)
				expectedAck := sock.writeBuffer.NXT + sock.baseSeq
				// sock.chansMtx.Lock()
				sock.chans[expectedAck] = make(chan bool)
				// sock.chansMtx.Unlock()
			out:
				for {
					n.HandleSend(sock.RemoteAddr, packet, 6)
					go func(tmt chan bool) {
						time.Sleep(1000 * time.Second) // TESTING make a lot shorter (see specs)
						tmt <- true
					}(timeout)
					select {
					case <-sock.chans[expectedAck]: // wait for ack from receiver thread
						break out
					case <-timeout:
						i++
						fmt.Printf("try num: %d\n", i)
						if i == 3 {
							delete(sock.chans, expectedAck)
							numBytesChan <- 0
							return
						}
						continue
					}
				}

				wn.acked = true
				// sock.chansMtx.Lock()
				delete(sock.chans, expectedAck)
				// sock.chansMtx.Unlock()

				numBytesChan <- int(bytesToSend)
			}(&curPacket, tail, numBytesChan)

			segment++
			tail = tail.next
			window.tail = tail
		}
		for j := 0; j < 5; j++ {
			n := <-numBytesChan
			if n == 0 { // one of the packets was not successfully acked
				return
			}
			bytesSent += n
		}
		if bytesSent == len(packet.payload) {
			return
		}
		for window.head != window.tail && window.head.acked {
			window.head = window.head.next
		}
	}
}

func (sock *NormalSocket) ReceiverThread(n *node.Node) {
	var received node.TCPInfo
	for {
		received = <-sock.normalChan
		if received.Flag == header.TCPFlagAck {
			sock.ClientWindowSize = received.WindowSize
			// ignore packets whose contents should already be acked by a
			// previous packet with higher ack
			// TODO: account for wrap around case where we don't want to ignore
			// this
			// if received.AckNum <= sock.writeBuffer.UNA ||
			// 	received.AckNum > sock.writeBuffer.NXT {
			// 	continue
			// }
			sock.writeBuffer.UNA = (received.AckNum - sock.baseSeq + WINDOW_SIZE) % WINDOW_SIZE

			// here we received an ack packet in response to what we sent
			// sock.chansMtx.Lock()
			c, ok := sock.chans[received.AckNum]
			if ok {
				c <- true
				// sock.chansMtx.Unlock()
				continue
			}
			// sock.chansMtx.Unlock()

			// update read buffer pointers
			// write the payload to the read buffer
			// TODO: wrap around case
			copy(sock.readBuffer.buffer[sock.readBuffer.NXT:sock.readBuffer.NXT+uint32(len(received.Payload))], received.Payload)

			// cond variable for blocking in VRead
			sock.readBuffer.readMtx.Lock()
			sock.readBuffer.NXT = (received.SeqNum + uint32(len(received.Payload)) - sock.baseAck + WINDOW_SIZE) % WINDOW_SIZE
			sock.readBuffer.readCond.Broadcast()
			sock.readBuffer.readMtx.Unlock()

			new_window := (sock.readBuffer.LBR - sock.readBuffer.NXT + 1 + WINDOW_SIZE) % WINDOW_SIZE
			// need to send packet
			sk := received.SocketTableKey
			new_ack_num := sock.readBuffer.NXT + sock.baseAck
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

func (sock *NormalSocket) VWrite(message []byte) error {
	// first get how much left to write: this is LBW to UNA (so writing from LBW
	// + 1 to UNA - 1)
	sock.writeBuffer.writeMtx.Lock()
	space := (sock.writeBuffer.UNA - 1 - sock.writeBuffer.LBW + WINDOW_SIZE) % WINDOW_SIZE
	if space == 0 {
		space = WINDOW_SIZE
	}
	to_write := min(uint32(len(message)), space)
	to_write = min(to_write, uint32(node.MaxMessageSize-40)) // max is maxmsg - ip header - tcp header

	first_seg := min(WINDOW_SIZE-sock.writeBuffer.LBW-1, to_write)
	second_seg := to_write - first_seg

	copy(sock.writeBuffer.buffer[sock.writeBuffer.LBW+1:sock.writeBuffer.LBW+1+first_seg], message[:first_seg])
	copy(sock.writeBuffer.buffer[:second_seg], message[first_seg:first_seg+second_seg])

	// update pointer
	sock.writeBuffer.LBW = (sock.writeBuffer.LBW + to_write) % WINDOW_SIZE
	// fmt.Printf("Read %d bytes: %s\n", to_write, string(toRead))
	sock.writeBuffer.writeCond.Signal()
	sock.writeBuffer.writeMtx.Unlock()
	return nil
}

func (sock *NormalSocket) VRead(numbytes uint16) error {
	num_buf := (sock.readBuffer.NXT - 1 - sock.readBuffer.LBR + WINDOW_SIZE) % WINDOW_SIZE

	// block if num_buf == 0
	sock.readBuffer.readMtx.Lock()
	for num_buf == 0 {
		sock.readBuffer.readCond.Wait()
		num_buf = (sock.readBuffer.NXT - 1 - sock.readBuffer.LBR + WINDOW_SIZE) % WINDOW_SIZE
		if num_buf > 0 {
			break
		}
	}

	num_read := min(num_buf, uint32(numbytes))

	// read up to this
	first_seg := min(sock.readBuffer.LBR+uint32(num_read), WINDOW_SIZE-1)
	second_seg := num_read - (first_seg - sock.readBuffer.LBR) // in case it wraps around

	// append the two parts (hopefully slice[0:0] is just empty)
	//fmt.Printf("lbr: %d, nxt: %d, first_seg: %d, buf: %s\n", sock.readBuffer.LBR, sock.readBuffer.NXT, first_seg, string(sock.readBuffer.buffer[:20]))
	toRead := append(sock.readBuffer.buffer[sock.readBuffer.LBR+1:first_seg+1], sock.readBuffer.buffer[0:second_seg]...)

	fmt.Printf("Read %d bytes: %s\n", num_read, string(toRead))

	// adjust LBR
	sock.readBuffer.LBR = (sock.readBuffer.LBR + uint32(num_read)) % WINDOW_SIZE
	sock.readBuffer.readMtx.Unlock()

	return nil
}

func (sock *NormalSocket) VClose() error {
	panic("todo")
}

func (sock *ListenSocket) VClose() error {
	panic("todo")
}

func (sock *NormalSocket) printSocket() {
	fmt.Printf("  %d   %8s %5d   %8s %5d %12s\n", sock.SID,
		sock.LocalAddr, sock.LocalPort, sock.RemoteAddr, sock.RemotePort, stateMap[sock.state])
}

func (sock *ListenSocket) printSocket() {
	fmt.Printf("  %d    0.0.0.0 %5d    0.0.0.0     0       LISTEN\n", sock.SID, sock.localPort)
}

func (t *TCPStack) PrintTable() {
	fmt.Println("SID      LAddr LPort      RAddr RPort       Status")
	for _, sock := range t.SocketTable {
		sock.printSocket()
	}
}
