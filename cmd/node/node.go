package main

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"

	ipv4header "github.com/brown-cs1680-f23/iptcp-luke-allistair/pkg/iptcp-headers"
	"github.com/brown-cs1680-f23/iptcp-luke-allistair/pkg/lnxconfig"
)

type Node struct {
	addr            netip.Addr
	neighbors       []lnxconfig.NeighborConfig
	interfaces      []lnxconfig.InterfaceConfig
	forwardingTable map[netip.Addr]lnxconfig.NeighborConfig
	enableChan      chan bool
}

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
	node.enableChan = make(chan bool)
	node.CreateForwardingTable(lnxConfig.Neighbors)
	return
}

func (node *Node) interfaceRoutine(iface lnxconfig.InterfaceConfig) {
	defer close(node.enableChan)
	listenString := fmt.Sprintf(":%s", iface.UDPAddr) // TODO: tf is listenstring
	listenAddr, err := net.ResolveUDPAddr("udp4", listenString)
	if err != nil {
		log.Panicln("Error resolving address:  ", err)
	}
	conn, err := net.ListenUDP("udp4", listenAddr)
	if err != nil {
		log.Panicln("Could not bind to UDP port: ", err)
	}

	enabled := true
OUTER: // https://relistan.com/continue-statement-with-labels-in-go
	for {
		buffer := make([]byte, 1400) // max IP packet size of 1400 bytes
		bytesRead, sourceAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			log.Panicln("Error reading from UDP socket ", err)
		}

		for !enabled {
			enabled = <-node.enableChan
			if enabled {
				continue OUTER // start reading from UDP socket again
			}
		}

		// TODO: IP in UDP example
		header, err := ipv4header.ParseHeader(buffer)
		// TODO: parse packet: check sum

		if header.Dst == node.addr {
			os.Stdout.Write(buffer)
		} else {
			header.TTL--
			if header.TTL <= 0 {
				// drop
			} else {
				// longest prefix match in forwarding table
			}
		}
	}
}

func (node *Node) CreateForwardingTable(neighbors []lnxconfig.NeighborConfig) {
	forwardingTable := make(map[netip.Addr]lnxconfig.NeighborConfig)
	for _, neighbor := range neighbors {
		forwardingTable[neighbor.DestAddr] = neighbor
	}
	node.forwardingTable = forwardingTable
}

func (node *Node) GetNeighborList() []lnxconfig.NeighborConfig {
	return node.neighbors
}

func (node *Node) EnableInterface() {
	node.enableChan <- true
}

func (node *Node) DisableInterface() {
	node.enableChan <- false
}
