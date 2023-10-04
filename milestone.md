# iptcp-luke-allistair
iptcp-luke-allistair created by GitHub Classroom

Node (host/router)

- Read in Config file
- Create socket to represent each interface (one thread per interface)
- Create a map to represent forwarding table (neighbor addr info —> interface name)
- Create a neighbor struct to wrap neighbor IP, UDP address, interface
- Create map (neighbor IP —> neighbor struct)
- Enabling/disabling an interface is just with a boolean
- Iterate through forwarding table to get list of neighbors
