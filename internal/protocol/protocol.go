package protocol

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const MaxPacketSize = 65520

type Packet struct {
	Control int
	Data    []byte
}

func Encode(line string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(line)+4, line))
}

func Decode(data []byte) ([]Packet, error) {
	var result []Packet
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("truncated packet header")
		}
		for _, c := range data[:4] {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return nil, fmt.Errorf("invalid packet header")
			}
		}
		n, _ := strconv.ParseUint(string(data[:4]), 16, 16)
		if n <= 2 {
			result = append(result, Packet{Control: int(n)})
			data = data[4:]
			continue
		}
		if n < 4 || n > MaxPacketSize || int(n) > len(data) {
			return nil, fmt.Errorf("invalid packet size")
		}
		result = append(result, Packet{Control: -1, Data: data[4:n]})
		data = data[n:]
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
	packets, err := Decode(data)
	if err != nil {
		return Fetch{}, err
	}
	f := Fetch{Supported: true}
	args := false
	for i, p := range packets {
		if p.Control == 0 && i == len(packets)-1 {
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
			f.Wants = append(f.Wants, value)
		case strings.HasPrefix(line, "have "):
			value := strings.TrimPrefix(line, "have ")
			if !OID(value) {
				f.Supported = false
			}
			f.Haves = append(f.Haves, value)
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
	packets, err := Decode(data)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	ended := false
	names := make(map[string]bool)
	for i, p := range packets {
		if p.Control == 0 {
			ended = true
			if i != len(packets)-1 && !(i == len(packets)-2 && packets[i+1].Control == 2) {
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
