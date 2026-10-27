package policy

import (
	"errors"
	"testing"
)

func TestAuthorizeAlwaysBlocksInMVP(t *testing.T) {
	for _, mode := range []string{"observe_only", "act", ""} {
		if err := Authorize(mode, Action{Type: "recommendation"}); !errors.Is(err, ErrActionBlocked) {
			t.Errorf("modo %q: esperado ErrActionBlocked, obtido %v", mode, err)
		}
	}
}
