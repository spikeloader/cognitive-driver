// Package policy decide o que um driver tem permissão de fazer.
package policy

import (
	"errors"
	"fmt"

	"github.com/spikeloader/cognitive-driver/internal/config"
)

// ErrActionBlocked é retornado para toda ação no MVP.
var ErrActionBlocked = errors.New("ação bloqueada pela política")

// Action é uma ação proposta por um driver.
type Action struct {
	Type        string `json:"type"` // "recommendation" no MVP
	Description string `json:"description"`
}

// Authorize nunca autoriza execução no MVP: o modo observe_only só
// permite observar, detectar, explicar e recomendar.
func Authorize(mode string, a Action) error {
	if mode == config.ModeObserveOnly {
		return fmt.Errorf("%w: modo %s não executa ações (%s)", ErrActionBlocked, mode, a.Type)
	}
	return fmt.Errorf("%w: modo %q desconhecido", ErrActionBlocked, mode)
}
