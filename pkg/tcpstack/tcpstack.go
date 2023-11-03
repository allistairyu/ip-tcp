package tcpstack

import (
	"errors"
	"fmt"
	"iptcp/pkg/iptcp_utils"
	"iptcp/pkg/node"
	"math/rand"
	"net/netip"

	"github.com/google/netstack/tcpip/header"
)

const (
	LISTEN = 1 << iota
	SYN_SENT
	SYN_RECEIVED
	ESTABLISHED
)

type TCPStack struct {
	socketTable map[node.HandshakeInfo]Socket
	ip          netip.Addr
	numSockets  int
}

type Socket interface {
	// VClose()
	printSocket()
}

type ListenSocket struct {
	localPort  uint16
	listenChan chan node.HandshakeInfo
}

type NormalSocket struct {
	SID    uint16
	buffer []byte
	state  uint8
	node.HandshakeInfo
	normalChan chan node.HandshakeInfo
}

func Initialize(n *node.Node) TCPStack {
	tcpStack := new(TCPStack)
	tcpStack.ip = n.IpAddr
	tcpStack.socketTable = make(map[node.HandshakeInfo]Socket)

	// handle matching process
	go func() {
		for {
			received := <-n.TCPChan

			// server and client flipped??
			if sock, ok := tcpStack.socketTable[node.HandshakeInfo{ClientAddr: received.ServerAddr, ClientPort: received.ServerPort, ServerAddr: received.ClientAddr, ServerPort: received.ClientPort}]; ok {
				sock.(*NormalSocket).normalChan <- received
			} else {
				// fmt.Println("2")
				listenTuple := node.HandshakeInfo{ClientPort: received.ServerPort}
				// fmt.Printf("created: %+v\n", listenTuple)
				if sock, ok := tcpStack.socketTable[listenTuple]; ok {
					// fmt.Println("3")

					sock.(*ListenSocket).listenChan <- received
				}
			}
		}
	}()
	return *tcpStack
}

func (t *TCPStack) VListen(port uint16) (*ListenSocket, error) {
	sock := new(node.HandshakeInfo)
	sock.ClientPort = port
	if _, ok := t.socketTable[*sock]; ok {
		// already listening on port
		return nil, fmt.Errorf("already listening on port %d", port)
	}
	lsock := &ListenSocket{localPort: port, listenChan: make(chan node.HandshakeInfo)}
	t.socketTable[*sock] = lsock
	return lsock, nil
}

func (t *TCPStack) VConnect(destAddr netip.Addr, destPort uint16, n *node.Node) (NormalSocket, error) {
	// generate new random port
	var randSrcPort uint16
	for {
		randSrcPort = uint16(rand.Intn(65535-20000) + 20000)
		sock := &node.HandshakeInfo{ClientPort: randSrcPort}
		if _, ok := t.socketTable[*sock]; !ok {
			break
		}
	}
	hs := &node.HandshakeInfo{ClientAddr: t.ip, ClientPort: randSrcPort, ServerAddr: destAddr, ServerPort: destPort}
	sock := &NormalSocket{
		SID:           uint16(t.numSockets),
		buffer:        make([]byte, 0),
		state:         SYN_SENT,
		HandshakeInfo: *hs,
		normalChan:    make(chan node.HandshakeInfo),
	}
	t.numSockets++
	t.socketTable[*hs] = sock

	tcpPacket := makeTCPPacket(t.ip, destAddr, nil, header.TCPFlagSyn, randSrcPort, destPort)
	// TODO: timeout stuff
	i := 0
	for {
		n.HandleSend(destAddr, tcpPacket, 6)
		res := <-sock.normalChan // wait until protocol6 confirms
		synack := uint8(header.TCPFlagSyn | header.TCPFlagAck)
		if res.Flag == synack {
			break
		}
		i++
		if i == 3 {
			return NormalSocket{}, errors.New("could not connect")
		}
	}

	t.socketTable[*hs].(*NormalSocket).state = ESTABLISHED
	tcpPacket = makeTCPPacket(t.ip, destAddr, nil, header.TCPFlagAck, randSrcPort, destPort)
	// establish connection
	n.HandleSend(destAddr, tcpPacket, 6)

	return *sock, nil
}

func (lsock *ListenSocket) VAccept(t *TCPStack, n *node.Node) (*NormalSocket, error) {
	// wait for SYN
	var hs node.HandshakeInfo
	for !(hs.Flag == header.TCPFlagSyn && hs.ServerPort == lsock.localPort) {
		hs = <-lsock.listenChan
	}

	// create new normal socket
	newSocket := &NormalSocket{
		SID:           0, // TODO:
		buffer:        make([]byte, 0),
		state:         SYN_RECEIVED,
		HandshakeInfo: hs,
		normalChan:    make(chan node.HandshakeInfo),
	}
	t.socketTable[hs] = newSocket

	// send SYN+ACK back to client
	synack := uint8(header.TCPFlagSyn | header.TCPFlagAck)
	tcpPacket := makeTCPPacket(hs.ServerAddr, hs.ClientAddr, nil, synack, hs.ServerPort, hs.ClientPort)
	// tcpPacket := makeTCPPacket(hs.ClientAddr, hs.ServerAddr, nil, synack, hs.ClientPort, hs.ServerPort)

	n.HandleSend(hs.ClientAddr, tcpPacket, 6)

	// wait for final packet to establish TCP
	for !(hs.Flag == header.TCPFlagAck && hs.ServerPort == lsock.localPort) {
		hs = <-lsock.listenChan
	}
	newSocket.state = ESTABLISHED

	return newSocket, nil
}

func makeTCPPacket(sourceIp netip.Addr, destIp netip.Addr,
	payload []byte, flags uint8, sourcePort uint16, destPort uint16) []byte {

	tcpHdr := header.TCPFields{
		SrcPort:       sourcePort,
		DstPort:       destPort,
		SeqNum:        1,
		AckNum:        1, // TODO: need to be random i think
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
	fmt.Printf("  %d    %s%-6d    %s     %d     ESTABLISHED\n", socket.SID,
		socket.ClientAddr, socket.ClientPort, socket.ServerAddr, socket.ServerPort) // TODO: what other statuses are possible
}

func (socket *ListenSocket) printSocket() {
	fmt.Printf("  0    0.0.0.0  %-6d  0.0.0.0     0       LISTEN\n", socket.localPort)
}

func (t *TCPStack) PrintTable() {
	fmt.Println("SID      LAddr LPort      RAddr RPort       Status")
	for _, socket := range t.socketTable {
		socket.printSocket()
	}
}
