// Package middleware defines the Hook interface and Registry used to extend
// proxy behaviour without modifying core code.
//
// # Usage
//
//  1. Implement the Hook interface (embed NoopHook to skip unneeded methods).
//  2. Register with Registry.Register before starting the proxy.
//  3. The proxy calls Registry.Fire* at the appropriate lifecycle points.
package middleware

import (
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// ─── Context types ────────────────────────────────────────────────────────────

// PlayerInfo carries identity fields for a connected player.
type PlayerInfo struct {
	// SessionID is the proxy-internal UUID. Use with Proxy.TransferPlayer.
	SessionID string

	// Username is the Bedrock / Xbox display name.
	Username string

	// XUID is the Xbox User ID. Empty when auth is disabled.
	XUID string

	// DeviceID is the UUID of the player's device.
	DeviceID string

	// SelfSignedID is the self-signed UUID from the client identity chain.
	SelfSignedID string

	// RemoteAddr is the player's IP:port as seen by the proxy.
	RemoteAddr string
}

// PacketContext wraps a packet flowing in one direction.
// Hooks may inspect, replace (ctx.Packet = newPkt), or drop (ctx.Blocked = true) it.
type PacketContext struct {
	Player      PlayerInfo
	Packet      packet.Packet
	ToBackend   bool   // true = client→backend, false = backend→client
	BackendName string // name of the current backend (as in config)
	Blocked     bool   // set to true to drop the packet
}

// CommandContext is passed to OnCommand when a slash-command is received.
type CommandContext struct {
	Player  PlayerInfo
	Command string // raw command without leading slash
	Handled bool   // set to true to prevent forwarding to backend
}

// TransferContext carries details of a server-switch event.
type TransferContext struct {
	Player      PlayerInfo
	FromBackend string
	ToBackend   string
}

// ─── Hook interface ───────────────────────────────────────────────────────────

// Hook is the primary extension point for the proxy.
// Embed NoopHook to implement only the methods you care about.
type Hook interface {
	OnPlayerJoin(info PlayerInfo)
	OnPlayerLeave(info PlayerInfo)
	OnPacketReceive(ctx *PacketContext)
	OnPacketSend(ctx *PacketContext)
	OnCommand(ctx *CommandContext)
	OnServerTransfer(ctx *TransferContext)
}

// NoopHook is an empty Hook implementation safe to embed.
//
//	type MyHook struct { middleware.NoopHook }
//	func (h *MyHook) OnPlayerJoin(info middleware.PlayerInfo) { ... }
type NoopHook struct{}

func (NoopHook) OnPlayerJoin(_ PlayerInfo)           {}
func (NoopHook) OnPlayerLeave(_ PlayerInfo)          {}
func (NoopHook) OnPacketReceive(_ *PacketContext)    {}
func (NoopHook) OnPacketSend(_ *PacketContext)       {}
func (NoopHook) OnCommand(_ *CommandContext)         {}
func (NoopHook) OnServerTransfer(_ *TransferContext) {}

// ─── Registry ─────────────────────────────────────────────────────────────────

// Registry holds an ordered list of Hook implementations.
// Register all hooks before starting the proxy; Fire* calls are not thread-safe
// with respect to Register.
type Registry struct{ hooks []Hook }

func NewRegistry() *Registry { return &Registry{} }

// Register appends h to the registry. Hooks are called in registration order.
func (r *Registry) Register(h Hook) { r.hooks = append(r.hooks, h) }

func (r *Registry) FirePlayerJoin(info PlayerInfo) {
	for _, h := range r.hooks {
		h.OnPlayerJoin(info)
	}
}

func (r *Registry) FirePlayerLeave(info PlayerInfo) {
	for _, h := range r.hooks {
		h.OnPlayerLeave(info)
	}
}

func (r *Registry) FirePacketReceive(ctx *PacketContext) {
	for _, h := range r.hooks {
		if ctx.Blocked {
			return
		}
		h.OnPacketReceive(ctx)
	}
}

func (r *Registry) FirePacketSend(ctx *PacketContext) {
	for _, h := range r.hooks {
		if ctx.Blocked {
			return
		}
		h.OnPacketSend(ctx)
	}
}

func (r *Registry) FireCommand(ctx *CommandContext) {
	for _, h := range r.hooks {
		if ctx.Handled {
			return
		}
		h.OnCommand(ctx)
	}
}

func (r *Registry) FireServerTransfer(ctx *TransferContext) {
	for _, h := range r.hooks {
		h.OnServerTransfer(ctx)
	}
}
