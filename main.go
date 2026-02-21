// Command odonata-proxy is a high-performance, production-ready Minecraft
// Bedrock Edition proxy built on top of the gophertunnel library.
//
// # Features
//
//   - Bidirectional, middleware-filtered packet forwarding
//   - Zero-downtime server switching (Lobby → Survival → Minigames, etc.)
//   - Modular hook system (OnPlayerJoin, OnPacketReceive, OnPacketSend,
//     OnCommand, OnServerTransfer)
//   - Per-IP token-bucket rate limiting
//   - Graceful shutdown with configurable deadline
//   - Structured logging via zap
//   - YAML-driven configuration
//
// # Getting started
//
//	go build -o odonata-proxy .
//	./odonata-proxy -config config.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/MEMOxiiii/odonata-proxy/internal/config"
	"github.com/MEMOxiiii/odonata-proxy/internal/middleware"
	"github.com/MEMOxiiii/odonata-proxy/internal/proxy"
	"github.com/MEMOxiiii/odonata-proxy/pkg/logger"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"go.uber.org/zap"
)

func main() {
	// ── CLI flags ────────────────────────────────────────────────────────────
	configPath := flag.String("config", "config.yaml", "Path to the YAML configuration file")
	flag.Parse()

	// ── Load configuration ───────────────────────────────────────────────────
	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "fatal: load config: %v\n", err)
		os.Exit(1)
	}

	// ── Initialise logger ────────────────────────────────────────────────────
	log := logger.Must(logger.Config{
		Level:      cfg.Logging.Level,
		Format:     cfg.Logging.Format,
		OutputFile: cfg.Logging.OutputFile,
	})
	defer func() { _ = log.Sync() }()

	log.Infow("Odonata Proxy starting",
		"version", "1.0.0",
		"listen", cfg.Proxy.Listen,
		"backends", len(cfg.Backends),
	)

	// ── Build proxy ──────────────────────────────────────────────────────────
	p, err := proxy.New(cfg, log)
	if err != nil {
		log.Fatalw("Failed to create proxy", zap.Error(err))
	}

	// ── Register hooks ───────────────────────────────────────────────────────
	//
	// Register your own hooks here. Each hook is called in registration order.
	// Implement the middleware.Hook interface (embed middleware.NoopHook to
	// skip methods you don't need) and register below.
	//
	// Example:
	//   p.Middleware().Register(&myCustomHook{})
	p.Middleware().Register(NewLoggingHook(log))
	p.Middleware().Register(NewServerSwitchHook(p, log))

	// ── Graceful shutdown ────────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Infow("Received shutdown signal", "signal", sig.String())

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := p.Shutdown(shutdownCtx); err != nil {
			log.Errorw("Graceful shutdown incomplete", zap.Error(err))
			os.Exit(1)
		}
	}()

	// ── Start listening ──────────────────────────────────────────────────────
	// ListenAndServe blocks until Shutdown is called.
	if err := p.ListenAndServe(); err != nil {
		log.Fatalw("Proxy exited with error", zap.Error(err))
	}

	log.Info("Odonata Proxy stopped.")
}

// ─── Built-in hooks ───────────────────────────────────────────────────────────

// LoggingHook logs every player join/leave and server transfer event.
type LoggingHook struct {
	middleware.NoopHook
	log logger.Logger
}

func NewLoggingHook(log logger.Logger) *LoggingHook { return &LoggingHook{log: log} }

func (h *LoggingHook) OnPlayerJoin(info middleware.PlayerInfo) {
	h.log.Infow("Player joined",
		"player", info.Username,
		"xuid", info.XUID,
		"ip", info.RemoteAddr,
	)
}

func (h *LoggingHook) OnPlayerLeave(info middleware.PlayerInfo) {
	h.log.Infow("Player left",
		"player", info.Username,
		"xuid", info.XUID,
	)
}

func (h *LoggingHook) OnServerTransfer(ctx *middleware.TransferContext) {
	h.log.Infow("Player transferred",
		"player", ctx.Player.Username,
		"from", ctx.FromBackend,
		"to", ctx.ToBackend,
	)
}

// ─── Server-switch hook ───────────────────────────────────────────────────────

// ServerSwitchHook handles the "/server <name>" command from players.
// It intercepts the command on the proxy side (never reaches the backend)
// and injects autocomplete entries for all configured backends.
//
// Usage in Minecraft chat:
//
//	/server lobby
//	/server survival
//	/server minigames
type ServerSwitchHook struct {
	middleware.NoopHook
	proxy *proxy.Proxy
	log   logger.Logger
}

func NewServerSwitchHook(p *proxy.Proxy, log logger.Logger) *ServerSwitchHook {
	return &ServerSwitchHook{proxy: p, log: log}
}

// OnCommand intercepts "/server <backend>" commands.
func (h *ServerSwitchHook) OnCommand(ctx *middleware.CommandContext) {
	if !strings.HasPrefix(ctx.Command, "server ") {
		return
	}
	target := strings.TrimSpace(strings.ToLower(strings.TrimPrefix(ctx.Command, "server ")))
	if target == "" {
		return
	}

	ctx.Handled = true // consume — never forwarded to backend

	// Block transfer if the player is already on the target server.
	if current, ok := h.proxy.PlayerBackend(ctx.Player.SessionID); ok {
		if strings.EqualFold(current, target) {
			_ = h.proxy.SendPlayerMessage(ctx.Player.SessionID,
				"§cYou are already connected to §e"+target+"§c!")
			return
		}
	}

	if err := h.proxy.TransferPlayer(ctx.Player.SessionID, target); err != nil {
		h.log.Warnw("Transfer request failed",
			"player", ctx.Player.Username,
			"target", target,
			"err", err,
		)
		_ = h.proxy.SendPlayerMessage(ctx.Player.SessionID,
			"§cServer §e"+target+"§c not found or is offline.")
		return
	}

	h.log.Infow("Player switching server",
		"player", ctx.Player.Username,
		"target", target,
	)
}

// OnPacketSend injects the /server command into AvailableCommands so the
// client shows autocomplete when typing /server.
func (h *ServerSwitchHook) OnPacketSend(ctx *middleware.PacketContext) {
	if ctx.ToBackend {
		return
	}
	pkt, ok := ctx.Packet.(*packet.AvailableCommands)
	if !ok {
		return
	}

	backends := h.proxy.BackendNames()
	if len(backends) == 0 {
		return
	}

	startIdx := uint32(len(pkt.EnumValues))
	pkt.EnumValues = append(pkt.EnumValues, backends...)

	indices := make([]uint32, len(backends))
	for i := range backends {
		indices[i] = startIdx + uint32(i)
	}
	enumIdx := uint32(len(pkt.Enums))
	pkt.Enums = append(pkt.Enums, protocol.CommandEnum{
		Type:         "ServerName",
		ValueIndices: indices,
	})

	pkt.Commands = append(pkt.Commands, protocol.Command{
		Name:        "server",
		Description: "Switch to a different server",
		Overloads: []protocol.CommandOverload{
			{
				Parameters: []protocol.CommandParameter{
					{
						Name: "name",
						Type: protocol.CommandArgEnum | enumIdx,
					},
				},
			},
		},
	})
}
