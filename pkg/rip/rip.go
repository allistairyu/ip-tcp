package rip

type ripUpdate struct {
	cost    uint32
	address uint32
	mask    uint32
}

type ripPacket struct {
	command     uint16
	num_entries uint16
	entries     []*ripUpdate
}
