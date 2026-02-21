package proxy

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MEMOxiiii/odonata-proxy/internal/middleware"
	"github.com/MEMOxiiii/odonata-proxy/pkg/logger"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"go.uber.org/zap"
)

// Session represents a single connected player and owns the entire lifecycle
// of their connection: login, bidirectional packet forwarding, and server
// transfer.
type Session struct {
	id string

	proxy    *Proxy
	client   *minecraft.Conn
	upstream *Upstream
	info     middleware.PlayerInfo

	log logger.Logger

	ctx    context.Context
	cancel context.CancelFunc

	// transferring is set to 1 while a server switch is in progress.
	transferring atomic.Int32

	// transferNotify is closed when the transfer finishes, waking parked goroutines.
	transferNotify chan struct{}
	transferMu     sync.Mutex

	// upstreamMu guards swaps to the upstream field.
	upstreamMu sync.RWMutex

	once sync.Once

	// posX/Y/Z hold the player's last known position (float32 bits stored atomically).
	posX, posY, posZ atomic.Uint32

	// entityMu guards the entities map.
	entityMu sync.Mutex
	// entities tracks every EntityUniqueID that the current backend has sent to the
	// client via AddActor / AddPlayer / AddItemActor / AddPainting.
	// On server transfer we send RemoveActor for every entry so the client never
	// sees ghost entities from the old server on the new one.
	entities map[int64]struct{}
}

// newSession constructs a fully initialised Session.
func newSession(
	id string,
	proxy *Proxy,
	client *minecraft.Conn,
	upstream *Upstream,
	info middleware.PlayerInfo,
	log logger.Logger,
	ctx context.Context,
	cancel context.CancelFunc,
) *Session {
	notify := make(chan struct{})
	close(notify) // pre-closed = "no transfer in progress"
	return &Session{
		id:             id,
		proxy:          proxy,
		client:         client,
		upstream:       upstream,
		info:           info,
		log:            log,
		ctx:            ctx,
		cancel:         cancel,
		transferNotify: notify,
		entities:       make(map[int64]struct{}),
	}
}

// ID returns the proxy-internal session UUID.
func (s *Session) ID() string { return s.id }

// PlayerInfo returns the identity information for this player.
func (s *Session) PlayerInfo() middleware.PlayerInfo { return s.info }

// CurrentBackend returns the name of the backend server this session is connected to.
func (s *Session) CurrentBackend() string {
	s.upstreamMu.RLock()
	defer s.upstreamMu.RUnlock()
	return s.upstream.name
}

// Disconnect gracefully closes both connections.
func (s *Session) Disconnect(reason string) {
	s.once.Do(func() {
		_ = s.proxy.listener.Disconnect(s.client, reason)
		s.upstreamMu.RLock()
		up := s.upstream
		s.upstreamMu.RUnlock()
		if up != nil {
			_ = up.conn.Close()
		}
		s.cancel()
	})
}

// SendMessage sends a system chat message directly to the client.
func (s *Session) SendMessage(text string) {
	_ = s.client.WritePacket(&packet.Text{
		TextType: packet.TextTypeSystem,
		Message:  text,
	})
}

// run starts bidirectional packet forwarding and blocks until both directions end.
func (s *Session) run() {
	errCh := make(chan error, 2)

	go func() { errCh <- s.forwardClientToBackend() }()
	go func() { errCh <- s.forwardBackendToClient() }()

	select {
	case err := <-errCh:
		if err != nil {
			s.log.Debugw("Session pipe closed", zap.Error(err))
		}
	case <-s.ctx.Done():
	}

	s.cancel()
	_ = s.client.Close()
	s.upstreamMu.RLock()
	up := s.upstream
	s.upstreamMu.RUnlock()
	if up != nil {
		_ = up.conn.Close()
	}
	<-errCh
}

// forwardClientToBackend reads packets from the client and writes to the backend.
func (s *Session) forwardClientToBackend() error {
	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
		}

		if !s.proxy.limiter.Allow(s.info.RemoteAddr) {
			s.log.Warnw("Rate limit exceeded; dropping packet", "player", s.info.Username)
			continue
		}

		pk, err := s.client.ReadPacket()
		if err != nil {
			return fmt.Errorf("read client packet: %w", err)
		}

		// Track player position for the chunk-reset hint.
		if mp, ok := pk.(*packet.MovePlayer); ok {
			s.posX.Store(math.Float32bits(mp.Position[0]))
			s.posY.Store(math.Float32bits(mp.Position[1]))
			s.posZ.Store(math.Float32bits(mp.Position[2]))
		}

		// Sanitise outgoing chat Text packets.
		// The Bedrock client includes FilteredMessage, XUID and other fields
		// that Dragonfly validates strictly. Critically: the proxy connects to
		// backends without a TokenSource, so gophertunnel calls
		// clearXBLIdentityData() and strips the XUID to "" before sending the
		// login request. The backend therefore stores XUID="" for this session.
		// If we forward the client's real XUID, Dragonfly rejects the message
		// and disconnects the player. We must send XUID="" to match the backend
		// session's stored value.
		if txtPkt, ok := pk.(*packet.Text); ok && txtPkt.TextType == packet.TextTypeChat {
			pk = &packet.Text{
				TextType:   packet.TextTypeChat,
				SourceName: s.info.Username,
				Message:    txtPkt.Message,
				XUID:       "", // backend session has XUID="" (cleared by gophertunnel offline dial)
			}
		}

		// Intercept slash-commands.
		if cmdPkt, ok := pk.(*packet.CommandRequest); ok {
			if s.handleCommand(cmdPkt) {
				continue
			}
		}

		pctx := &middleware.PacketContext{
			Player:    s.info,
			Packet:    pk,
			ToBackend: true,
		}
		s.upstreamMu.RLock()
		pctx.BackendName = s.upstream.name
		s.upstreamMu.RUnlock()

		s.proxy.middleware.FirePacketReceive(pctx)
		if pctx.Blocked {
			continue
		}

		if s.transferring.Load() == 1 {
			continue
		}

		s.upstreamMu.RLock()
		up := s.upstream
		s.upstreamMu.RUnlock()

		if err := up.conn.WritePacket(pctx.Packet); err != nil {
			if s.transferring.Load() == 1 {
				continue
			}
			return fmt.Errorf("write to backend: %w", err)
		}
	}
}

// ─── Entity tracker ──────────────────────────────────────────────────────────

// trackEntity records an entity unique ID spawned by the current backend.
func (s *Session) trackEntity(uid int64) {
	s.entityMu.Lock()
	s.entities[uid] = struct{}{}
	s.entityMu.Unlock()
}

// untrackEntity removes an entity unique ID (called when the backend despawns it normally).
func (s *Session) untrackEntity(uid int64) {
	s.entityMu.Lock()
	delete(s.entities, uid)
	s.entityMu.Unlock()
}

// removeAllEntities sends a RemoveActor packet to the client for every entity
// tracked from the current backend, then clears the tracker.
// Call this during server transfer before swapping the upstream so the client
// never sees ghost entities from the old server on the new one.
func (s *Session) removeAllEntities() {
	s.entityMu.Lock()
	ids := make([]int64, 0, len(s.entities))
	for uid := range s.entities {
		ids = append(ids, uid)
	}
	s.entities = make(map[int64]struct{})
	s.entityMu.Unlock()

	for _, uid := range ids {
		_ = s.client.WritePacket(&packet.RemoveActor{
			EntityUniqueID: uid,
		})
	}
}

// forwardBackendToClient reads packets from the backend and writes to the client.
func (s *Session) forwardBackendToClient() error {
	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
		}

		s.upstreamMu.RLock()
		up := s.upstream
		s.upstreamMu.RUnlock()

		pk, err := up.conn.ReadPacket()
		if err != nil {
			s.transferMu.Lock()
			ch := s.transferNotify
			s.transferMu.Unlock()

			select {
			case <-ch:
				continue
			case <-s.ctx.Done():
				return nil
			}
		}

		if s.transferring.Load() == 1 {
			continue
		}

		// Track entities spawned by this backend so we can remove them on transfer.
		switch p := pk.(type) {
		case *packet.AddActor:
			s.trackEntity(p.EntityUniqueID)
		case *packet.AddPlayer:
			// AddPlayer has no EntityUniqueID in gophertunnel; the unique ID
			// used by RemoveActor is int64(EntityRuntimeID) by convention.
			s.trackEntity(int64(p.EntityRuntimeID))
		case *packet.AddItemActor:
			s.trackEntity(p.EntityUniqueID)
		case *packet.AddPainting:
			s.trackEntity(p.EntityUniqueID)
		case *packet.RemoveActor:
			// Entity despawned normally — remove from tracker.
			s.untrackEntity(p.EntityUniqueID)
		}

		pctx := &middleware.PacketContext{
			Player:      s.info,
			Packet:      pk,
			ToBackend:   false,
			BackendName: up.name,
		}
		s.proxy.middleware.FirePacketSend(pctx)
		if pctx.Blocked {
			continue
		}

		if err := s.client.WritePacket(pctx.Packet); err != nil {
			return fmt.Errorf("write to client: %w", err)
		}
	}
}

// handleCommand processes a CommandRequestPacket through the OnCommand hooks.
func (s *Session) handleCommand(pkt *packet.CommandRequest) bool {
	cmdLine := strings.TrimPrefix(pkt.CommandLine, "/")
	cmdCtx := &middleware.CommandContext{
		Player:  s.info,
		Command: cmdLine,
	}
	s.proxy.middleware.FireCommand(cmdCtx)
	return cmdCtx.Handled
}

// beginTransfer raises the transferring flag and replaces transferNotify.
func (s *Session) beginTransfer() {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	s.transferNotify = make(chan struct{})
	s.transferring.Store(1)
}

// endTransfer clears the transferring flag and wakes parked goroutines.
func (s *Session) endTransfer() {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	s.transferring.Store(0)
	close(s.transferNotify)
}
