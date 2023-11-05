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

var stateMap = map[uint8]string{
	LISTEN:       "LISTEN",
	SYN_SENT:     "SYN_SENT",
	SYN_RECEIVED: "SYN_RECEIVED",
	ESTABLISHED:  "ESTABLISHED",
}

type TCPStack struct {
	socketTable map[node.SocketTableKey]Socket
	ip          netip.Addr
	SID         uint16
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
	readBuffer  []byte
	writeBuffer []byte
	state       uint8
	node.SocketTableKey
	normalChan chan node.TCPInfo
}

func Initialize(n *node.Node) TCPStack {
	tcpStack := &TCPStack{ip: n.IpAddr, socketTable: make(map[node.SocketTableKey]Socket), SID: 0}

	// handle matching process
	go func() {
		for {
			received := <-n.TCPChan

			// server and client flipped??
			if sock, ok := tcpStack.socketTable[node.SocketTableKey{ClientAddr: received.ServerAddr, ClientPort: received.ServerPort, ServerAddr: received.ClientAddr, ServerPort: received.ClientPort}]; ok {
				sock.(*NormalSocket).normalChan <- received
			} else {
				listenTuple := node.SocketTableKey{ClientPort: received.ServerPort}
				if sock, ok := tcpStack.socketTable[listenTuple]; ok {
					sock.(*ListenSocket).listenChan <- received
				}
			}
		}
	}()
	return *tcpStack
}

func (t *TCPStack) VListen(port uint16) (*ListenSocket, error) {
	SocketTableKey := new(node.SocketTableKey)
	SocketTableKey.ClientPort = port
	if _, ok := t.socketTable[*SocketTableKey]; ok {
		// already listening on port
		return nil, fmt.Errorf("already listening on port %d", port)
	}
	lsock := &ListenSocket{localPort: port, listenChan: make(chan node.TCPInfo), SID: t.SID} // TODO: chan blocking?
	t.SID++
	t.socketTable[*SocketTableKey] = lsock
	return lsock, nil
}

func (t *TCPStack) VConnect(destAddr netip.Addr, destPort uint16, n *node.Node) (NormalSocket, error) {
	// generate new random port
	var randSrcPort uint16
	for {
		randSrcPort = uint16(rand.Intn(65535-20000) + 20000)
		SocketTableKey := &node.SocketTableKey{ClientPort: randSrcPort}
		if _, ok := t.socketTable[*SocketTableKey]; !ok {
			break
		}
	}
	sk := &node.SocketTableKey{ClientAddr: t.ip, ClientPort: randSrcPort, ServerAddr: destAddr, ServerPort: destPort}
	newSocket := &NormalSocket{
		SID:            t.SID,
		readBuffer:     make([]byte, 0),
		writeBuffer:    make([]byte, 0),
		state:          SYN_SENT,
		SocketTableKey: *sk,
		normalChan:     make(chan node.TCPInfo), // TODO: chan blocking?
	}
	t.SID++
	t.socketTable[*sk] = newSocket

	seqNum := rand.Uint32()
	tcpPacket := makeTCPPacket(t.ip, destAddr, nil, header.TCPFlagSyn, randSrcPort, destPort, seqNum, 0) //TODO: ack num?
	i := 0
	var ci node.TCPInfo
	var timeout chan bool
	for {
		n.HandleSend(destAddr, tcpPacket, 6)
		go func(timeout chan bool) {
			time.Sleep(3 * time.Second)
			timeout <- true
		}(timeout)
		select {
		case ci = <-newSocket.normalChan: // wait until protocol6 confirms
		case <-timeout:
			i++
			if i == 3 {
				return NormalSocket{}, errors.New("could not connect")
			}
			continue
		}
		if ci.Flag == SYNACK {
			break
		}
		i++
		if i == 3 {
			return NormalSocket{}, errors.New("could not connect")
		}
	}

	t.socketTable[*sk].(*NormalSocket).state = ESTABLISHED
	tcpPacket = makeTCPPacket(t.ip, destAddr, nil, header.TCPFlagAck, randSrcPort, destPort, ci.AckNum, ci.SeqNum+1)
	// establish connection
	n.HandleSend(destAddr, tcpPacket, 6)

	return *newSocket, nil
}

func (lsock *ListenSocket) VAccept(t *TCPStack, n *node.Node) (*NormalSocket, error) {
	// wait for SYN
	var ci node.TCPInfo
	for !(ci.Flag == header.TCPFlagSyn && ci.SocketTableKey.ServerPort == lsock.localPort) {
		ci = <-lsock.listenChan
	}
	sk := ci.SocketTableKey

	// create new normal socket
	newSocket := &NormalSocket{
		SID:            t.SID,
		readBuffer:     make([]byte, 0),
		writeBuffer:    make([]byte, 0),
		state:          SYN_RECEIVED,
		SocketTableKey: sk,
		normalChan:     make(chan node.TCPInfo), // TODO: chan blocking?
	}
	t.SID++
	t.socketTable[sk] = newSocket

	// send SYN+ACK back to client
	seqNum := rand.Uint32() // TODO: random?
	tcpPacket := makeTCPPacket(sk.ServerAddr, sk.ClientAddr, nil, SYNACK, sk.ServerPort, sk.ClientPort, seqNum, ci.SeqNum+1)

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

func (socket *NormalSocket) VWrite() error {
	panic("todo")
}

func (socket *NormalSocket) VRead() error {
	panic("todo")
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
	fmt.Printf("  0    0.0.0.0 %5d    0.0.0.0     0       LISTEN\n", socket.localPort)
}

func (t *TCPStack) PrintTable() {
	fmt.Println("SID      LAddr LPort      RAddr RPort       Status")
	for _, socket := range t.socketTable {
		socket.printSocket()
	}
}
