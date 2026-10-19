package history

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/storage"
)

type interruptedPublication struct {
	current string
	publish bool
}

func (p *interruptedPublication) History(_ context.Context, _ string, _ io.Reader, args ...string) ([]byte, error) {
	switch args[0] {
	case "update-ref":
		if p.publish {
			p.current = args[3]
		}
		return nil, context.Canceled
	case "symbolic-ref":
		return nil, &gitexec.Error{Command: "symbolic-ref", Code: 1}
	case "show-ref":
		return nil, nil
	case "rev-parse":
		return []byte(p.current + "\n"), nil
	default:
		panic("unexpected history command")
	}
}

func TestAdvanceReconcilesCancellationAfterRefPublication(t *testing.T) {
	old, next, foreign := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, tc := range []struct {
		name    string
		current string
		publish bool
		want    error
	}{
		{"published", old, true, nil},
		{"not published", old, false, context.Canceled},
		{"unexpected ref", foreign, false, storage.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &interruptedPublication{current: tc.current, publish: tc.publish}
			err := advance(context.Background(), state, "store", "refs/gitlocal/test", next, old)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("publication result: %v, want %v", err, tc.want)
			}
		})
	}
}
