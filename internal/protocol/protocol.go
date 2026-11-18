package protocol

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const (
	MaxPacketSize     = 65520
	MaxCommandPackets = 4096
	maxLocalWants     = 64
	maxLocalHaves     = 128
)

type Packet struct {
	Control int
	Data    []byte
}

func Encode(line string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(line)+4, line))
}

func nextPacket(data []byte) (Packet, []byte, error) {
	if len(data) < 4 {
		return Packet{}, nil, fmt.Errorf("truncated packet header")
	}
	for _, c := range data[:4] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return Packet{}, nil, fmt.Errorf("invalid packet header")
		}
	}
	n, _ := strconv.ParseUint(string(data[:4]), 16, 16)
	if n <= 2 {
		return Packet{Control: int(n)}, data[4:], nil
	}
	if n < 4 || n > MaxPacketSize || int(n) > len(data) {
		return Packet{}, nil, fmt.Errorf("invalid packet size")
	}
	return Packet{Control: -1, Data: data[4:n]}, data[n:], nil
}

func Decode(data []byte) ([]Packet, error) {
	var result []Packet
	for len(data) > 0 {
		if len(result) >= MaxCommandPackets {
			return nil, fmt.Errorf("too many packets")
		}
		p, rest, err := nextPacket(data)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
		data = rest
	}
	return result, nil
}

type Fetch struct {
	Command    string
	Wants      []string
	Haves      []string
	IncludeTag bool
	Supported  bool
}

func IsV2(header string) bool {
	var version string
	for _, field := range strings.Split(header, ":") {
		if strings.HasPrefix(field, "version=") {
			if version != "" {
				return false
			}
			version = field
		}
	}
	return version == "version=2"
}

func OID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func ParseFetch(data []byte) (Fetch, error) {
	f := Fetch{Supported: true}
	args := false
	for count := 0; len(data) > 0; count++ {
		if count >= MaxCommandPackets {
			return f, fmt.Errorf("too many command packets")
		}
		p, rest, err := nextPacket(data)
		if err != nil {
			return f, err
		}
		data = rest
		if p.Control == 0 && len(data) == 0 {
			if !args {
				f.Supported = false
			}
			return f, nil
		}
		if p.Control == 1 && !args {
			args = true
			continue
		}
		if p.Control != -1 {
			return f, fmt.Errorf("unexpected control packet")
		}
		line := strings.TrimSuffix(string(p.Data), "\n")
		if strings.ContainsAny(line, "\x00\r\n") {
			return f, fmt.Errorf("invalid command line")
		}
		if !args {
			switch {
			case strings.HasPrefix(line, "command="):
				if f.Command != "" {
					return f, fmt.Errorf("duplicate command")
				}
				f.Command = strings.TrimPrefix(line, "command=")
			case strings.HasPrefix(line, "agent="), line == "object-format=sha1":
			default:
				f.Supported = false
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			value := strings.TrimPrefix(line, "want ")
			if !OID(value) {
				f.Supported = false
			}
			if len(f.Wants) < maxLocalWants {
				f.Wants = append(f.Wants, value)
			} else {
				f.Supported = false
			}
		case strings.HasPrefix(line, "have "):
			value := strings.TrimPrefix(line, "have ")
			if !OID(value) {
				f.Supported = false
			}
			if len(f.Haves) < maxLocalHaves {
				f.Haves = append(f.Haves, value)
			} else {
				f.Supported = false
			}
		case line == "include-tag":
			f.IncludeTag = true
		case line == "thin-pack", line == "ofs-delta", line == "no-progress", line == "done", line == "wait-for-done":
		default:
			f.Supported = false
		}
	}
	return f, fmt.Errorf("request must end with flush")
}

type Ref struct {
	Name   string
	OID    string
	Peeled string
	Symref string
}

type Snapshot struct {
	Refs []Ref
}

func RefName(name string) bool {
	if name == "HEAD" {
		return true
	}
	if !strings.HasPrefix(name, "refs/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for _, c := range name {
		if c <= ' ' || c == 127 || strings.ContainsRune("~^:?*[\\", c) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func ParseRefs(data []byte) (Snapshot, error) {
	var s Snapshot
	ended := false
	names := make(map[string]bool)
	for len(data) > 0 {
		p, rest, err := nextPacket(data)
		if err != nil {
			return s, err
		}
		data = rest
		if p.Control == 0 {
			ended = true
			if len(data) != 0 && !bytes.Equal(data, []byte("0002")) {
				return s, fmt.Errorf("data after ref response")
			}
			break
		}
		if p.Control != -1 || bytes.ContainsAny(p.Data, "\x00\r") {
			return s, fmt.Errorf("invalid ref packet")
		}
		fields := strings.Split(strings.TrimSuffix(string(p.Data), "\n"), " ")
		if len(fields) < 2 || !RefName(fields[1]) || names[fields[1]] {
			return s, fmt.Errorf("invalid ref")
		}
		if fields[0] == "unborn" {
			continue
		}
		if !OID(fields[0]) {
			return s, fmt.Errorf("unsupported object format or ref response")
		}
		ref := Ref{Name: fields[1], OID: fields[0]}
		for _, attribute := range fields[2:] {
			switch {
			case strings.HasPrefix(attribute, "peeled:"):
				ref.Peeled = strings.TrimPrefix(attribute, "peeled:")
				if !OID(ref.Peeled) {
					return s, fmt.Errorf("invalid peeled OID")
				}
			case strings.HasPrefix(attribute, "symref-target:"):
				ref.Symref = strings.TrimPrefix(attribute, "symref-target:")
				if !RefName(ref.Symref) {
					return s, fmt.Errorf("invalid symref")
				}
			default:
				return s, fmt.Errorf("unsupported ref attribute")
			}
		}
		names[ref.Name] = true
		s.Refs = append(s.Refs, ref)
	}
	if !ended {
		return s, fmt.Errorf("unterminated ref response")
	}
	return s, nil
}

func (s Snapshot) Allows(wants []string) bool {
	allowed := make(map[string]bool)
	for _, ref := range s.Refs {
		allowed[ref.OID] = true
		if ref.Peeled != "" {
			allowed[ref.Peeled] = true
		}
	}
	for _, want := range wants {
		if !allowed[want] {
			return false
		}
	}
	return len(wants) > 0
}

func LSRefsRequest() []byte {
	var b bytes.Buffer
	b.Write(Encode("command=ls-refs\n"))
	b.Write(Encode("object-format=sha1\n"))
	b.WriteString("0001")
	b.Write(Encode("symrefs\n"))
	b.Write(Encode("peel\n"))
	b.WriteString("0000")
	return b.Bytes()
}
