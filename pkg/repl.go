package repl

import (
	"bufio"
	"fmt"
	"iptcp/pkg/node"
	"iptcp/pkg/tcpstack"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

func listenAndAccept(port uint16, t *tcpstack.TCPStack, n *node.Node) error {
	lsocket, err := t.VListen(port)
	if err != nil {
		return err
	}
	for {
		_, err := lsocket.VAccept(t, n)
		if err != nil {
			return err
		}

	}
}

func REPL(node *node.Node, t *tcpstack.TCPStack) {
	reader := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for reader.Scan() {
		command := cleanInput(reader.Text())

		tokens := strings.Split(command, " ")
		switch tokens[0] {
		case "li":
			node.PrintInterfaces()
		case "ln":
			node.PrintNeighbors()
		case "lr":
			node.PrintRoutes()
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
				fmt.Println("send usage: send <vip> <message ...>")
			} else {
				parsed_addr := netip.MustParseAddr(tokens[1])
				_, _, found := node.FindNext(parsed_addr) // where to forward to; our "source"
				if !found {
					fmt.Println("Error: No match in forwarding table.")
				} else {
					node.HandleSend(parsed_addr, []byte(strings.Join(tokens[2:], " ")), 0)
				}
			}
		case "a":
			if len(tokens) != 2 {
				fmt.Println("a usage: a <port>")
			} else {
				port, err := strconv.ParseUint(tokens[1], 10, 16)
				if err != nil {
					fmt.Println("Could not parse port as int")
				}
				go listenAndAccept(uint16(port), t, node)
			}
		case "c":
			if len(tokens) != 3 {
				fmt.Println("c usage: c <vip> <port>")
			} else {
				port, err := strconv.ParseUint(tokens[2], 10, 16)
				if err != nil {
					fmt.Println("Could not parse port as int")
					continue
				}
				addr := netip.MustParseAddr(tokens[1])
				if err != nil {
					fmt.Println("Could not parse address")
					continue
				}
				t.VConnect(addr, uint16(port), node)
			}
		case "ls":
			t.PrintTable()
		case "s":
			fmt.Println("to do")
		case "r":
			fmt.Println("to do")
		case "cl":
			fmt.Println("to do")
		case "sf":
			fmt.Println("to do")
		case "rf":
			fmt.Println("to do")
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
