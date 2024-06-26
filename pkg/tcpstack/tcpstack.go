package tcpstack

/*
TODO: i think this is all that's left...
- VClose stuff; i'll do this first
- RTO calculation for sliding window timeout
- send/receive file
- zero window probing
	- take care of wrap around edge case
*/

import (
	"bufio"
	"container/heap"
	"errors"
	"fmt"
	"io"
	"iptcp/pkg/iptcp_utils"
	"iptcp/pkg/node"
	"iptcp/pkg/pq"
	"math/rand"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/google/netstack/tcpip/header"
)

const (
	LISTEN = iota
	SYN_SENT
	SYN_RECEIVED
	ESTABLISHED
	FIN_WAIT_1
	FIN_WAIT_2
	CLOSE_WAIT
	LAST_ACK
	TIME_WAIT
	SYNACK = uint8(header.TCPFlagSyn | header.TCPFlagAck)
	FINACK = uint8(header.TCPFlagFin | header.TCPFlagAck)
)

// const WINDOW_SIZE = 1 << 16

// TESTING
const WINDOW_SIZE = 1 << 16

// const MSS = uint16(1360)

// TESTING
const MSS = uint16(1360)

var stateMap = map[uint8]string{
	LISTEN:       "LISTEN",
	SYN_SENT:     "SYN_SENT",
	SYN_RECEIVED: "SYN_RECEIVED",
	ESTABLISHED:  "ESTABLISHED",
	FIN_WAIT_1:   "FIN_WAIT_1",
	FIN_WAIT_2:   "FIN_WAIT_2",
	CLOSE_WAIT:   "CLOSE_WAIT",
	LAST_ACK:     "LAST_ACK",
	TIME_WAIT:    "TIME_WAIT",
}

type TCPStack struct {
	SocketTable    map[node.SocketTableKey]Socket
	ip             netip.Addr
	SID_to_sk      map[uint16]node.SocketTableKey
	SID            uint16
	socketTableMtx *sync.Mutex
}

type ReadBuffer struct {
	buffer   []byte
	readMtx  *sync.Mutex // protect pointers
	readCond *sync.Cond
	LBR      uint32
	NXT      uint32
}

type WriteBuffer struct {
	buffer    []byte
	writeMtx  *sync.Mutex // protect pointers
	writeCond *sync.Cond
	UNA       uint32
	NXT       uint32
	LBW       uint32
	writeChan chan uint32
}

type Socket interface {
	VClose(*node.Node, *TCPStack) error
	printSocket()
	getSID() uint16
}

type ListenSocket struct {
	localPort  uint16
	SID        uint16
	listenChan chan node.TCPInfo
}

const (
	RTO_MIN   = 500
	RTO_MAX   = 5000
	RTO_ALPHA = 0.85
	RTO_BETA  = 1.65
)

type NormalSocket struct {
	SID              uint16
	readBuffer       *ReadBuffer
	writeBuffer      *WriteBuffer
	state            uint8
	stateMtx         *sync.Mutex // for if socket is closed
	normalChan       chan node.TCPInfo
	baseSeq          uint32
	baseAck          uint32
	ClientWindowSize uint16
	ackChan          chan node.TCPInfo
	finChan          chan uint32
	unackedNums      map[uint32]int
	unackedFINs      map[uint32]bool
	unackedMtx       *sync.Mutex
	earlyPQ          pq.EarlyPriorityQueue
	index            int // for priority queue/heap stuff
	node.SocketTableKey
	ticker         *time.Ticker
	RTO            float64
	SRTT           float64
	packetDoneChan chan bool
	cumWriteUNA    uint32
	cumReadNXT     uint32
	notFirstWrite  bool
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
	head    *WindowNode
	tail    *WindowNode
	winLock *sync.Mutex
}

type WindowNode struct {
	payloadIndex uint16
	next         *WindowNode
	expectedAck  uint32
	packet       *TCPPacket
	index        uint32
	lastSent     time.Time
	retrans      bool
}

func Initialize(n *node.Node) *TCPStack {
	t := &TCPStack{
		ip:             n.IpAddr,
		SocketTable:    make(map[node.SocketTableKey]Socket),
		SID_to_sk:      make(map[uint16]node.SocketTableKey),
		SID:            0,
		socketTableMtx: &sync.Mutex{},
	}

	// handle matching process
	go func(t *TCPStack) {
		for {
			received := <-n.TCPChan
			key := flipSocketKeyFields(received.SocketTableKey)
			t.socketTableMtx.Lock()
			sock, ok := t.SocketTable[key]
			t.socketTableMtx.Unlock()

			if ok && sock.(*NormalSocket).state != SYN_RECEIVED {
				sock.(*NormalSocket).normalChan <- received
			} else { // add something here for passive close
				listenTuple := node.SocketTableKey{LocalPort: received.RemotePort}

				t.socketTableMtx.Lock()
				sock, ok := t.SocketTable[listenTuple]
				t.socketTableMtx.Unlock()

				if ok {
					sock.(*ListenSocket).listenChan <- received
				}
			}
		}
	}(t)
	return t
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
	t.SocketTable[*SocketTableKey] = lsock
	t.SID_to_sk[t.SID] = *SocketTableKey
	t.SID++
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
		normalChan:     make(chan node.TCPInfo, 100),
		ticker:         time.NewTicker(time.Duration(RTO_MIN * float64(time.Millisecond))),
		RTO:            RTO_MIN,
		SRTT:           -1,
		packetDoneChan: make(chan bool, 1),
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
		window:     WINDOW_SIZE - 1,
	}
	packet := tcpPacket.marshallTCPPacket()
	i := 0
	var ci node.TCPInfo
	timeout := time.NewTicker(3 * time.Second)
	for {
		n.HandleSend(destAddr, packet, 6)
		select {
		case ci = <-newSocket.normalChan: // wait until protocol6 confirms
		case <-timeout.C:
			i++
			fmt.Printf("try num: %d\n", i)
			if i == 3 {
				t.deleteTCB(newSocket)
				return &NormalSocket{}, errors.New("could not connect")
			}
			continue
		}
		if ci.Flag == SYNACK {
			break
		}
		i++
		if i == 3 {
			t.deleteTCB(newSocket)
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
		writeChan: make(chan uint32),
	}
	newSocket.readBuffer = new_read
	newSocket.writeBuffer = new_send
	newSocket.baseAck = ci.SeqNum
	newSocket.baseSeq = ci.AckNum - 1
	newSocket.ClientWindowSize = WINDOW_SIZE - 1
	// newSocket.chans = make(map[uint32]chan bool)
	newSocket.ackChan = make(chan node.TCPInfo, 100) // is 100 the right size
	newSocket.finChan = make(chan uint32)
	newSocket.unackedNums = make(map[uint32]int)
	newSocket.earlyPQ = make(pq.EarlyPriorityQueue, 0)
	newSocket.index = 0
	newSocket.unackedMtx = &sync.Mutex{}
	newSocket.unackedFINs = make(map[uint32]bool)
	newSocket.stateMtx = &sync.Mutex{}

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
		window:     WINDOW_SIZE - 1,
	}
	packet = tcpPacket.marshallTCPPacket()
	// establish connection
	n.HandleSend(destAddr, packet, 6)
	newSocket.cumReadNXT = ci.SeqNum + 1
	newSocket.cumWriteUNA = ci.AckNum
	newSocket.ClientWindowSize = ci.WindowSize

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
		writeChan: make(chan uint32),
	}
	// create new normal socket
	newSK := flipSocketKeyFields(sk)
	newSocket := &NormalSocket{
		SID:            t.SID,
		readBuffer:     new_read,
		writeBuffer:    new_send,
		state:          SYN_RECEIVED,
		SocketTableKey: newSK,
		normalChan:     make(chan node.TCPInfo, 100),
		ackChan:        make(chan node.TCPInfo, 100),
		finChan:        make(chan uint32),
		unackedNums:    make(map[uint32]int),
		unackedFINs:    make(map[uint32]bool),
		earlyPQ:        make(pq.EarlyPriorityQueue, 0),
		index:          0,
		unackedMtx:     &sync.Mutex{},
		ticker:         time.NewTicker(time.Duration(RTO_MIN * float64(time.Millisecond))),
		RTO:            RTO_MIN,
		SRTT:           -1,
		packetDoneChan: make(chan bool, 1),
		stateMtx:       &sync.Mutex{},
	}
	t.SID_to_sk[t.SID] = newSK
	t.SID++
	t.SocketTable[newSK] = newSocket

	newSocket.baseAck = ci.SeqNum
	// newSocket.baseSeq = rand.Uint32()
	newSocket.ClientWindowSize = WINDOW_SIZE - 1
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
		window:     WINDOW_SIZE - 1,
	}
	packet := tcpPacket.marshallTCPPacket()

	n.HandleSend(sk.LocalAddr, packet, 6)
	newSocket.cumReadNXT = ci.SeqNum + 1
	newSocket.cumWriteUNA = newSocket.baseSeq + 1

	// wait for final packet to establish TCP
	for !(ci.Flag == header.TCPFlagAck && ci.SocketTableKey.RemotePort == lsock.localPort) {
		ci = <-lsock.listenChan
	}
	newSocket.state = ESTABLISHED
	newSocket.ClientWindowSize = ci.WindowSize

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

func (sock *NormalSocket) SenderThread(n *node.Node, t *TCPStack) {
	// sock.writeBuffer.writeMtx.Lock()
	// defer sock.writeBuffer.writeMtx.Unlock()
	for {
		// wait for LBW/NXT fields to be updated by VWrite
		// sock.writeBuffer.writeCond.Wait()
		lbw := <-sock.writeBuffer.writeChan

		// distLBW := (sock.writeBuffer.LBW - sock.writeBuffer.UNA) %
		// WINDOW_SIZE
		distLBW := (lbw - sock.writeBuffer.UNA) % WINDOW_SIZE
		distNXT := (sock.writeBuffer.NXT - sock.writeBuffer.UNA) % WINDOW_SIZE
		if distLBW >= distNXT { // order should always be UNA, then LBW NXT so compare distances
			// stuff to send
			amount_to_send := (lbw - sock.writeBuffer.NXT + 1 + WINDOW_SIZE) % WINDOW_SIZE
			// amount_to_send = min(amount_to_send, uint32(sock.ClientWindowSize))

			payload := make([]byte, 0) // change

			first_seg := min(WINDOW_SIZE-sock.writeBuffer.NXT, amount_to_send)
			second_seg := amount_to_send - first_seg
			payload = append(payload, sock.writeBuffer.buffer[sock.writeBuffer.NXT:sock.writeBuffer.NXT+first_seg]...)
			payload = append(payload, sock.writeBuffer.buffer[:second_seg]...)

			// sock.writeBuffer.writeMtx.Lock()
			// sock.readBuffer.readMtx.Lock()
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
			// sock.readBuffer.readMtx.Unlock()

			// fmt.Printf("sending %d bytes: %s\n", amount_to_send, string(payload))
			sock.slidingWindow(packet, n, t) // decided to not make it a goroutine -- revisit later if performance issues
		}
	}
}

func (sock *NormalSocket) recomputeRTO(RTT float64) {
	// fmt.Printf("curr rto: %f\n", sock.RTO)
	if RTT < 0 { // then we're indicating just expiration
		sock.RTO = 2 * sock.RTO
		sock.RTO = max(RTO_MIN, min(RTO_MAX, sock.RTO))
		return
	} else if sock.SRTT < 0 {
		sock.SRTT = RTT
	} else {
		sock.SRTT = (RTO_ALPHA * sock.SRTT) + (1-RTO_ALPHA)*RTT
	}
	sock.RTO = max(RTO_MIN, min(RTO_MAX, RTO_BETA*sock.SRTT))
}

func (sock *NormalSocket) resetTicker() {
	sock.ticker.Reset(time.Duration(sock.RTO * float64(time.Millisecond)))
}

func (sock *NormalSocket) retransThread(window *Window, n *node.Node, t *TCPStack) {
	last_index := -1
	cnt := 0
	for {
		select {
		case <-sock.packetDoneChan:
			return
		case <-sock.ticker.C:
			fmt.Println("ticker expired")
			sock.ticker.Stop()
			// send + restart timer
			sock.recomputeRTO(-1)
			window.winLock.Lock()
			if sock.ClientWindowSize > 0 && window.head != nil { // should not be retransmitting a packet if should be zwp instead
				if window.head.expectedAck != 0 { // update the packet with new ack nums + window size
					// first check if head is still dummy
					win_node := window.head
					if window.head.expectedAck == 0 {
						win_node = window.head.next
					}
					if !win_node.lastSent.IsZero() {
						win_node.packet.ackNum = sock.cumReadNXT
						win_node.packet.window = uint16((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE)
						p := win_node.packet.marshallTCPPacket()
						// indicate this is a retransmission, update lastSent time
						win_node.retrans = true
						win_node.lastSent = time.Now()
						if uint32(last_index) == win_node.index {
							cnt += 1
						} else {
							cnt = 1
							last_index = int(win_node.index)
						}
						if cnt == 10 {
							// terminate
							fmt.Println("Retransmission failed after 10 times; closing connection.")
							defer sock.VClose(n, t)
							return
						} else {
							n.HandleSend(sock.RemoteAddr, p, 6)
						}
					}
				} else if window.head.next != nil {
					win_node := window.head.next
					if !win_node.lastSent.IsZero() {
						win_node.packet.ackNum = sock.cumReadNXT
						win_node.packet.window = uint16((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE)
						p := win_node.packet.marshallTCPPacket()
						// indicate this is a retransmission, update lastSent time
						win_node.retrans = true
						win_node.lastSent = time.Now()
						n.HandleSend(sock.RemoteAddr, p, 6)
					}
				}
			}
			window.winLock.Unlock()
			sock.resetTicker()
		}
	}
}

func (sock *NormalSocket) slidingWindow(packet TCPPacket, n *node.Node, t *TCPStack) {
	// for testing purposes, split total payload into segments of size 1
	// should eventually be MSS (max ip packet size - ip header - tcp header)
	// sock.writeBuffer.writeMtx.Lock()
	payloadSize := len(packet.payload)

	// allocate first node/packet to send
	bytesToSend := min(int(MSS), len(packet.payload))
	bytesToSend = min(bytesToSend, int(sock.ClientWindowSize))
	firstPacket := &TCPPacket{
		sourceIp:   sock.LocalAddr,
		destIp:     sock.RemoteAddr,
		payload:    packet.payload[:bytesToSend],
		flags:      packet.flags,
		sourcePort: sock.LocalPort,
		destPort:   sock.RemotePort,
		seqNum:     sock.cumWriteUNA, // ???
		ackNum:     sock.cumReadNXT,
		window:     uint16((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE),
	}
	packet.payload = packet.payload[bytesToSend:]
	// sock.writeBuffer.NXT += uint32(bytesToSend) // move NXT?
	distUNA := (sock.writeBuffer.NXT - sock.writeBuffer.UNA + WINDOW_SIZE) % WINDOW_SIZE
	tail := &WindowNode{
		payloadIndex: 0,
		packet:       firstPacket,
		expectedAck:  distUNA + sock.cumWriteUNA + uint32(bytesToSend),
		index:        0,
	}
	head := &WindowNode{
		next: tail,
	}
	window := &Window{
		head:    head,
		tail:    tail,
		winLock: &sync.Mutex{},
	}

	first := true
	sock.ticker.Stop()
	// reset channel
	for len(sock.ticker.C) > 0 {
		<-sock.ticker.C
	}
	go sock.retransThread(window, n, t)

	bytesSent := 0
	//startTime := time.Now()
	skip_probe := true
	for bytesSent < payloadSize {
		window.winLock.Lock()
		// zero window probing
		for (sock.writeBuffer.NXT == sock.writeBuffer.UNA) && !skip_probe && (sock.ClientWindowSize == 0) {
			fmt.Println("zwp")
			// send first unacked byte
			sock.writeBuffer.NXT = (sock.writeBuffer.NXT + 1) % WINDOW_SIZE
			distUNA = (sock.writeBuffer.NXT - sock.writeBuffer.UNA + WINDOW_SIZE) % WINDOW_SIZE
			b := []byte{sock.writeBuffer.buffer[sock.writeBuffer.UNA]}

			probe_packet := &TCPPacket{
				sourceIp:   sock.LocalAddr,
				destIp:     sock.RemoteAddr,
				payload:    b,
				flags:      header.TCPFlagAck,
				sourcePort: sock.LocalPort,
				destPort:   sock.RemotePort,
				seqNum:     distUNA + sock.cumWriteUNA - 1,
				ackNum:     sock.cumReadNXT,
				window:     uint16((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE),
			}
			probe := probe_packet.marshallTCPPacket()
			expectedAck := distUNA + sock.cumWriteUNA
			sock.unackedMtx.Lock()
			sock.unackedNums[expectedAck] = 0
			sock.unackedMtx.Unlock()

			n.HandleSend(sock.RemoteAddr, probe, 6)
			// wait for ack
			timeout := time.NewTicker(1 * time.Second)
			for {
				select {
				case received := <-sock.ackChan:
					if received.AckNum == expectedAck && received.WindowSize > 0 {
						sock.ClientWindowSize = received.WindowSize
						bytesSent += 1
						sock.cumWriteUNA = received.AckNum
						sock.writeBuffer.UNA = (sock.cumWriteUNA - sock.baseSeq + WINDOW_SIZE) % WINDOW_SIZE
						// ok so the probe packet actually fucks with the original packet we might want to send, the tail
						if len(tail.packet.payload) == 0 {
							// this happens when bytestosend starts as 0, or we know off the bat client window size is 0
							// wait just restart then lol
							packet.payload = packet.payload[1:]
							sock.packetDoneChan <- true
							skip_probe = true
							defer sock.slidingWindow(packet, n, t)
							return
						} else {
							tail.packet.payload = tail.packet.payload[1:]
						}
						// only weird thing is what if it was a packet of size 0... ok we still end up sending it and will get ack back so its fine i think
						skip_probe = true
						goto normal
					}
				case <-timeout.C:
					n.HandleSend(sock.RemoteAddr, probe, 6)
				}
			}
		}
	normal:
		if (sock.ClientWindowSize > 0) && (bytesToSend > 0) && ((window.head != nil && window.head.next == nil) || window.tail.index-window.head.next.index < WINDOW_SIZE) {
			//fmt.Printf("window size: %d\n", sock.ClientWindowSize)
			sock.writeBuffer.NXT = (sock.writeBuffer.NXT + uint32(bytesToSend)) % WINDOW_SIZE
			distUNA = (sock.writeBuffer.NXT - sock.writeBuffer.UNA + WINDOW_SIZE) % WINDOW_SIZE
			expectedAck := distUNA + sock.cumWriteUNA
			sock.unackedMtx.Lock()
			sock.unackedNums[expectedAck] = 0
			sock.unackedMtx.Unlock()

			p := window.tail.packet.marshallTCPPacket()
			window.tail.retrans = false
			window.tail.lastSent = time.Now()
			n.HandleSend(sock.RemoteAddr, p, 6)
			// i think ideal is to adjust client window size here as predicted, just in case delays occur?
			sock.ClientWindowSize -= uint16(len(p) - 20)
			if first {
				first = false
				sock.resetTicker()
			}

			// allocate next window node
			bytesToSend = min(int(MSS), payloadSize-int(window.tail.payloadIndex), len(packet.payload)) // last segment will have fewer than MSS bytes
			if bytesToSend > 0 {                                                                        // don't need to make next packet if no more bytes to send
				nextPacket := &TCPPacket{
					sourceIp:   window.tail.packet.sourceIp,
					destIp:     window.tail.packet.destIp,
					payload:    packet.payload[:bytesToSend],
					flags:      window.tail.packet.flags,
					sourcePort: window.tail.packet.sourcePort,
					destPort:   window.tail.packet.destPort,
					seqNum:     expectedAck,
					ackNum:     sock.cumReadNXT,
					window:     uint16((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE),
				}
				packet.payload = packet.payload[bytesToSend:]
				nextNode := &WindowNode{
					payloadIndex: window.tail.payloadIndex + MSS,
					packet:       nextPacket,
					expectedAck:  expectedAck + uint32(bytesToSend),
					index:        window.tail.index + 1,
				}
				window.tail.next = nextNode
				window.tail = window.tail.next
			}
		}
		if sock.ClientWindowSize == 0 {
			skip_probe = false
		}

		// check which packets were ACKed
		for len(sock.ackChan) > 0 {
			received := <-sock.ackChan
			receivedAck := received.AckNum
			sock.unackedMtx.Lock()
			//delete(sock.unackedNums, receivedAck)
			if sock.unackedNums[receivedAck] == 0 {
				// first time seeing
				sock.unackedNums[receivedAck] += 1
			} else {
				// dup ack
				continue
			}
			sock.unackedMtx.Unlock()

			sock.ClientWindowSize = received.WindowSize

			recievedTime := time.Now()

			// if we received an ACK ahead of UNA (can ignore ACKs before UNA
			// except wrap around case!!)
			if receivedAck > sock.cumWriteUNA { // fix this
				bytesSent += int(receivedAck - sock.cumWriteUNA) // fix after
				sock.cumWriteUNA = receivedAck
				sock.writeBuffer.UNA = (sock.cumWriteUNA - sock.baseSeq + WINDOW_SIZE) % WINDOW_SIZE // same
				// shrink window from head
				if window.head != nil {
					for window.head.expectedAck <= receivedAck { // think about later
						if !window.head.retrans && !window.head.lastSent.IsZero() { // update the RTO
							sock.resetTicker()
							diff := recievedTime.Sub(window.head.lastSent).Milliseconds()
							sock.recomputeRTO(float64(diff))
						}
						window.head = window.head.next
						if window.head == nil {
							break
						}
					}
				}
			}
		}
		window.winLock.Unlock()
	}
	sock.packetDoneChan <- true
	// fmt.Println("finished sending")
	fmt.Printf("Sent %d bytes\n", payloadSize)

}

func (sock *NormalSocket) ReceiverThread(n *node.Node, t *TCPStack) {
	var received node.TCPInfo
	for {
		received = <-sock.normalChan
		if received.Flag&header.TCPFlagAck > 0 {
			sock.ClientWindowSize = received.WindowSize

			sock.unackedMtx.Lock()
			if _, ok := sock.unackedFINs[received.AckNum]; ok {
				sock.finChan <- received.AckNum
				sock.unackedMtx.Unlock()
				continue
			}
			// here we received an ack packet in response to what we sent
			if _, ok := sock.unackedNums[received.AckNum]; ok { // TODO: possible that this received ACK num is in map but not an ACK in response to what we sent
				if len(received.Payload) == 0 {
					// not a dupack, new stuff
					sock.ackChan <- received
					sock.unackedMtx.Unlock()
					continue
				}
			}
			sock.unackedMtx.Unlock()

			// TODO: account for when window runs out of space
			// update read buffer pointers
			// write the payload to the read buffer

			// put into early queue if packet is out of order
			if received.SeqNum > sock.cumReadNXT { // fix this ... might need to change priority to signed distance from pointer
				ep := pq.EarlyPacket{
					Priority: received.SeqNum, // need to handle wrap around
					Index:    sock.index,
					Flags:    received.Flag,
					Payload:  received.Payload,
				}
				sock.index++
				heap.Push(&sock.earlyPQ, &ep) // pq is ordered by seqnum
				// goto sendack                  // TODO: send duplicate ACK,
				// currently this causes some bug
				continue
			}

			// check if this is FIN
			sock.readBuffer.readMtx.Lock()

			if received.Flag&header.TCPFlagFin > 0 {
				sock.cumReadNXT += 1
				sock.stateMtx.Lock()
				if sock.state == ESTABLISHED {
					sock.state = CLOSE_WAIT
					sock.stateMtx.Unlock()
				} else if sock.state == FIN_WAIT_2 {
					sock.state = TIME_WAIT
					sock.stateMtx.Unlock()
					go func(t *TCPStack, sock *NormalSocket) {
						time.Sleep(120 * time.Second)
						t.deleteTCB(sock)
					}(t, sock)
				}
				// sock.stateMtx.Unlock()
			} else if ((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE) != 0 {
				// insert packet payload into read buffer if NOT FULL
				// check size... full if next to be written into is not read yet, so NXT + 1 == LBR
				to_add := uint32(len(received.Payload))
				sock.cumReadNXT = received.SeqNum + uint32(len(received.Payload))
				for to_add > 0 {
					curr := min(WINDOW_SIZE-sock.readBuffer.NXT, to_add)
					to_add -= curr
					copy(sock.readBuffer.buffer[sock.readBuffer.NXT:sock.readBuffer.NXT+curr], received.Payload[:curr])
					received.Payload = received.Payload[curr:]
					sock.readBuffer.NXT = (sock.readBuffer.NXT + curr) % WINDOW_SIZE
				}
				//copy(sock.readBuffer.buffer[sock.readBuffer.NXT:sock.readBuffer.NXT+uint32(len(received.Payload))], received.Payload)
				sock.readBuffer.NXT = (sock.cumReadNXT - sock.baseAck + WINDOW_SIZE) % WINDOW_SIZE
			} else {
				sock.readBuffer.readCond.Broadcast()
				sock.readBuffer.readMtx.Unlock()
				continue
			}
			// insert early arrival packets if possible
			for len(sock.earlyPQ) > 0 && sock.earlyPQ[0].Priority <= sock.cumReadNXT { //TODO: wrap around
				if sock.earlyPQ[0].Priority < sock.cumReadNXT { // packet was already copied into buffer
					// TODO: if priority changed to displacement, adjust RHS
					heap.Pop(&sock.earlyPQ)
				} else {
					earlyPacket := heap.Pop(&sock.earlyPQ).(*pq.EarlyPacket)
					// check if this is FIN
					if earlyPacket.Flags&header.TCPFlagFin > 0 {
						sock.stateMtx.Lock()
						if sock.state == ESTABLISHED {
							sock.state = CLOSE_WAIT
							sock.stateMtx.Unlock()
						} else if sock.state == FIN_WAIT_2 {
							sock.state = TIME_WAIT
							sock.stateMtx.Unlock()
							go func(t *TCPStack, sock *NormalSocket) {
								time.Sleep(120 * time.Second)
								t.deleteTCB(sock)
							}(t, sock)
						}
						// sock.stateMtx.Unlock()

					} else {
						to_add := uint32(len(earlyPacket.Payload))
						sock.cumReadNXT = earlyPacket.Priority + uint32(len(earlyPacket.Payload))
						for to_add > 0 {
							curr := min(WINDOW_SIZE-sock.readBuffer.NXT, to_add)
							to_add -= curr
							copy(sock.readBuffer.buffer[sock.readBuffer.NXT:sock.readBuffer.NXT+curr], earlyPacket.Payload[:curr])
							earlyPacket.Payload = earlyPacket.Payload[curr:]
							sock.readBuffer.NXT = (sock.readBuffer.NXT + curr) % WINDOW_SIZE
						}
						sock.readBuffer.NXT = (sock.cumReadNXT - sock.baseAck + WINDOW_SIZE) % WINDOW_SIZE
					}
				}
			}

			sock.readBuffer.readCond.Broadcast() // or signal? to unblock VRead
			sock.readBuffer.readMtx.Unlock()

			// sendack: // send back ACK
			new_window := (sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE
			sk := received.SocketTableKey
			tcpPacket := TCPPacket{
				sourceIp:   sk.RemoteAddr,
				destIp:     sk.LocalAddr,
				payload:    nil,
				flags:      header.TCPFlagAck,
				sourcePort: sk.RemotePort,
				destPort:   sk.LocalPort,
				seqNum:     received.AckNum,
				ackNum:     sock.cumReadNXT,
				window:     uint16(new_window),
			}
			// TODO: do i need to update clientwindowsize here?
			packet := tcpPacket.marshallTCPPacket()
			n.HandleSend(sk.LocalAddr, packet, 6)
		}
	}
}

func (sock *NormalSocket) VWrite(message []byte) error {
	sock.stateMtx.Lock()
	if sock.state == FIN_WAIT_1 || sock.state == FIN_WAIT_2 || sock.state == TIME_WAIT {
		sock.stateMtx.Unlock()
		return errors.New("sock closed")
	}
	sock.stateMtx.Unlock()
	bytesSent := uint32(0)
	new_message := true
	for bytesSent < uint32(len(message)) {
		space := (sock.writeBuffer.UNA - 1 - sock.writeBuffer.LBW + WINDOW_SIZE) % WINDOW_SIZE
		if !sock.notFirstWrite {
			space = WINDOW_SIZE - 1 - sock.writeBuffer.LBW
			sock.notFirstWrite = true
			new_message = false
		} else if new_message {
			new_message = false
			space = WINDOW_SIZE - 1 - sock.writeBuffer.LBW
		}
		to_write := min(uint32(len(message))-bytesSent, space, uint32(node.MaxMessageSize-40)) // max is maxmsg - ip header - tcp header

		first_seg := min(WINDOW_SIZE-sock.writeBuffer.LBW-1, to_write)
		second_seg := to_write - first_seg

		copy(sock.writeBuffer.buffer[sock.writeBuffer.LBW+1:sock.writeBuffer.LBW+1+first_seg], message[bytesSent:bytesSent+first_seg])
		copy(sock.writeBuffer.buffer[:second_seg], message[bytesSent+first_seg:bytesSent+first_seg+second_seg])

		bytesSent += to_write

		// update pointer
		sock.writeBuffer.LBW = (sock.writeBuffer.LBW + to_write) % WINDOW_SIZE
		// sock.writeBuffer.writeCond.Signal()
		sock.writeBuffer.writeChan <- sock.writeBuffer.LBW
		// sock.writeBuffer.writeMtx.Unlock()
		//fmt.Println(bytesSent)
	}
	return nil
}

func (sock *NormalSocket) VRead(numbytes uint16, file *os.File) error {
	sock.stateMtx.Lock()
	if sock.state == CLOSE_WAIT || sock.state == LAST_ACK {
		sock.stateMtx.Unlock()
		return errors.New("sock closed")
	}
	sock.stateMtx.Unlock()
	num_buf := (sock.readBuffer.NXT - 1 - sock.readBuffer.LBR + WINDOW_SIZE) % WINDOW_SIZE
	//fmt.Printf("amount to read: %d\n", num_buf)

	// block if nothing to read
	sock.readBuffer.readMtx.Lock()
	defer sock.readBuffer.readMtx.Unlock()
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

	toRead := append(sock.readBuffer.buffer[sock.readBuffer.LBR+1:first_seg+1], sock.readBuffer.buffer[0:second_seg]...)

	if file != nil {
		if _, err := file.Write(toRead); err != nil {
			fmt.Println(err)
			return err
		}
	} else {
		fmt.Printf("Read %d bytes: %s\n", num_read, string(toRead))
	}
	// adjust LBR
	sock.readBuffer.LBR = (sock.readBuffer.LBR + uint32(num_read)) % WINDOW_SIZE

	return nil
}

func (sock *NormalSocket) VClose(n *node.Node, t *TCPStack) error {
	// send FIN/ACK
	distUNA := (sock.writeBuffer.NXT - sock.writeBuffer.UNA + WINDOW_SIZE) % WINDOW_SIZE
	seq := distUNA + sock.cumWriteUNA
	ack := sock.cumReadNXT
	// if !sock.notFirstWrite {
	// 	sock.readBuffer.NXT++
	// 	seq++
	// 	sock.notFirstWrite = true
	// }
	packet := TCPPacket{
		sourceIp:   sock.LocalAddr,
		destIp:     sock.RemoteAddr,
		payload:    nil,
		flags:      FINACK,
		sourcePort: sock.LocalPort,
		destPort:   sock.RemotePort,
		seqNum:     seq,
		ackNum:     ack,
		window:     uint16((sock.readBuffer.LBR - sock.readBuffer.NXT + WINDOW_SIZE) % WINDOW_SIZE),
	}
	p := packet.marshallTCPPacket()
	sock.stateMtx.Lock()
	defer sock.stateMtx.Unlock()
	if sock.state == ESTABLISHED {
		sock.state = FIN_WAIT_1
	} else if sock.state == CLOSE_WAIT {
		sock.state = LAST_ACK
	} else {
		return errors.New("current socket state is invalid")
	}

	sock.unackedMtx.Lock()
	sock.unackedFINs[packet.seqNum+1] = true
	sock.unackedMtx.Unlock()

	n.HandleSend(sock.RemoteAddr, p, 6)

	// wait for ACK
	go func() {
		for {
			receivedAck := <-sock.finChan
			if receivedAck == packet.seqNum+1 {
				sock.unackedMtx.Lock()
				delete(sock.unackedFINs, receivedAck)
				sock.unackedMtx.Unlock()
				sock.stateMtx.Lock()
				if sock.state == FIN_WAIT_1 {
					sock.state = FIN_WAIT_2
				} else if sock.state == LAST_ACK {
					t.deleteTCB(sock)
				}
				sock.stateMtx.Unlock()

				return
			}
		}
	}()
	return nil
}

func (sock *ListenSocket) VClose(n *node.Node, t *TCPStack) error {
	t.deleteTCB(sock)
	return nil
}

func (sock *NormalSocket) getSID() uint16 { return sock.SID }
func (sock *ListenSocket) getSID() uint16 { return sock.SID }

func (t *TCPStack) deleteTCB(sock Socket) {
	sk := t.SID_to_sk[sock.getSID()]
	t.socketTableMtx.Lock()
	delete(t.SocketTable, sk)
	t.socketTableMtx.Unlock()
	delete(t.SID_to_sk, sock.getSID())
}

func (t *TCPStack) SendFile(file *os.File, addr netip.Addr, port uint16, n *node.Node) error {
	// Get the file size
	stat, err := file.Stat()
	if err != nil {
		return err
	}

	// Read the file into a byte slice
	bs := make([]byte, stat.Size())
	_, err = bufio.NewReader(file).Read(bs)
	if err != nil && err != io.EOF {
		return err
	}

	sock, err := t.VConnect(addr, port, n)
	if err != nil {
		return err
	}
	go sock.ReceiverThread(n, t)
	go sock.SenderThread(n, t)
	time.Sleep(1 * time.Second)
	fmt.Println(len(bs))
	err = sock.VWrite(bs)
	if err != nil {
		fmt.Println(err)
		return err
	}
	time.Sleep(50 * time.Millisecond)
	err = sock.VClose(n, t)
	return err
}

func (t *TCPStack) ReceiveFile(filename string, port uint16, n *node.Node) error {
	lsock, err := t.VListen(port)
	if err != nil {
		return err
	}
	go func() {
		// accept connection
		sock, err := lsock.VAccept(t, n)
		if err != nil {
			return
		} else {
			fmt.Println("connection established")
		}
		file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			fmt.Println(err)
			return
		}
		go sock.ReceiverThread(n, t)
		go sock.SenderThread(n, t)

		// receive logic
		for {
			sock.stateMtx.Lock()
			if sock.state == CLOSE_WAIT {
				sock.stateMtx.Unlock()
				time.Sleep(50 * time.Millisecond)
				sock.VClose(n, t)
				break
			}
			sock.stateMtx.Unlock()
			//			fmt.Printf("nxt: %d, lbr: %d\n", sock.readBuffer.NXT, sock.readBuffer.LBR)
			if (sock.readBuffer.NXT-1-sock.readBuffer.LBR+WINDOW_SIZE)%WINDOW_SIZE > 0 {
				//fmt.Println("go")
				err := sock.VRead(uint16(1<<16-1), file)
				if err != nil {
					fmt.Println(err)
				}
			}
		}
		fmt.Println("finished receiving file")
	}()
	return nil
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
