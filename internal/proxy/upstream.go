package proxy

import (
	"github.com/sandertv/gophertunnel/minecraft"
)

// Upstream represents the proxy's connection to a single backend Dragonfly
// server on behalf of one player session.
type Upstream struct {
	conn    *minecraft.Conn
	name    string
	address string
}

// Close terminates the connection to the backend server.
func (u *Upstream) Close() error {
	if u.conn == nil {
		return nil
	}
	return u.conn.Close()
}

// Name returns the backend identifier.
func (u *Upstream) Name() string { return u.name }

// Address returns the backend host:port.
func (u *Upstream) Address() string { return u.address }
