# IP 

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

# TCP
Most of the design choices made for TCP were fairly direct. 

**Three Way Handshake**
- implemented directly using VAccept and VConnect: packets were sent/received manually
- initial sequence and ack numbers are stored for future use

**Socket**
- socket table designed very similar to how the routing table in IP was designed, storing all necessary fields using a map as the main data structure (with structs to store needed data)
- has a read and write buffer, along with cumulative pointers (for ease of maintaining the proper ack/seq numbers to send)
- has the early arrival priority queue, client window size, queue of unacked segments

**Circular Buffer**
- this was done fairly directly: we decided to utilize uint32 to account for seq/ack number wrapping, and the rest was done manually using mod WINDOW_SIZE
- pointers are all stored in the natural way for each buffer
- most pointer movement handled in VRead and VWrite

**Sending**
- we decided that the best way to approach sending/receiving is to have separate threads for both
- the sending thread will break up the message into segments, and pass it into a sliding window
- the sliding window handles the unacked number queue, as well as setting up the packets for sending
- in the sliding window function, we have the thread for retransmissions (will discuss later)
- zero window probing is also done in this function
- we decided to maintain a sliding window **for each message sent**: by maintaining a separate queue of unacked segments for each message, it makes it much easier to handle ack numbers + determining which segments prior are acked as a result of a received ack.

**Receiving**
- this thread will receive a packet and first determine if it's an ack to a packet sent (using the unacked queue): if so, it will pass it to the sliding window for trimming as needed
- otherwise, it will copy the packet into the buffer and update circular buffer pointers as needed
- then, it builds the ack packet to send back

**Remaining REPL Commands**
Most of these are done in the direct way 
- VClose is done by maintaining unacked FIN numbers and a straight implementation of the RFC 
- sf/rf are just repeated VWrite/VRead calls, with a VClose at the end
- ls is direct printing of our socket table, which, as mentioned, is maintained in the TCB Stack

**Retransmission**
- the design choice of maintaining a sliding window makes this very easy
- once the RTO ticker pops, we only need to retransmit the head of the window and update its last sent time.
- by maintaining in the window node whether or not it has been retransmitted, we are able to avoid using retransmitted packets for RTT calculations

**Zero Window Probing**
- we pop into this for loop in the sending thread (to block off everything else) when the client window size is 0
- then, the next byte in the payload is sent repeatedly until the corresponding ack is received + the client window size is positive
- to make this compatible with the sliding window, this requires looking at the head and trimming the first packet by 1 byte (if necessary)

**Flags**
- most of the flag + state logic is handled by mapping numbers to respective strings in a list of consts / stateMap that we created 

**Known Bugs**

The only issue at the moment is zero window probing, which seems to end up in a loop of meaningless acks sometimes. However, this is not that frequent and does not seem to affect other functionality.

**Packet Capture**
- 3-way handshake: frames 1-3
- sent/ack segment: frames 4-5
- retransmitted segment: frame 130
- connection teardown: frames 1834-1837