package overlay

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxPacket = 65520
const maxFilterInput = 16 << 20

type Transform func(context.Context, string, string, []byte) ([]byte, error)

type packetReader struct{ *bufio.Reader }

func (r packetReader) packet() ([]byte, bool, error) {
	var header [4]byte
	if _, err := io.ReadFull(r.Reader, header[:]); err != nil {
		return nil, false, err
	}
	var size uint16
	if _, err := fmt.Sscanf(string(header[:]), "%04x", &size); err != nil {
		return nil, false, errors.New("invalid filter packet length")
	}
	if size == 0 {
		return nil, true, nil
	}
	if size < 4 || int(size) > maxPacket {
		return nil, false, errors.New("invalid filter packet size")
	}
	data := make([]byte, int(size)-4)
	if _, err := io.ReadFull(r.Reader, data); err != nil {
		return nil, false, err
	}
	return data, false, nil
}
func (r packetReader) list(allowEOF bool) ([][]byte, error) {
	result := [][]byte{}
	for {
		data, flush, err := r.packet()
		if err == io.EOF && allowEOF && len(result) == 0 {
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}
		if flush {
			return result, nil
		}
		if len(result) >= 128 {
			return nil, errors.New("filter packet list too long")
		}
		result = append(result, data)
	}
}
func packet(w *bufio.Writer, data []byte) error {
	if len(data)+4 > maxPacket {
		return errors.New("filter output packet too large")
	}
	var header [4]byte
	hex.Encode(header[:], []byte{byte((len(data) + 4) >> 8), byte(len(data) + 4)})
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}
func flush(w *bufio.Writer) error {
	if _, err := w.WriteString("0000"); err != nil {
		return err
	}
	return w.Flush()
}
func contains(lines [][]byte, want string) bool {
	for _, line := range lines {
		if strings.TrimSuffix(string(line), "\n") == want {
			return true
		}
	}
	return false
}
func field(lines [][]byte, key string) string {
	for _, line := range lines {
		s := strings.TrimSuffix(string(line), "\n")
		if strings.HasPrefix(s, key+"=") {
			return strings.TrimPrefix(s, key+"=")
		}
	}
	return ""
}

// ServeFilter implements Git's version-2 long-running clean/smudge protocol.
// No content is emitted when a transform fails; Git receives status=error.
func ServeFilter(ctx context.Context, input io.Reader, output io.Writer, diagnostic io.Writer, transform Transform) error {
	r := packetReader{bufio.NewReader(input)}
	w := bufio.NewWriter(output)
	initial, err := r.list(false)
	if err != nil {
		return err
	}
	if !contains(initial, "git-filter-client") || !contains(initial, "version=2") {
		return errors.New("unsupported Git filter handshake")
	}
	for _, line := range []string{"git-filter-server", "version=2"} {
		if err := packet(w, []byte(line)); err != nil {
			return err
		}
	}
	if err := flush(w); err != nil {
		return err
	}
	capabilities, err := r.list(false)
	if err != nil {
		return err
	}
	if !contains(capabilities, "capability=clean") || !contains(capabilities, "capability=smudge") {
		return errors.New("Git lacks required filter capabilities")
	}
	for _, line := range []string{"capability=clean", "capability=smudge"} {
		if err := packet(w, []byte(line)); err != nil {
			return err
		}
	}
	if err := flush(w); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		headers, err := r.list(true)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		command, path := field(headers, "command"), field(headers, "pathname")
		var content []byte
		tooLarge := false
		for {
			part, end, e := r.packet()
			if e != nil {
				return e
			}
			if end {
				break
			}
			if len(content)+len(part) > maxFilterInput {
				tooLarge = true
				continue
			}
			if !tooLarge {
				content = append(content, part...)
			}
		}
		if command != "clean" && command != "smudge" {
			err = errors.New("unsupported filter command")
		} else if path == "" || strings.ContainsAny(path, "\x00\r\n") {
			err = errors.New("invalid filter pathname")
		} else if tooLarge {
			err = errors.New("filter input exceeds 16 MiB")
		} else {
			content, err = transform(ctx, command, path, content)
		}
		if err != nil {
			fmt.Fprintf(diagnostic, "git-local overlay: %s: %v\n", path, err)
			if e := packet(w, []byte("status=error")); e != nil {
				return e
			}
			if e := flush(w); e != nil {
				return e
			}
			continue
		}
		if e := packet(w, []byte("status=success")); e != nil {
			return e
		}
		if e := flush(w); e != nil {
			return e
		}
		for len(content) > 0 {
			n := min(len(content), maxPacket-4)
			if e := packet(w, content[:n]); e != nil {
				return e
			}
			content = content[n:]
		}
		if e := flush(w); e != nil {
			return e
		}
		if e := flush(w); e != nil {
			return e
		}
	}
}
