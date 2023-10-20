package rip

import (
	"bytes"
	"encoding/binary"
)

type RipUpdate struct {
	Cost    uint32
	Address uint32
	Mask    uint32
}

type RipPacket struct {
	Command     uint16
	Num_entries uint16
	Entries     []*RipUpdate
}

func MarshalRIP(packet *RipPacket) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.BigEndian, packet.Command)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, packet.Num_entries)
	if err != nil {
		return nil, err
	}
	for _, entry := range packet.Entries {
		err = binary.Write(buf, binary.BigEndian, entry.Cost)
		if err != nil {
			return nil, err
		}
		err = binary.Write(buf, binary.BigEndian, entry.Address)
		if err != nil {
			return nil, err
		}
		err = binary.Write(buf, binary.BigEndian, entry.Mask)
		if err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil

}

func ExtractRIP(message []byte) *RipPacket {
	command := uint16(binary.BigEndian.Uint16(message[:2]))
	num_entries := uint16(binary.BigEndian.Uint16(message[2:4]))
	iter := 4
	entries := make([]*RipUpdate, 0)
	for i := 0; i < int(num_entries); i++ {
		cost := uint32(binary.BigEndian.Uint32(message[iter : iter+4]))
		address := uint32(binary.BigEndian.Uint32(message[iter+4 : iter+8]))
		mask := uint32(binary.BigEndian.Uint32(message[iter+8 : iter+12]))
		iter += 12
		entry := &RipUpdate{
			Cost:    cost,
			Address: address,
			Mask:    mask,
		}
		entries = append(entries, entry)
	}
	packet := &RipPacket{
		Command:     command,
		Num_entries: num_entries,
		Entries:     entries,
	}
	return packet
}
