package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"

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
	file, err := os.Open(filePath)
	if err != nil {
		return errors.New("file open fail")
	}
	defer file.Close()

	// Parse the file
	lnxConfig, err := lnxconfig.ParseConfig(filePath)
	if err != nil {
		panic(err)
	}

	// Demo:  print out the IP for each interface in this config
	for _, iface := range lnxConfig.Interfaces {
		prefixForm := netip.PrefixFrom(iface.AssignedIP, iface.AssignedPrefix.Bits())
		fmt.Printf("%s has IP %s\n", iface.Name, prefixForm.String())
	}
	return nil
}

func CreateInterface() {

}

func CreateForwardingTable() {

}

func GetNeighborList() {

}

func EnableInterface() {

}

func DisableInterface() {

}
