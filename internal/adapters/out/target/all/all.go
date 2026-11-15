// Package all registers every built-in target type. Add new types here.
package all

import (
	_ "github.com/johnnycube/stellwerk/internal/adapters/out/target/email"
	_ "github.com/johnnycube/stellwerk/internal/adapters/out/target/matrix"
	_ "github.com/johnnycube/stellwerk/internal/adapters/out/target/ntfy"
	_ "github.com/johnnycube/stellwerk/internal/adapters/out/target/webhook"
)
