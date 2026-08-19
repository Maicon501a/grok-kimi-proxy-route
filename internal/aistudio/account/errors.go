package account

import "errors"

// ErrNoAccountAvailable is returned when no eligible account remains for
// selection.
var ErrNoAccountAvailable = errors.New("account: nenhuma conta disponivel para balanceamento")
