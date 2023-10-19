package node

import (
	"bufio"
	"fmt"
	ipv4header "iptcp/pkg/iptcp-headers"
	"iptcp/pkg/lnxconfig"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/google/netstack/tcpip/header"
)

type ForwardingInfo struct {
	route_type   string
	next         netip.Addr
	pre          netip.Prefix
	cost         int
	last_updated int64
	ifname       string

	forwardLock sync.Mutex
}

type sendInterface struct {
	pack    Packet
	address netip.Addr
}

type Node struct {
	//addr            netip.Addr
	neighbors        []lnxconfig.NeighborConfig
	interfaces       []lnxconfig.InterfaceConfig
	interfaceSockets map[string]chan *sendInterface
	neighborTable    map[netip.Addr]lnxconfig.NeighborConfig
	forwardingTable  []*ForwardingInfo
	handlerTable     map[int]HandlerFunc

	// TODO: is there a less stupid way to do this...
	enableMtxs  map[string]*sync.Mutex
	enableConds map[string]*sync.Cond
	enabled     map[string]bool
}

type Packet []byte

type HandlerFunc func(Packet, *ipv4header.IPv4Header)

const (
	MaxMessageSize = 1400
)

/*
 * Initialize neighbors, table, etc.
 */
func Initialize(lnxConfig *lnxconfig.IPConfig) (node *Node, err error) {
	node = new(Node)
	node.handlerTable = make(map[int]HandlerFunc)
	node.enableMtxs = make(map[string]*sync.Mutex)
	node.enableConds = make(map[string]*sync.Cond)
	node.enabled = make(map[string]bool)
	node.interfaceSockets = make(map[string]chan *sendInterface)
	//node.forwardingTable = make([]*ForwardingInfo)

	// fill interfaces, forwarding table
	for _, iface := range lnxConfig.Interfaces {
		node.interfaces = append(node.interfaces, iface)
		info := &ForwardingInfo{
			route_type: "L",
			next:       iface.AssignedIP,
			pre:        iface.AssignedPrefix,
			cost:       0,
			ifname:     iface.Name,
		}
		node.forwardingTable = append(node.forwardingTable, info)
		sendChan := make(chan *sendInterface, 1)
		node.interfaceSockets[iface.Name] = sendChan
		go node.interfaceRoutine(iface)
	}
	node.neighbors = lnxConfig.Neighbors
	for pre, addr := range lnxConfig.StaticRoutes {
		info := &ForwardingInfo{
			route_type: "S",
			pre:        pre,
			next:       addr,
			cost:       0,
		}
		node.forwardingTable = append(node.forwardingTable, info)
	}
	node.createNeighborTable(lnxConfig.Neighbors)

	for _, rip_neighbor := range lnxConfig.RipNeighbors {
		iface := node.neighborTable[rip_neighbor].InterfaceName
		for _, inter := range node.interfaces {
			if inter.Name == iface {
				pre := inter.AssignedPrefix
				info := &ForwardingInfo{
					route_type: "R",
					next:       rip_neighbor,
					pre:        pre,
					cost:       -1,
				}
				node.forwardingTable = append(node.forwardingTable, info)
				continue
			}
		}
	}

	// figure out what to do with this later
	node.handlerTable[0] = protocol0
	node.handlerTable[200] = protocol200
	return
}

func protocol0(message Packet, header *ipv4header.IPv4Header) {
	fmt.Printf("Received test packet:  Src: %s, Dst: %s, TTL: %d, Data: %s\n", header.Src.String(), header.Dst.String(), header.TTL, string(message))
}

func protocol200(message Packet, header *ipv4header.IPv4Header) {
	// handle rip packets...

}

func (node *Node) interfaceRoutine(iface lnxconfig.InterfaceConfig) {
	enableMutex := sync.Mutex{}
	enableCond := sync.NewCond(&enableMutex)
	node.enableMtxs[iface.Name] = &enableMutex
	node.enableConds[iface.Name] = enableCond
	node.enabled[iface.Name] = true

	// listenString := fmt.Sprintf(":%s", iface.UDPAddr)
	listenAddr, err := net.ResolveUDPAddr("udp4", iface.UDPAddr.String())
	if err != nil {
		log.Panicln("Error resolving address:  ", err)
	}
	conn, err := net.ListenUDP("udp4", listenAddr)
	if err != nil {
		log.Panicln("Could not bind to UDP port: ", err)
	}

	go func() {
		// handle sends
		for {
			select {
			case received := <-node.interfaceSockets[iface.Name]:
				// want to send this
				node.enableMtxs[iface.Name].Lock()
				if node.enabled[iface.Name] {
					node.forwardPacket(conn, received.address, received.pack)
				}
				node.enableMtxs[iface.Name].Unlock()
			}
		}
	}()

OUTER:
	for {
		buffer := make([]byte, MaxMessageSize) // max IP packet size of 1400 bytes
		_, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			log.Panicln("Error reading from UDP socket ", err)
		}

		// Check if interface is disabled
		node.enableMtxs[iface.Name].Lock()
		for !node.enabled[iface.Name] {
			node.enableConds[iface.Name].Wait()
			if node.enabled[iface.Name] {
				node.enableMtxs[iface.Name].Unlock()
				continue OUTER // https://relistan.com/continue-statement-with-labels-in-go
			}
		}
		node.enableMtxs[iface.Name].Unlock()

		// https://github.com/brown-csci1680/lecture-examples/blob/main/ip-demo/cmd/udp-ip-recv/main.go#L107
		header, err := ipv4header.ParseHeader(buffer)
		if err != nil {
			fmt.Println("Error parsing header", err)
			continue
		}
		header.TTL--

		// this should never happen, but just in case
		if header.TTL < 0 {
			continue
		}

		headerSize := header.Len
		headerBytes := buffer[:headerSize]
		checksumFromHeader := uint16(header.Checksum)
		computedChecksum := ValidateChecksum(headerBytes, checksumFromHeader)
		protocolNum := header.Protocol
		if computedChecksum == checksumFromHeader {
			header.Checksum = int(computedChecksum)

			if node.checkDest(header.Dst) { // check that we end here
				node.handlerTable[protocolNum](buffer[headerSize:], header)
			} else if header.TTL > 0 {
				next_addr, found := node.findNext(header.Dst)
				if !found {
					fmt.Println("Error: No match in forwarding table.")
				} else {
					// send the packet to desired
					data := makePacket(header.Src, header.Dst, protocolNum, header.TTL, buffer[headerSize:])
					node.forwardPacket(conn, next_addr, data)
				}
			}
		} else {
			fmt.Printf("Dropped packet, message from %s\n", iface.Name)
		}
	}
}

// forward a given packet to nextAddr
func (node *Node) forwardPacket(conn *net.UDPConn, nextAddr netip.Addr, p Packet) {
	udpPort, found := node.neighborUDP(nextAddr)
	remoteAddr, err := net.ResolveUDPAddr("udp4", udpPort.String())
	if err != nil {
		fmt.Println("error resolving udp address")
	}
	if !found {
		fmt.Println("Destination VIP is not a known neighbor")
	} else {
		bytesWritten, err := conn.WriteToUDP(p, remoteAddr)
		if err != nil {
			fmt.Println("Error writing to socket")
		}
		fmt.Printf("Sent %d bytes\n", bytesWritten)
	}
}

// check if neighbor; if exists, return the udp port
func (node *Node) neighborUDP(dst netip.Addr) (netip.AddrPort, bool) {
	for _, neighbor := range node.neighbors {
		if neighbor.DestAddr == dst {
			return neighbor.UDPAddr, true
		}
	}
	dummy, _ := netip.ParseAddrPort("127.0.0.1:1680")
	return dummy, false
}

// check if destination corresponds to current node
func (node *Node) checkDest(dst netip.Addr) bool {
	for _, iface := range node.interfaces {
		prefix := iface.AssignedPrefix
		if prefix.Contains(dst) {
			return true
		}
	}
	return false
}

// return where to send packet next, error = not found in forwarding table
func (node *Node) findNext(dst netip.Addr) (netip.Addr, bool) {
	// first, check forwrarding table: if not there then err
	// if it is, get next hop (depending on type)
	// after this, check neighbor table / interface tables as needed

	// first check local network, then go past if needed
	if node.checkDest(dst) {
		return dst, true
	}
	len := -1
	var useForward *ForwardingInfo
	for _, forward := range node.forwardingTable {
		pre := forward.pre
		if pre.Contains(dst) {
			if pre.Bits() > len {
				len = pre.Bits()
				useForward = forward
			}
		}
	}
	if len == -1 {
		// didnt find
		dummy, _ := netip.ParseAddr("0.0.0.0")
		return dummy, false
	} else {
		return useForward.next, true
	}
}

// makes packet for given source, destination, protocol, TTL, data
func makePacket(src netip.Addr, dst netip.Addr, protocolNum int, ttl int, data []byte) Packet {
	hdr := ipv4header.IPv4Header{
		Version:  4,
		Len:      20, // Header length is always 20 when no IP options
		TOS:      0,
		TotalLen: ipv4header.HeaderLen + len(data),
		ID:       0,
		Flags:    0,
		FragOff:  0,
		TTL:      ttl,
		Protocol: protocolNum,
		Checksum: 0, // Should be 0 until checksum is computed
		Src:      netip.MustParseAddr(src.String()),
		Dst:      netip.MustParseAddr(dst.String()),
		Options:  []byte{},
	}

	// Assemble the header into a byte array
	headerBytes, err := hdr.Marshal()
	if err != nil {
		log.Fatalln("Error marshalling header:  ", err)
	}

	// Compute the checksum (see below)
	// Cast back to an int, which is what the Header structure expects
	hdr.Checksum = int(ComputeChecksum(headerBytes))

	headerBytes, err = hdr.Marshal()
	if err != nil {
		log.Fatalln("Error marshalling header:  ", err)
	}

	bytesToSend := make([]byte, 0, len(headerBytes)+len(data))
	bytesToSend = append(bytesToSend, headerBytes...)
	bytesToSend = append(bytesToSend, data...)

	return bytesToSend
}

// handle the send query in REPL
func (node *Node) handleSend(dst netip.Addr, data []byte) {
	// we want to find proper source (interface)
	// findNext will give us if local or outside
	// if outside (aka router), send to router with source of interface in neighbortable
	// else: if local, we kept local.
	// then use neighborUDP to check validity
	// if valid, then iterate neighbor list to get interface
	next_addr, found := node.findNext(dst)
	if !found {
		fmt.Println("Error: No match in forwarding table.")
	} else {
		// check neighbor
		_, isNeighbor := node.neighborUDP(next_addr)
		if isNeighbor {
			// then send from corersponding interface; find this
			neighbor := node.neighborTable[next_addr]
			iface := neighbor.InterfaceName
			for _, inter := range node.interfaces {
				if inter.Name == iface {
					src := inter.AssignedIP
					p := makePacket(src, dst, 0, 16, data)
					toSend := &sendInterface{
						pack:    p,
						address: next_addr,
					}
					node.interfaceSockets[iface] <- toSend
				}
			}

		} else {
			fmt.Println("Error: VIP is not valid dest")
		}
	}
	return
}

func (node *Node) RegisterHandler(protocolNum int, callback HandlerFunc) {
	node.handlerTable[protocolNum] = callback
}

// creates neighbor table
func (node *Node) createNeighborTable(neighbors []lnxconfig.NeighborConfig) {
	neighborTable := make(map[netip.Addr]lnxconfig.NeighborConfig)
	for _, neighbor := range neighbors {
		neighborTable[neighbor.DestAddr] = neighbor
	}
	node.neighborTable = neighborTable
}

func (node *Node) EnableInterface(name string) error {
	if _, ok := node.enabled[name]; !ok {
		return fmt.Errorf("%s not a valid interface\n", name)
	}
	node.enableMtxs[name].Lock()
	node.enabled[name] = true
	node.enableConds[name].Broadcast()
	node.enableMtxs[name].Unlock()
	return nil
}

func (node *Node) DisableInterface(name string) error {
	if _, ok := node.enabled[name]; !ok {
		return fmt.Errorf("%s not a valid interface", name)
	}
	node.enableMtxs[name].Lock()
	node.enabled[name] = false
	node.enableConds[name].Broadcast()
	node.enableMtxs[name].Unlock()
	return nil
}

func ValidateChecksum(b []byte, fromHeader uint16) uint16 {
	checksum := header.Checksum(b, fromHeader)
	return checksum
}

func ComputeChecksum(b []byte) uint16 {
	checksum := header.Checksum(b, 0)
	checksumInv := checksum ^ 0xffff
	return checksumInv
}

func (node *Node) REPL() {
	reader := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for reader.Scan() {
		command := cleanInput(reader.Text())

		tokens := strings.Split(command, " ")
		switch tokens[0] {
		case "li":
			node.printInterfaces()
		case "ln":
			node.printNeighbors()
		case "lr":
			node.printRoutes()
		case "down":
			if len(tokens) != 2 {
				fmt.Println("down usage: down <ifname>")
			} else {
				err := node.DisableInterface(tokens[1])
				if err != nil {
					fmt.Println("Invalid interface")
				}
			}
		case "up":
			if len(tokens) != 2 {
				fmt.Println("up usage: up <ifname>")
			} else {
				err := node.EnableInterface(tokens[1])
				if err != nil {
					fmt.Println("Invalid interface")
				}
			}
		case "send":
			if len(tokens) < 3 {
				fmt.Println("send usage: send <addr> <message ...>")
			} else {
				parsed_addr := netip.MustParseAddr(tokens[1])
				_, found := node.findNext(parsed_addr) // where to forward to; our "source"
				if !found {
					fmt.Println("Error: No match in forwarding table.")
				} else {
					node.handleSend(parsed_addr, []byte(strings.Join(tokens[2:], " ")))
				}
			}
		default:

		}
		fmt.Print("> ")
	}
}

func cleanInput(text string) string {
	output := strings.TrimSpace(text)
	output = strings.ToLower(output)
	return output
}

func (node *Node) printInterfaces() {
	fmt.Println("Name  Addr/Prefix State")
	for _, iface := range node.interfaces {
		state := "down"
		if node.enabled[iface.Name] {
			state = "up"
		}
		fmt.Printf("%4s  %s/%d %4s\n", iface.Name, iface.AssignedIP.String(), iface.AssignedPrefix.Bits(), state)
	}
}

func (node *Node) printNeighbors() {
	fmt.Println("Iface          VIP          UDPAddr")
	for _, neighbor := range node.neighbors {
		fmt.Printf("%4s    %9s  %15s\n", neighbor.InterfaceName, neighbor.DestAddr, neighbor.UDPAddr)
	}
}

func (node *Node) printRoutes() {
	fmt.Println("T       Prefix     Next hop    Cost")
	for _, info := range node.forwardingTable {
		pre := info.pre
		if info.route_type == "L" {
			fmt.Printf("%s       %s     LOCAL:%s    %d\n", info.route_type, pre, info.ifname, info.cost)
		} else if info.route_type == "R" {
			if info.cost != -1 {
				fmt.Printf("%s       %s     %s    %d\n", info.route_type, pre, info.next, info.cost)
			}
		} else {
			fmt.Printf("%s       %s     %s    %d\n", info.route_type, pre, info.next, info.cost)
		}
	}
}
