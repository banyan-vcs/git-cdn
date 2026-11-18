package sshproxy

import (
	"fmt"
	"io"
	"strconv"
)

type packet struct {
	raw, data []byte
	control   int
}

func readPacket(r io.Reader) (packet, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return packet{}, err
	}
	n, err := strconv.ParseUint(string(header), 16, 16)
	if err != nil {
		return packet{}, fmt.Errorf("invalid Git packet header")
	}
	if n <= 2 {
		return packet{raw: header, control: int(n)}, nil
	}
	if n < 4 || n > 65520 {
		return packet{}, fmt.Errorf("invalid Git packet length")
	}
	data := make([]byte, int(n)-4)
	if _, err := io.ReadFull(r, data); err != nil {
		return packet{}, err
	}
	return packet{raw: append(header, data...), data: data, control: -1}, nil
}

func readMessage(r io.Reader, max int64) ([]byte, error) {
	return readBoundedMessage(r, max, int(max/4))
}

func readBoundedMessage(r io.Reader, max int64, maxPackets int) ([]byte, error) {
	var data []byte
	for count := 0; count < maxPackets; count++ {
		p, err := readPacket(r)
		if err != nil {
			return nil, err
		}
		if int64(len(data)+len(p.raw)) > max {
			return nil, fmt.Errorf("Git message exceeds byte limit")
		}
		data = append(data, p.raw...)
		if p.control == 0 || p.control == 2 {
			return data, nil
		}
	}
	return nil, fmt.Errorf("too many Git packets")
}
