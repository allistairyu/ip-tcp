package main

import (
	"fmt"
	"log"
	"net"
	"net/netip"

	ipv4header "github.com/brown-cs1680-f23/iptcp-luke-allistair/pkg/iptcp-headers"
	"github.com/brown-cs1680-f23/iptcp-luke-allistair/pkg/lnxconfig"
)

type Node struct {
	addr       netip.Addr
	neighbors  []lnxconfig.NeighborConfig
	interfaces []lnxconfig.InterfaceConfig
}

/*
 * Read LNX file and initialize neighbors, table, etc.
 */
func Initialize(filePath string) (node *Node, err error) {
	// Parse the file
	lnxConfig, err := lnxconfig.ParseConfig(filePath)
	if err != nil {
		panic(err)
	}

	for _, iface := range lnxConfig.Interfaces {
		prefixForm := netip.PrefixFrom(iface.AssignedIP, iface.AssignedPrefix.Bits())
		fmt.Printf("%s has IP %s\n", iface.Name, prefixForm.String())
		// CreateInterface()
	}
	// CreateForwardingTable(lnxConfig.Neighbors)

	return
}

// type InterfaceConfig struct {
// 	Name           string
// 	AssignedIP     netip.Addr
// 	AssignedPrefix netip.Prefix

//		UDPAddr netip.AddrPort
//	}
func interfaceRoutine(iface lnxconfig.InterfaceConfig) {
	listenString := fmt.Sprintf(":%s", iface.UDPAddr) // TODO: tf is listenstring
	listenAddr, err := net.ResolveUDPAddr("udp4", listenString)
	if err != nil {
		log.Panicln("Error resolving address:  ", err)
	}
	conn, err := net.ListenUDP("udp4", listenAddr)
	if err != nil {
		log.Panicln("Could not bind to UDP port: ", err)
	}
	for {
		buffer := make([]byte, 100) // TODO: max IP packet size?
		bytesRead, sourceAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			log.Panicln("Error reading from UDP socket ", err)
		}
		// TODO: IP in UDP example
		header, err := ipv4header.ParseHeader(buffer)
		// TODO: parse packet: check sum

		// TODO: check if current host is destination
		if header.Dst == nil {
			// print message
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

func CreateForwardingTable(neighbors []lnxconfig.NeighborConfig) map[netip.Addr]lnxconfig.NeighborConfig {
	forwardingTable := make(map[netip.Addr]lnxconfig.NeighborConfig)
	for _, neighbor := range neighbors {
		forwardingTable[neighbor.DestAddr] = neighbor
	}
	return forwardingTable
}

func GetNeighborList() {

}

func EnableInterface() {

}

func DisableInterface() {

}
