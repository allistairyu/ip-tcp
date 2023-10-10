package main

import (
	"fmt"
	"log"
	"net"
	"net/netip"

	"github.com/brown-cs1680-f23/iptcp-luke-allistair/pkg/lnxconfig"
)

type neighbor struct {
	ipAddr  int32  // TODO: ?
	udpAddr int32  // TODO: ?
	name    string // TODO ?
}

/*
 * Read LNX file and initialize neighbors, table, etc.
 */
func Initialize(filePath string) (err error) {
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
	return nil
}

func CreateInterface() {

	// go interfaceRoutine()
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
		// TODO: parse packet: check sum, decrement TTL

		// if this node is destination, print message
		// else use forwarding table to route packet
	}
}

func CreateForwardingTable() {

}

func GetNeighborList() {

}

func EnableInterface() {

}

func DisableInterface() {

}
