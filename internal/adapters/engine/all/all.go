// Package all registers every built-in decision engine. Add new ones here.
package all

import (
	_ "github.com/johnnycube/stellwerk/internal/adapters/engine/cel" // registers itself in init
)
