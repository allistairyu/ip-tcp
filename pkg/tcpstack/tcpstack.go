package tcpstack

import (
	"errors"
	"fmt"
	"iptcp/pkg/iptcp_utils"
	"iptcp/pkg/node"
	"math/rand"
	"net/netip"
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
const WINDOW_SIZE = 65536

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
	buffer []byte
	LBR    uint32
	NXT    uint32
	// add some sort of queue for out of order
}

type WriteBuffer struct {
	buffer []byte
	UNA    uint32
	NXT    uint32
	LBW    uint32
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
	SID         uint16
	readBuffer  *ReadBuffer
	writeBuffer *WriteBuffer
	state       uint8
	node.SocketTableKey
	normalChan chan node.TCPInfo
	baseSeq    uint32
	baseAck    uint32
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
				listenTuple := node.SocketTableKey{ClientPort: received.ServerPort}
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
		ClientAddr: sk.ServerAddr,
		ClientPort: sk.ServerPort,
		ServerAddr: sk.ClientAddr,
		ServerPort: sk.ClientPort,
	}
}

func (t *TCPStack) VListen(port uint16) (*ListenSocket, error) {
	SocketTableKey := new(node.SocketTableKey)
	SocketTableKey.ClientPort = port
	if _, ok := t.SocketTable[*SocketTableKey]; ok {
		// already listening on port
		return nil, fmt.Errorf("already listening on port %d", port)
	}
	lsock := &ListenSocket{localPort: port, listenChan: make(chan node.TCPInfo), SID: t.SID} // TODO: chan blocking?
	t.SID++
	t.SocketTable[*SocketTableKey] = lsock
	return lsock, nil
}

func (t *TCPStack) VConnect(destAddr netip.Addr, destPort uint16, n *node.Node) (*NormalSocket, error) {
	// generate new random port
	var randSrcPort uint16
	for {
		randSrcPort = uint16(rand.Intn(65535-20000) + 20000)
		SocketTableKey := &node.SocketTableKey{ClientPort: randSrcPort}
		if _, ok := t.SocketTable[*SocketTableKey]; !ok {
			break
		}
	}
	// TESTING:
	randSrcPort = 20000

	sk := node.SocketTableKey{ClientAddr: t.ip, ClientPort: randSrcPort, ServerAddr: destAddr, ServerPort: destPort}
	newSocket := &NormalSocket{
		SID:            t.SID,
		state:          SYN_SENT,
		SocketTableKey: sk,
		normalChan:     make(chan node.TCPInfo), // TODO: chan blocking?
	}
	t.SID_to_sk[t.SID] = sk
	t.SID++
	t.SocketTable[sk] = newSocket

	seqNum := rand.Uint32()
	// TESTING
	seqNum = 5000
	tcpPacket := makeTCPPacket(t.ip, destAddr, nil, header.TCPFlagSyn, randSrcPort, destPort, seqNum, 0)
	i := 0
	var ci node.TCPInfo
	timeout := make(chan bool)
	// TO FIX: it never processes that true is passed into timeout, gets stuck at select block
	for {
		if i > 0 {
			n.HandleSend(destAddr, tcpPacket, 6)
		}
		go func(tmt chan bool) { // TODO: test timeout stuff
			time.Sleep(3 * time.Second)
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
	new_read := &ReadBuffer{
		buffer: make([]byte, WINDOW_SIZE),
		LBR:    0,
		NXT:    1,
	}
	new_send := &WriteBuffer{
		buffer: make([]byte, WINDOW_SIZE),
		UNA:    1,
		NXT:    1,
		LBW:    0,
	}
	newSocket.readBuffer = new_read
	newSocket.writeBuffer = new_send
	newSocket.baseAck = ci.SeqNum
	newSocket.baseSeq = ci.AckNum - 1

	t.SocketTable[sk].(*NormalSocket).state = ESTABLISHED
	tcpPacket = makeTCPPacket(t.ip, destAddr, nil, header.TCPFlagAck, randSrcPort, destPort, ci.AckNum, ci.SeqNum+1)
	// establish connection
	n.HandleSend(destAddr, tcpPacket, 6)

	return newSocket, nil
}

func (lsock *ListenSocket) VAccept(t *TCPStack, n *node.Node) (*NormalSocket, error) {
	// wait for SYN
	var ci node.TCPInfo
	for !(ci.Flag == header.TCPFlagSyn && ci.SocketTableKey.ServerPort == lsock.localPort) {
		ci = <-lsock.listenChan
	}
	sk := ci.SocketTableKey

	new_read := &ReadBuffer{
		buffer: make([]byte, WINDOW_SIZE),
		LBR:    0,
		NXT:    1,
	}
	new_send := &WriteBuffer{
		buffer: make([]byte, WINDOW_SIZE),
		UNA:    1,
		NXT:    1,
		LBW:    0,
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
	}
	t.SID_to_sk[t.SID] = newSK
	t.SID++
	t.SocketTable[newSK] = newSocket

	newSocket.baseAck = ci.SeqNum
	newSocket.baseSeq = rand.Uint32()
	// TESTING
	newSocket.baseSeq = 6000

	// send SYN+ACK back to client
	tcpPacket := makeTCPPacket(sk.ServerAddr, sk.ClientAddr, nil, SYNACK, sk.ServerPort, sk.ClientPort, newSocket.baseSeq, ci.SeqNum+1)

	n.HandleSend(sk.ClientAddr, tcpPacket, 6)

	// wait for final packet to establish TCP
	for !(ci.Flag == header.TCPFlagAck && ci.SocketTableKey.ServerPort == lsock.localPort) {
		ci = <-lsock.listenChan
	}
	newSocket.state = ESTABLISHED

	return newSocket, nil
}

func makeTCPPacket(sourceIp netip.Addr, destIp netip.Addr,
	payload []byte, flags uint8, sourcePort uint16, destPort uint16,
	seqNum uint32, ackNum uint32) []byte {

	tcpHdr := header.TCPFields{
		SrcPort:       sourcePort,
		DstPort:       destPort,
		SeqNum:        seqNum,
		AckNum:        ackNum,
		DataOffset:    20,
		Flags:         flags,
		WindowSize:    65535,
		Checksum:      0,
		UrgentPointer: 0,
	}

	checksum := iptcp_utils.ComputeTCPChecksum(&tcpHdr, sourceIp, destIp, payload)
	tcpHdr.Checksum = checksum

	// Serialize the TCP header
	tcpHeaderBytes := make(header.TCP, iptcp_utils.TcpHeaderLen)
	tcpHeaderBytes.Encode(&tcpHdr)

	// Combine the TCP header + payload into one byte array, which
	// becomes the payload of the IP packet
	ipPacketPayload := make([]byte, 0, len(tcpHeaderBytes)+len(payload))
	ipPacketPayload = append(ipPacketPayload, tcpHeaderBytes...)
	ipPacketPayload = append(ipPacketPayload, []byte(payload)...)

	return ipPacketPayload
}

// FOR SENDER + RECEIVER: later if we need to deal with clean shutdowns, im guessing channels would work here (or some sort of var)

func (socket *NormalSocket) SenderThread() {
	// i think the whole channel thing is prolly applicable here; wake up when new stuff written?
	// actually not too sure
	// ok ok it should be like
	/*
		for {
			if (ack is not caught up) {
				// logic
			}
			else {
				all caught up now, just need to wait for new stuff to be written

				use channel to wake up this thread
			}
		}
	*/
	for {

	}
}

func (socket *NormalSocket) ReceiverThread() {
	// this should be more straight forward??
	// p sure its just direct impl
	var received node.TCPInfo
	for {
		received = <-socket.normalChan
		if received.Flag == header.TCPFlagAck {
			// update read buffer pointers
			// write the payload to the read buffer
			// assume for now that bytes are guaranteed to be in order
			socket.readBuffer.NXT = received.AckNum
			socket.readBuffer.buffer = append(socket.readBuffer.buffer, received.Payload...)
			fmt.Printf("receiver thread: ack %d, %s\n", received.AckNum, socket.readBuffer.buffer)
		}
	}
}

func (socket *NormalSocket) VWrite(message []byte) error {
	// first get how much left to write: this is LBW to UNA (so writing from LBW + 1 to UNA - 1)
	space := (socket.writeBuffer.UNA - 1 - socket.writeBuffer.LBW + WINDOW_SIZE) % WINDOW_SIZE
	if space == 0 {
		space = WINDOW_SIZE
	}
	to_write := min(uint32(len(message)), space)
	to_write = min(to_write, uint32(node.MaxMessageSize-40)) // max is maxmsg - ip header - tcp header

	first_seg := min(WINDOW_SIZE-socket.writeBuffer.LBW, to_write)
	second_seg := to_write - first_seg

	copy(socket.writeBuffer.buffer[socket.writeBuffer.LBW:first_seg], message[:first_seg])
	copy(socket.writeBuffer.buffer[:second_seg], message[first_seg:first_seg+second_seg])

	// update pointer
	socket.writeBuffer.LBW = (socket.writeBuffer.LBW + to_write) % WINDOW_SIZE
	return nil
}

func (socket *NormalSocket) VRead(numbytes uint16) error {
	num_buf := (socket.readBuffer.NXT - socket.readBuffer.LBR + WINDOW_SIZE) % WINDOW_SIZE
	num_read := min(num_buf, uint32(numbytes))

	// read up to this
	first_seg := min(socket.readBuffer.LBR+uint32(num_read), WINDOW_SIZE-1)
	second_seg := num_read - (first_seg - socket.readBuffer.LBR) // in case it wraps around

	// append the two parts (hopefully slice[0:0] is just empty)
	toRead := append(socket.readBuffer.buffer[socket.readBuffer.LBR:socket.readBuffer.LBR+first_seg], socket.readBuffer.buffer[0:second_seg]...)

	fmt.Printf("Read %d bytes: %s\n", num_read, string(toRead))

	// adjust LBR
	socket.readBuffer.LBR = (socket.readBuffer.LBR + uint32(num_read)) % WINDOW_SIZE
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
		socket.ClientAddr, socket.ClientPort, socket.ServerAddr, socket.ServerPort, stateMap[socket.state])
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
