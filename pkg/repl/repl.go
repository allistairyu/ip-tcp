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
		normalSock, err := lsocket.VAccept(t, n)
		if err != nil {
			return err
		}
		go normalSock.ReceiverThread(n)
		go normalSock.SenderThread(n)
	}
}

func REPL(n *node.Node, t *tcpstack.TCPStack) {
	reader := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for reader.Scan() {
		command := cleanInput(reader.Text())

		tokens := strings.Split(command, " ")
		switch tokens[0] {
		case "li":
			n.PrintInterfaces()
		case "ln":
			n.PrintNeighbors()
		case "lr":
			n.PrintRoutes()
		case "down":
			if len(tokens) != 2 {
				fmt.Println("down usage: down <ifname>")
			} else {
				err := n.ToggleInterface(tokens[1], false)
				if err != nil {
					fmt.Println("Invalid interface")
				}
			}
		case "up":
			if len(tokens) != 2 {
				fmt.Println("up usage: up <ifname>")
			} else {
				err := n.ToggleInterface(tokens[1], true)
				if err != nil {
					fmt.Println("Invalid interface")
				}
			}
		case "send":
			if len(tokens) < 3 {
				fmt.Println("send usage: send <vip> <message ...>")
			} else {
				parsed_addr := netip.MustParseAddr(tokens[1])
				_, _, found := n.FindNext(parsed_addr) // where to forward to; our "source"
				if !found {
					fmt.Println("Error: No match in forwarding table.")
				} else {
					n.HandleSend(parsed_addr, []byte(strings.Join(tokens[2:], " ")), 0)
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
				go listenAndAccept(uint16(port), t, n)
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
				norm, err := t.VConnect(addr, uint16(port), n)
				if err != nil {
					fmt.Printf("Error: %s\n", err)
					continue
				}
				go norm.SenderThread(n)
				go norm.ReceiverThread(n)
			}
		case "ls":
			t.PrintTable()
		case "s":
			if len(tokens) < 3 {
				fmt.Println("s usage: s <socketID> <bytes>")
				continue
			}
			sock, err := strconv.ParseUint(tokens[1], 10, 16)
			if err != nil {
				fmt.Println("Could not parse socket ID as int")
				continue
			}
			// check socket ID is valid
			sk, ok := t.SID_to_sk[uint16(sock)]
			if !ok {
				fmt.Println("Could not find socket")
				continue
			}
			socket, ok := t.SocketTable[sk]
			if !ok {
				fmt.Println("Could not find socket")
				continue
			}
			message := strings.Join(tokens[2:], " ")
			socket.(*tcpstack.NormalSocket).VWrite([]byte(message))
		case "r":
			if len(tokens) != 3 {
				fmt.Println("r usage: r <socket ID> <numbytes>")
				continue
			}
			sock, err := strconv.ParseUint(tokens[1], 10, 16)
			if err != nil {
				fmt.Println("Could not parse socket ID as int")
				continue
			}
			num, err := strconv.ParseUint(tokens[2], 10, 16)
			if err != nil {
				fmt.Println("Could not parse numbytes as int")
				continue
			}
			// check socket ID is valid
			sk, ok := t.SID_to_sk[uint16(sock)]
			if !ok {
				fmt.Println("Could not find socket")
				continue
			}

			socket, ok := t.SocketTable[sk]
			if !ok {
				fmt.Println("Could not find socket")
				continue
			}
			socket.(*tcpstack.NormalSocket).VRead(uint16(num))

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