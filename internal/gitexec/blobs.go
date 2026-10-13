package gitexec

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// BlobReader streams private history blobs through one git cat-file --batch
// process, for a caller that reads many: seeding a worktree wrote each file
// through its own cat-file process. Blob and Close are not safe for
// concurrent use.
type BlobReader struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *boundedBuffer
	// err is the first failure; the stream can't be trusted after it.
	err error
}

// HistoryBlobs starts a BlobReader on the private store, with the same
// controls and store checks as HistoryBlob. The caller must Close it.
func (r *Runner) HistoryBlobs(ctx context.Context, store string) (*BlobReader, error) {
	if err := r.CheckCapabilities(ctx); err != nil {
		return nil, err
	}
	if err := r.checkStore(store); err != nil {
		return nil, err
	}
	argv := append(r.controls(), "--git-dir="+store, "cat-file", "--batch")
	cmd := exec.CommandContext(ctx, r.executable, argv...)
	cmd.Dir = store
	cmd.Env = append(append([]string{}, r.env...), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	cmd.WaitDelay = 2 * time.Second
	stderr := &boundedBuffer{}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	r.processes.Add(1)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Git: %w", err)
	}
	return &BlobReader{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 64<<10), stderr: stderr}, nil
}

// Blob writes blob id's content to dst. Any failure, including dst's, ends
// the reader: later calls return the same error.
func (b *BlobReader) Blob(id string, dst io.Writer) error {
	if b.err != nil {
		return b.err
	}
	if !validObjectID(id) {
		return errors.New("invalid blob ID")
	}
	b.err = b.blob(id, dst)
	return b.err
}

func (b *BlobReader) blob(id string, dst io.Writer) error {
	if _, err := io.WriteString(b.in, id+"\n"); err != nil {
		return b.failed(err)
	}
	header, err := b.out.ReadString('\n')
	if err != nil {
		return b.failed(err)
	}
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[0] == id && fields[1] == "missing" {
		return fmt.Errorf("private history object %s is missing", id)
	}
	if len(fields) != 3 || fields[0] != id || fields[1] != "blob" {
		return fmt.Errorf("private history object %s isn't a blob", id)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return errors.New("malformed Git batch output")
	}
	// io.CopyN would drop dst's error once every byte was written.
	if n, err := io.Copy(dst, io.LimitReader(b.out, size)); err != nil {
		return err
	} else if n != size {
		return b.failed(io.ErrUnexpectedEOF)
	}
	if c, err := b.out.ReadByte(); err != nil || c != '\n' {
		return errors.New("malformed Git batch output")
	}
	return nil
}

// failed adds Git's own message to a pipe failure, once Git has stopped.
func (b *BlobReader) failed(err error) error {
	b.in.Close()
	io.Copy(io.Discard, b.out)
	if waitErr := b.cmd.Wait(); waitErr != nil {
		b.cmd = nil
		return fmt.Errorf("git cat-file --batch: %w: %s", waitErr, strings.TrimSpace(b.stderr.String()))
	}
	b.cmd = nil
	return err
}

// Close ends the cat-file process. It reads what Git still writes (the rest
// of a blob whose copy failed) so Git can exit.
func (b *BlobReader) Close() error {
	if b.cmd == nil {
		return nil
	}
	b.in.Close()
	io.Copy(io.Discard, b.out)
	err := b.cmd.Wait()
	b.cmd = nil
	if b.err != nil {
		return nil
	}
	return err
}

func validObjectID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
