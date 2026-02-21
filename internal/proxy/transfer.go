package proxy

import (
	"math"

	"github.com/MEMOxiiii/odonata-proxy/internal/config"
	"github.com/MEMOxiiii/odonata-proxy/internal/middleware"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// transferTo switches the session from its current backend to target.
//
// The transfer is fully seamless — no "Generating World / Building terrain"
// loading screen is shown to the player:
//
//  1. beginTransfer() pauses packet forwarding.
//  2. The new backend connection is established via fast-dial.
//  3. sendChunkReset() evicts the client's chunk cache silently.
//  4. The upstream is atomically swapped.
//  5. endTransfer() resumes forwarding.
func (s *Session) transferTo(target config.BackendConfig) {
	fromName := s.CurrentBackend()

	s.log.Infow("Transfer starting",
		"player", s.info.Username,
		"from", fromName,
		"to", target.Name,
	)

	s.proxy.middleware.FireServerTransfer(&middleware.TransferContext{
		Player:      s.info,
		FromBackend: fromName,
		ToBackend:   target.Name,
	})

	s.beginTransfer()

	transferOK := false
	defer func() {
		if !transferOK {
			s.endTransfer()
		}
	}()

	var newConn *minecraft.Conn
	var dialErr error
	if pl, ok := s.proxy.pools[target.Name]; ok {
		newConn, dialErr = pl.Acquire(s.client)
	} else {
		newConn, dialErr = fastDialBackend(s.client, target.Address)
	}
	if dialErr != nil {
		s.log.Errorw("Transfer failed: cannot dial target backend",
			"backend", target.Name, "err", dialErr)
		s.Disconnect("Server transfer failed. Please reconnect.")
		return
	}

	// Evict client chunk cache without showing a loading screen.
	s.sendChunkReset()

	// Remove all entities from the old server so they don't ghost on the new one.
	// Must happen before swapping upstream so the client is clean before new
	// entities start arriving.
	s.removeAllEntities()

	s.upstreamMu.RLock()
	oldUpstream := s.upstream
	s.upstreamMu.RUnlock()
	if err := oldUpstream.Close(); err != nil {
		s.log.Warnw("Error closing old upstream during transfer",
			"backend", oldUpstream.name, "err", err)
	}

	newUpstream := &Upstream{
		conn:    newConn,
		name:    target.Name,
		address: target.Address,
	}
	s.upstreamMu.Lock()
	s.upstream = newUpstream
	s.upstreamMu.Unlock()

	transferOK = true
	s.endTransfer()

	s.log.Infow("Transfer complete", "player", s.info.Username, "to", target.Name)
}

// sendChunkReset evicts the client's entire chunk cache without triggering a
// loading screen.
//
// Sends NetworkChunkPublisherUpdate with center 30 000 000 blocks away and
// radius=0. The client silently frees all loaded chunks. No ChangeDimension
// packet is sent, so the "Generating World" dialog never appears.
func (s *Session) sendChunkReset() {
	x := int32(math.Float32frombits(s.posX.Load()))
	y := int32(math.Float32frombits(s.posY.Load()))
	z := int32(math.Float32frombits(s.posZ.Load()))

	const farOffset = int32(30_000_000)

	_ = s.client.WritePacket(&packet.NetworkChunkPublisherUpdate{
		Position: protocol.BlockPos{x + farOffset, y, z + farOffset},
		Radius:   0,
	})
}
