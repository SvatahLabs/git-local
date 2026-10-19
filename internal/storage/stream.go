package storage

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

// Observation retains handle-derived identity and observable read state.
// Recheck does not read contents and cannot detect edits that restore all evidence.
type Observation struct{ info fs.FileInfo }

func (o Observation) Mode() fs.FileMode { return o.info.Mode() }
func sameObservation(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime() && a.Mode() == b.Mode()
}
func (o Observation) Recheck(d *Dir, name string) error {
	f, err := d.open(name, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("%w: captured file changed at %q", ErrConflict, name)
	}
	defer f.Close()
	now, err := f.Stat()
	if err != nil {
		return err
	}
	if err = ValidateInfo(now); err != nil {
		return err
	}
	if !sameObservation(o.info, now) {
		return fmt.Errorf("%w: captured file changed at %q", ErrConflict, name)
	}
	return nil
}

// StreamStable supplies a bounded view of the initially observed file to consume.
// The callback must consume all bytes. No user bytes are buffered or logged here.
func (d *Dir) StreamStable(name string, consume func(io.Reader) error) (Observation, error) {
	return d.StreamStableSize(name, func(_ int64, input io.Reader) error { return consume(input) })
}

// StreamStableSize is StreamStable with the observed size, which the bounded
// view supplies exactly, given to consume first.
func (d *Dir) StreamStableSize(name string, consume func(int64, io.Reader) error) (Observation, error) {
	f, err := d.open(name, os.O_RDONLY, 0)
	if err != nil {
		return Observation{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return Observation{}, err
	}
	if err = ValidateInfo(before); err != nil {
		return Observation{}, err
	}
	if !before.Mode().IsRegular() {
		return Observation{}, fmt.Errorf("expected regular file at %q", name)
	}
	input := &io.LimitedReader{R: f, N: before.Size()}
	if err = consume(before.Size(), input); err != nil {
		return Observation{}, err
	}
	if input.N != 0 {
		return Observation{}, fmt.Errorf("%w: incomplete file read at %q", ErrConflict, name)
	}
	after, err := f.Stat()
	if err != nil {
		return Observation{}, err
	}
	if !sameObservation(before, after) {
		return Observation{}, fmt.Errorf("%w: file changed while reading %q", ErrConflict, name)
	}
	observation := Observation{before}
	if err = observation.Recheck(d, name); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

func (o Observation) Size() int64 { return o.info.Size() }

func (o Observation) ModTime() time.Time { return o.info.ModTime() }

// Observe opens name and observes it, as StreamStable does, without reading
// its content: for a file whose content is known from its stat fields.
func (d *Dir) Observe(name string) (Observation, error) {
	f, err := d.open(name, os.O_RDONLY, 0)
	if err != nil {
		return Observation{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Observation{}, err
	}
	if err = ValidateInfo(info); err != nil {
		return Observation{}, err
	}
	if !info.Mode().IsRegular() {
		return Observation{}, fmt.Errorf("expected regular file at %q", name)
	}
	return Observation{info}, nil
}
