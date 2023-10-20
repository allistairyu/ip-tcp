**Structure of Node**
- Store neighbors, interfaces, routing neighbors in the typical way
- Created neighborTable to store neighbor information hashed by IP address
- Forwarding table was a map from prefix to struct called ForwardingInfo: this struct contained route type (L, S, R), next hop, cost, last updated time, and an optional field of interface name for REPL purposes
- interfaceSockets is map from interface name to socket for handling sending packets through a specific interface
- mutex, conds, all stored in map hashed by interface names (for enable/disable)

**Vhost/Vrouter**
- Called on the node package, like an API
- Separate RIP package for basic structs, marshalling, parsing
- Logic for both effectively shared; differentiated by validity of input

**Handling IP Packets**
- Protocol handler for test and RIP protocols
- For sending in general, have a makePacket method to create packets easily, forwardPacket method to find where to forward a packet if needed, checkDest to see if a packet is at its destination, etc.
- separate method for REPL send, since it is not forwarding.
- To send packets on an interface, simple socket set up, which pushes everything to forwardPackets

**Interfaces**
- as mentioned earlier, mutexes/conds and socket for each interface
- goroutine for socket handling, also have while True loop to handle incoming packets
- packets are parsed using handlePackets after reading from UDP port, handled using protocol handler.

**RIP Protocol**
- pretty much the naive implementation of what is described. Garbage collection and Periodic Updates are goroutines
- sending RIP packets using split horizon + poisoned reverse is factored out into sendRIP method
- protocol200 handles response to request/response, as well as trigger updates.
- garbage collection set to be done every 7.5 seconds