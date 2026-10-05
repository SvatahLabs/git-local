package overlay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestFilterProtocolKeepsRequestsSeparated(t *testing.T) {
	var request bytes.Buffer
	w := bufio.NewWriter(&request)
	writeList := func(parts ...string) {
		t.Helper()
		for _, part := range parts {
			if err := packet(w, []byte(part)); err != nil {
				t.Fatal(err)
			}
		}
		if err := flush(w); err != nil {
			t.Fatal(err)
		}
	}
	writeList("git-filter-client", "version=2")
	writeList("capability=clean", "capability=smudge", "capability=delay")
	writeList("command=clean", "pathname=config/app.conf")
	writeList("port=8080\n")
	writeList("command=smudge", "pathname=config/app.conf")
	writeList("port=80\n")
	var output, diagnostic bytes.Buffer
	count := 0
	err := ServeFilter(context.Background(), &request, &output, &diagnostic, func(_ context.Context, command, path string, data []byte) ([]byte, error) {
		count++
		if path != "config/app.conf" {
			t.Fatal(path)
		}
		if command == "clean" {
			if string(data) != "port=8080\n" {
				t.Fatal(string(data))
			}
			return []byte("port=80\n"), nil
		}
		return nil, errors.New("overlap")
	})
	if err != nil || count != 2 {
		t.Fatalf("serve: %v, requests %d", err, count)
	}
	r := packetReader{bufio.NewReader(&output)}
	for _, want := range [][]string{{"git-filter-server", "version=2"}, {"capability=clean", "capability=smudge"}, {"status=success"}, {"port=80\n"}, {}, {"status=error"}} {
		list, e := r.list(false)
		if e != nil {
			t.Fatal(e)
		}
		if len(list) != len(want) {
			t.Fatalf("got %q want %q", list, want)
		}
		for i := range want {
			if string(list[i]) != want[i] {
				t.Fatalf("got %q want %q", list[i], want[i])
			}
		}
	}
	if _, err := r.list(true); err != io.EOF {
		t.Fatalf("expected clean EOF, got %v", err)
	}
	if !bytes.Contains(diagnostic.Bytes(), []byte("overlap")) {
		t.Fatal("missing error diagnostic")
	}
}
