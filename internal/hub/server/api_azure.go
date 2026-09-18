package server

import "context"

// Azurer is the hub seen from the server: reload the client after a save, and
// run one connection test on demand. An interface rather than the hub itself,
// so the server does not import the package that imports it.
type Azurer interface {
	ReloadAzure()
	TestAzure(ctx context.Context) error
}
