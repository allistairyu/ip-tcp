package main

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"sync"

	ipv4header "iptcp/pkg/iptcp-headers"
	"iptcp/pkg/lnxconfig"

	"github.com/google/netstack/tcpip/header"
)

type Node struct {
	addr            netip.Addr
	neighbors       []lnxconfig.NeighborConfig
	interfaces      []lnxconfig.InterfaceConfig
	forwardingTable map[netip.Addr]lnxconfig.NeighborConfig
	handlerTable    map[uint8]HandlerFunc

	// TODO: is there a less stupid way to do this...
	enableMtxs  map[string]*sync.Mutex
	enableConds map[string]*sync.Cond
	enabled     map[string]bool
}

type Packet []byte

type HandlerFunc func(*Packet)

const (
	MaxMessageSize = 1400
)

/*
 * Initialize neighbors, table, etc.
 */
func Initialize(lnxConfig *lnxconfig.IPConfig) (node *Node, err error) {
	node = new(Node)
	for _, iface := range lnxConfig.Interfaces {
		node.interfaces = append(node.interfaces, iface)
		go node.interfaceRoutine(iface)
	}
	for _, neighbor := range lnxConfig.Neighbors {
		node.neighbors = append(node.neighbors, neighbor)
	}
	node.createForwardingTable(lnxConfig.Neighbors)
	node.handlerTable = make(map[uint8]HandlerFunc)
	node.enableMtxs = make(map[string]*sync.Mutex)
	node.enableConds = make(map[string]*sync.Cond)
	node.enabled = make(map[string]bool)
	return
}

func (node *Node) interfaceRoutine(iface lnxconfig.InterfaceConfig) {
	enableMutex := sync.Mutex{}
	enableCond := sync.NewCond(&enableMutex)
	node.enableMtxs[iface.Name] = &enableMutex
	node.enableConds[iface.Name] = enableCond
	node.enabled[iface.Name] = true

	listenString := fmt.Sprintf(":%s", iface.UDPAddr)
	listenAddr, err := net.ResolveUDPAddr("udp4", listenString)
	if err != nil {
		log.Panicln("Error resolving address:  ", err)
	}
	conn, err := net.ListenUDP("udp4", listenAddr)
	if err != nil {
		log.Panicln("Could not bind to UDP port: ", err)
	}

OUTER:
	for {
		buffer := make([]byte, MaxMessageSize) // max IP packet size of 1400 bytes
		bytesRead, sourceAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			log.Panicln("Error reading from UDP socket ", err)
		}

		// Check if interface is disabled
		enableMutex.Lock()
		for !node.enabled[iface.Name] {
			enableCond.Wait()
			if node.enabled[iface.Name] {
				enableMutex.Unlock()
				continue OUTER // https://relistan.com/continue-statement-with-labels-in-go
			}
		}
		enableMutex.Unlock()

		// https://github.com/brown-csci1680/lecture-examples/blob/main/ip-demo/cmd/udp-ip-recv/main.go#L107
		header, err := ipv4header.ParseHeader(buffer)
		if err != nil {
			fmt.Println("Error parsing header", err)
			continue
		}
		// TODO: order of decrementing TTL and checksum?
		header.TTL--
		if header.TTL <= 0 {
			continue
		}

		headerSize := header.Len
		headerBytes := buffer[:headerSize]
		checksumFromHeader := uint16(header.Checksum)
		computedChecksum := ValidateChecksum(headerBytes, checksumFromHeader)
		if computedChecksum == checksumFromHeader {
			if header.Dst == node.addr {
				message := buffer[headerSize:]
				os.Stdout.Write(message)
			} else {
				// longest prefix match in forwarding table
				// TODO:
				prefix := iface.AssignedPrefix
			}
		}
	}
}

func (node *Node) createForwardingTable(neighbors []lnxconfig.NeighborConfig) {
	forwardingTable := make(map[netip.Addr]lnxconfig.NeighborConfig)
	for _, neighbor := range neighbors {
		forwardingTable[neighbor.DestAddr] = neighbor
	}
	node.forwardingTable = forwardingTable
}

func (node *Node) SendIP(dst netip.Addr, protocolNum uint8, data []byte) (err error) {
	// TODO: create IPV4 header
	// marshall stuff?

	// send via UDP to dst
	conn, err := net.Dial("udp4", "127.0.0.1:6666")
	if err != nil {
		panic(err)
	}

	_, err = conn.Write([]byte("Hello world!"))
	if err != nil {
		panic(err)
	}
	return
}

func (node *Node) RegisterHandler(protocolNum uint8, callback HandlerFunc) {
	node.handlerTable[protocolNum] = callback
}

func (node *Node) GetNeighborList() []lnxconfig.NeighborConfig {
	return node.neighbors
}

func (node *Node) EnableInterface(name string) {
	node.enableMtxs[name].Lock()
	node.enabled[name] = true
	node.enableConds[name].Broadcast()
	node.enableMtxs[name].Unlock()
}

func (node *Node) DisableInterface(name string) {
	node.enableMtxs[name].Lock()
	node.enabled[name] = false
	node.enableConds[name].Broadcast()
	node.enableMtxs[name].Unlock()
}

func ValidateChecksum(b []byte, fromHeader uint16) uint16 {
	checksum := header.Checksum(b, fromHeader)

	return checksum
}
