package main

import (
	"flag"
	"fmt"
	"iptcp/pkg/lnxconfig"
	"iptcp/pkg/node"
	repl "iptcp/pkg/repl"
	"iptcp/pkg/tcpstack"
	"log"
)

func main() {
	lnxPath := flag.String("config", "", "lnx config path")
	flag.Parse()

	if *lnxPath == "" {
		fmt.Println("vhost usage: --config <lnxConfigPath>")
		return
	}

	lnxConfig, err := lnxconfig.ParseConfig(*lnxPath)
	if err != nil {
		log.Fatal(err)
		return
	}
	host, err := node.Initialize(lnxConfig)
	if err != nil {

	}
	t := tcpstack.Initialize(host)
	repl.REPL(host, t)
}
