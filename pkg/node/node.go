package node

import (
	"encoding/binary"
	"fmt"
	ipv4header "iptcp/pkg/iptcp-headers"
	"iptcp/pkg/iptcp_utils"
	"iptcp/pkg/lnxconfig"
	rip "iptcp/pkg/rip"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	header "github.com/google/netstack/tcpip/header"
)

type ForwardingInfo struct {
	route_type   string
	next         netip.Addr
	pre          netip.Prefix
	cost         int
	last_updated time.Time
	ifname       string

	forwardLock sync.Mutex
}

type sendInterface struct {
	pack    Packet
	address netip.Addr
}

type Node struct {
	neighbors        []lnxconfig.NeighborConfig
	interfaces       map[string]lnxconfig.InterfaceConfig
	interfaceSockets map[string]chan *sendInterface
	neighborTable    map[netip.Addr]lnxconfig.NeighborConfig
	forwardingTable  map[netip.Prefix]*ForwardingInfo
	handlerTable     map[int]HandlerFunc
	ripNeighbors     []netip.Addr
	nodeLock         sync.Mutex
	enableMtxs       map[string]*sync.Mutex
	enableConds      map[string]*sync.Cond
	enabled          map[string]bool
	TCPChan          chan HandshakeInfo
	IpAddr           netip.Addr
}

type HandshakeInfo struct {
	ClientAddr netip.Addr
	ClientPort uint16
	ServerAddr netip.Addr
	ServerPort uint16
	Flag       uint8
}

type Packet []byte

type HandlerFunc func(Packet, *ipv4header.IPv4Header)

const (
	MaxMessageSize = 1400
	INFTY          = 16
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
	node.interfaces = make(map[string]lnxconfig.InterfaceConfig)
	node.interfaceSockets = make(map[string]chan *sendInterface)
	node.forwardingTable = make(map[netip.Prefix]*ForwardingInfo)
	node.TCPChan = make(chan HandshakeInfo)

	node.neighbors = lnxConfig.Neighbors
	for pre, addr := range lnxConfig.StaticRoutes {
		info := &ForwardingInfo{
			route_type: "S",
			pre:        pre,
			next:       addr,
			cost:       0,
		}
		node.forwardingTable[pre] = info
	}
	node.createNeighborTable(lnxConfig.Neighbors)

	for _, iface := range lnxConfig.Interfaces {
		node.interfaces[iface.Name] = iface
		info := &ForwardingInfo{
			route_type: "L",
			next:       iface.AssignedIP,
			pre:        iface.AssignedPrefix,
			cost:       0,
			ifname:     iface.Name,
		}

		node.forwardingTable[info.pre] = info
		sendChan := make(chan *sendInterface, 1)
		node.interfaceSockets[iface.Name] = sendChan
		go node.interfaceRoutine(iface)
	}
	// send initial rip requests
	for _, rip_neighbor := range lnxConfig.RipNeighbors {
		node.ripNeighbors = append(node.ripNeighbors, rip_neighbor)
		iface := node.neighborTable[rip_neighbor].InterfaceName
		request_packet := &rip.RipPacket{
			Command:     1,
			Num_entries: 0,
		}
		marshalled, err := rip.MarshalRIP(request_packet)
		if err != nil {
			fmt.Printf("Error marshalling request RIP: %s\n", err)
			continue
		}
		inter := node.interfaces[iface]
		packet := makePacket(inter.AssignedIP, rip_neighbor, 200, 16, marshalled)
		toSend := &sendInterface{
			pack:    packet,
			address: rip_neighbor,
		}
		node.interfaceSockets[iface] <- toSend
	}
	// should add itself
	node.IpAddr = node.interfaces["if0"].AssignedIP

	node.handlerTable[0] = node.protocol0
	node.handlerTable[6] = node.protocol6
	node.handlerTable[200] = node.protocol200

	go node.periodicUpdates()

	go node.garbageCollector()

	return
}

func (node *Node) protocol0(message Packet, header *ipv4header.IPv4Header) {
	fmt.Printf("Received test packet:  Src: %s, Dst: %s, TTL: %d, Data: %s\n", header.Src.String(), header.Dst.String(), header.TTL, string(message))
}

// TCP protocol
func (node *Node) protocol6(message Packet, hdr *ipv4header.IPv4Header) {
	tcpHeaderAndData := message[:hdr.TotalLen]
	tcpHdr := iptcp_utils.ParseTCPHeader(tcpHeaderAndData)
	// tcpPayload := tcpHeaderAndData[tcpHdr.DataOffset:]
	// tcpChecksumFromHeader := tcpHdr.Checksum // Save original
	tcpHdr.Checksum = 0
	// tcpComputedChecksum := iptcp_utils.ComputeTCPChecksum(&tcpHdr, hdr.Src, hdr.Dst, tcpPayload)

	// var tcpChecksumState string
	// if tcpComputedChecksum == tcpChecksumFromHeader {
	// 	tcpChecksumState = "OK"
	// } else {
	// 	tcpChecksumState = "FAIL"
	// }
	// Finally, print everything out
	// fmt.Printf("Received TCP packet from %s\nIP Header:  %v\nIP Checksum:  %s\nTCP header:  %+v\nFlags:  %s\nTCP Checksum:  %s\nPayload (%d bytes):  %s\n",
	// "0.0.0.0", hdr, "OK", tcpHdr, iptcp_utils.TCPFlagsAsString(tcpHdr.Flags), tcpChecksumState, len(tcpPayload), string(tcpPayload))
	node.TCPChan <- HandshakeInfo{ClientAddr: hdr.Src, ClientPort: tcpHdr.SrcPort, ServerAddr: node.IpAddr, ServerPort: tcpHdr.DstPort, Flag: tcpHdr.Flags}
}

func popcount(num uint32) int {
	res := 0
	for num > 0 {
		res += int(num % 2)
		num /= 2
	}
	return res
}

// impl split horizon
func (node *Node) sendRIP(dst netip.Addr, subset map[netip.Prefix]*ForwardingInfo) {
	entries := make([]*rip.RipUpdate, 0)
	for pre, info := range subset { // send cost infinity
		addy_bytes := pre.Addr().As4()
		addy_int := (uint32(addy_bytes[0]) << 24) + (uint32(addy_bytes[1]) << 16) + (uint32(addy_bytes[2]) << 8) + uint32(addy_bytes[3])
		entry := &rip.RipUpdate{
			Cost:    uint32(info.cost),
			Address: addy_int,
			Mask:    binary.BigEndian.Uint32(net.CIDRMask(info.pre.Bits(), 32)),
		}

		if dst == info.next { // split horizon/poison
			entry.Cost = uint32(INFTY)
		}
		entries = append(entries, entry)
	}
	ripPacket := &rip.RipPacket{
		Command:     2,
		Num_entries: uint16(len(entries)),
		Entries:     entries,
	}
	marshalled, err := rip.MarshalRIP(ripPacket)
	if err != nil {
		fmt.Printf("Error marshalling RIP response packet: %s\n", err)
		return
	}
	inter := node.neighborTable[dst].InterfaceName
	src := node.interfaces[inter].AssignedIP
	packet := makePacket(src, dst, 200, 16, marshalled)

	toSend := &sendInterface{
		pack:    packet,
		address: dst,
	}
	node.interfaceSockets[inter] <- toSend

}

func (node *Node) protocol200(message Packet, header *ipv4header.IPv4Header) {
	// handle received rip packets (aka update forwarding table)
	rip_packet := rip.ExtractRIP(message)
	//fmt.Printf("command: %d source: %s\n", rip_packet.Command, header.Src.String())

	if rip_packet.Command == 1 { // request, send in response
		defined := make(map[netip.Prefix]*ForwardingInfo)
		node.nodeLock.Lock()
		for pre, info := range node.forwardingTable {
			defined[pre] = info
		}
		node.nodeLock.Unlock()
		node.sendRIP(header.Src, defined)
	} else { // update table
		triggered := make(map[netip.Prefix]*ForwardingInfo)

		for _, entry := range rip_packet.Entries {
			mask := popcount(entry.Mask)
			addy := int(entry.Address & entry.Mask)

			pre_string := fmt.Sprintf("%d.%d.%d.%d/%d", (addy>>24)&0xFF, (addy>>16)&0xFF, (addy>>8)&0xFF, addy&0xFF, mask)
			pre, err := netip.ParsePrefix(pre_string)
			if err != nil {
				fmt.Println("Error parsing mask in RIP entry")
				continue
			}
			hop := header.Src
			cost := min(int(entry.Cost+1), INFTY)
			node.nodeLock.Lock()
			info, ok := node.forwardingTable[pre]
			if !ok && cost != INFTY {
				val := &ForwardingInfo{
					route_type:   "R",
					next:         hop,
					pre:          pre,
					cost:         cost,
					last_updated: time.Now(),
				}
				node.forwardingTable[pre] = val
				triggered[pre] = val
			} else if ok {
				info.forwardLock.Lock()
				if info.cost > cost {
					info.cost = cost
					info.next = hop
					info.last_updated = time.Now()
					triggered[pre] = info
				} else if info.cost < cost {
					if hop == info.next {
						// fmt.Printf("%d, %d\n", info.cost, cost)
						info.cost = cost
						info.last_updated = time.Now()
						triggered[pre] = info
						// delete
						delete(node.forwardingTable, pre)
					}
				} else {
					if hop == info.next {
						info.last_updated = time.Now()
						triggered[pre] = info
					}
				}
				info.forwardLock.Unlock()
			}
			node.nodeLock.Unlock()
		}
		// send to all route neighbors
		if len(triggered) > 0 {
			for _, rip_neighbor := range node.ripNeighbors {
				node.sendRIP(rip_neighbor, triggered)
			}
		}
	}
}

func (node *Node) interfaceRoutine(iface lnxconfig.InterfaceConfig) {
	enableMutex := sync.Mutex{}
	enableCond := sync.NewCond(&enableMutex)
	node.enableMtxs[iface.Name] = &enableMutex
	node.enableConds[iface.Name] = enableCond
	node.enabled[iface.Name] = true

	listenAddr, err := net.ResolveUDPAddr("udp4", iface.UDPAddr.String())
	if err != nil {
		log.Panicln("Error resolving address:  ", err)
	}
	conn, err := net.ListenUDP("udp4", listenAddr)
	if err != nil {
		log.Panicln("Could not bind to UDP port: ", err)
	}

	go func() { // handle sends
		for {
			received := <-node.interfaceSockets[iface.Name]
			node.enableMtxs[iface.Name].Lock()
			if node.enabled[iface.Name] {
				node.forwardPacket(conn, received.address, received.pack)
			}
			node.enableMtxs[iface.Name].Unlock()
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

		go node.handlePackets(buffer, conn)
	}
}

func (node *Node) handlePackets(buffer []byte, conn *net.UDPConn) {
	header, err := ipv4header.ParseHeader(buffer)
	if err != nil {
		fmt.Println("Error parsing header", err)
		return
	}
	header.TTL--

	// this should never happen, but just in case
	if header.TTL < 0 {
		return
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
			next_addr, t, found := node.FindNext(header.Dst)
			if !found {
				fmt.Println("Error: No match in forwarding table.")
			} else {
				if t == "L" { // if the route type we find in forwarding table is local, means that dst is neighbor
					next_addr = header.Dst
				}
				data := makePacket(header.Src, header.Dst, protocolNum, header.TTL, buffer[headerSize:])
				node.forwardPacket(conn, next_addr, data)
			}
		}
	} else {
		fmt.Printf("Dropped packet\n")
	}
}

func (node *Node) periodicUpdates() {
	for {
		time.Sleep(5000 * time.Millisecond)
		defined := make(map[netip.Prefix]*ForwardingInfo)
		node.nodeLock.Lock()
		for pre, info := range node.forwardingTable {
			defined[pre] = info
		}
		node.nodeLock.Unlock()
		for _, rip_neighbor := range node.ripNeighbors {
			node.sendRIP(rip_neighbor, defined)
		}
	}
}

func (node *Node) garbageCollector() {
	for {
		time.Sleep(7500 * time.Millisecond)
		triggered := make(map[netip.Prefix]*ForwardingInfo)
		node.nodeLock.Lock()
		currTime := time.Now()
		// iterate through and get rid of all expired
		for pre, info := range node.forwardingTable {
			if info.route_type == "R" {
				if currTime.Sub(info.last_updated).Seconds() >= 12 {
					// expired
					info.cost = 16
					triggered[pre] = info
					delete(node.forwardingTable, pre)
				}
			}
		}
		node.nodeLock.Unlock()
		if len(triggered) > 0 {
			for _, rip_neighbor := range node.ripNeighbors {
				node.sendRIP(rip_neighbor, triggered)
			}
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
		_, err := conn.WriteToUDP(p, remoteAddr)
		if err != nil {
			fmt.Println("Error writing to socket")
		}
		//fmt.Printf("Sent %d bytes\n", bytesWritten)
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
	var toUse lnxconfig.InterfaceConfig
	len := -1
	for _, iface := range node.interfaces {
		prefix := iface.AssignedPrefix
		if prefix.Contains(dst) {
			if prefix.Bits() > len {
				len = prefix.Bits()
				toUse = iface
			}
		}
	}
	if len == -1 {
		return false
	}
	if dst == toUse.AssignedIP {
		return true
	}
	return false
}

// return where to send packet next, error = not found in forwarding table
func (node *Node) FindNext(dst netip.Addr) (netip.Addr, string, bool) {
	// first, check forwrarding table: if not there then err
	// if it is, get next hop (depending on type)
	// after this, check neighbor table / interface tables as needed

	// first check local network, then go past if needed
	if node.checkDest(dst) {
		return dst, "L", true
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
		return dummy, "hello", false
	} else {
		return useForward.next, useForward.route_type, true
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
func (node *Node) HandleSend(dst netip.Addr, data []byte, protocolNum int) {
	// we want to find proper source (interface)
	// FindNext will give us if local or outside
	// if outside (aka router), send to router with source of interface in neighbortable
	// else: if local, we kept local.
	// then use neighborUDP to check validity
	// if valid, then iterate neighbor list to get interface
	next_addr, t, found := node.FindNext(dst)
	if !found {
		fmt.Println("Error: No match in forwarding table.")
	} else {
		if t == "L" {
			next_addr = dst
		}
		// check neighbor
		_, isNeighbor := node.neighborUDP(next_addr)
		if isNeighbor {
			// then send from corersponding interface; find this
			neighbor := node.neighborTable[next_addr]
			iface := neighbor.InterfaceName
			for _, inter := range node.interfaces {
				if inter.Name == iface {
					src := inter.AssignedIP
					p := makePacket(src, dst, protocolNum, 16, data)
					toSend := &sendInterface{
						pack:    p,
						address: next_addr,
					}
					// fmt.Printf("Sent %d bytes\n", 20+len(data))
					node.interfaceSockets[iface] <- toSend
				}
			}

		} else {
			fmt.Println("Error: VIP is not valid dest")
		}
	}
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
		return fmt.Errorf("%s not a valid interface", name)
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

func (node *Node) PrintInterfaces() {
	fmt.Println("Name  Addr/Prefix State")
	for _, iface := range node.interfaces {
		state := "down"
		if node.enabled[iface.Name] {
			state = "up"
		}
		fmt.Printf("%4s  %s/%d %4s\n", iface.Name, iface.AssignedIP.String(), iface.AssignedPrefix.Bits(), state)
	}
}

func (node *Node) PrintNeighbors() {
	fmt.Println("Iface          VIP          UDPAddr")
	for _, neighbor := range node.neighbors {
		fmt.Printf("%4s    %9s  %15s\n", neighbor.InterfaceName, neighbor.DestAddr, neighbor.UDPAddr)
	}
}

func (node *Node) PrintRoutes() {
	fmt.Println("T       Prefix   Next hop   Cost")
	for _, info := range node.forwardingTable {
		pre := info.pre
		if info.route_type == "L" {
			fmt.Printf("%s  %11s   LOCAL:%-5s   0\n", info.route_type, pre, info.ifname)
		} else if info.route_type == "R" {
			fmt.Printf("%s  %11s   %-11s  %2d\n", info.route_type, pre, info.next, info.cost)
		} else {
			fmt.Printf("%s  %11s   %-11s   -\n", info.route_type, pre, info.next)
		}
	}
}
