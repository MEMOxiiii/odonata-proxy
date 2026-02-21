// Package proxy implements the core proxy engine: it accepts Bedrock clients,
// establishes connections to backend Dragonfly servers, and provides
// bidirectional, middleware-filtered packet forwarding with support for
// zero-downtime server switching.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/MEMOxiiii/odonata-proxy/internal/config"
	"github.com/MEMOxiiii/odonata-proxy/internal/middleware"
	"github.com/MEMOxiiii/odonata-proxy/internal/pool"
	"github.com/MEMOxiiii/odonata-proxy/internal/ratelimit"
	"github.com/MEMOxiiii/odonata-proxy/pkg/logger"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/resource"
	"go.uber.org/zap"
)

// Proxy is the central gateway that manages the RakNet listener, session pool,
// rate limiter, and middleware registry.
type Proxy struct {
	cfg        *config.Config
	listener   *minecraft.Listener
	middleware *middleware.Registry
	limiter    *ratelimit.Limiter
	log        logger.Logger

	pools    map[string]*pool.Pool
	mu       sync.RWMutex
	sessions map[string]*Session

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closed atomic.Int32
}

// New constructs a Proxy from the supplied config.
func New(cfg *config.Config, log logger.Logger) (*Proxy, error) {
	if len(cfg.Backends) == 0 {
		return nil, fmt.Errorf("proxy: at least one backend must be configured")
	}

	limiter := ratelimit.New(ratelimit.Config{
		Enabled:          cfg.RateLimit.Enabled,
		PacketsPerSecond: cfg.RateLimit.PacketsPerSecond,
		BurstSize:        cfg.RateLimit.BurstSize,
		CleanupInterval:  cfg.RateLimit.CleanupInterval,
	})

	ctx, cancel := context.WithCancel(context.Background())

	pools := make(map[string]*pool.Pool)
	for _, b := range cfg.Backends {
		if b.WarmPoolSize > 0 {
			pools[b.Name] = pool.New(ctx, b.Address, b.WarmPoolSize)
			log.Infow("Backend warm pool started",
				"backend", b.Name,
				"address", b.Address,
				"pool_size", b.WarmPoolSize,
			)
		}
	}

	return &Proxy{
		cfg:        cfg,
		middleware: middleware.NewRegistry(),
		limiter:    limiter,
		pools:      pools,
		log:        log,
		sessions:   make(map[string]*Session),
		ctx:        ctx,
		cancel:     cancel,
	}, nil
}

// Middleware returns the hook registry.
func (p *Proxy) Middleware() *middleware.Registry { return p.middleware }

// ListenAndServe opens the RakNet listener and enters the accept loop.
func (p *Proxy) ListenAndServe() error {
	packs, err := loadResourcePacks(p.cfg.Proxy.ResourcePacks)
	if err != nil {
		return fmt.Errorf("load resource packs: %w", err)
	}
	if len(packs) > 0 {
		p.log.Infow("Resource packs loaded", "count", len(packs))
	}

	listenCfg := minecraft.ListenConfig{
		MaximumPlayers:         p.cfg.Proxy.MaxPlayers,
		StatusProvider:         newStatusProvider(p.cfg.Proxy.MOTD, p.cfg.Proxy.MaxPlayers),
		AuthenticationDisabled: p.cfg.Proxy.AuthDisabled,
		TexturePacksRequired:   p.cfg.Proxy.TexturePacksRequired,
		ResourcePacks:          packs,
	}

	var listenErr error
	p.listener, listenErr = listenCfg.Listen("raknet", p.cfg.Proxy.Listen)
	if listenErr != nil {
		return fmt.Errorf("bind listener on %s: %w", p.cfg.Proxy.Listen, listenErr)
	}
	defer p.listener.Close()

	p.log.Infow("Proxy listening", "addr", p.cfg.Proxy.Listen)
	p.acceptLoop()
	return nil
}

// Shutdown initiates a graceful shutdown.
func (p *Proxy) Shutdown(ctx context.Context) error {
	if !p.closed.CompareAndSwap(0, 1) {
		return nil
	}

	p.log.Info("Proxy shutting down...")
	p.cancel()

	if p.listener != nil {
		_ = p.listener.Close()
	}
	p.limiter.Close()

	for name, pl := range p.pools {
		pl.Close()
		p.log.Debugw("Warm pool closed", "backend", name)
	}

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.log.Info("Proxy shut down cleanly.")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("proxy: graceful shutdown deadline exceeded: %w", ctx.Err())
	}
}

// Sessions returns a snapshot of the currently connected sessions.
func (p *Proxy) Sessions() map[string]*Session {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]*Session, len(p.sessions))
	for k, v := range p.sessions {
		out[k] = v
	}
	return out
}

// BackendNames returns the names of all configured backend servers.
func (p *Proxy) BackendNames() []string {
	names := make([]string, len(p.cfg.Backends))
	for i, b := range p.cfg.Backends {
		names[i] = b.Name
	}
	return names
}

// PlayerBackend returns the backend name the given session is connected to.
func (p *Proxy) PlayerBackend(sessionID string) (string, bool) {
	p.mu.RLock()
	sess, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return "", false
	}
	return sess.CurrentBackend(), true
}

// SendPlayerMessage delivers a system chat message to the player.
func (p *Proxy) SendPlayerMessage(sessionID, text string) error {
	p.mu.RLock()
	sess, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.SendMessage(text)
	return nil
}

// TransferPlayer moves the player to the named backend.
func (p *Proxy) TransferPlayer(sessionID, backendName string) error {
	p.mu.RLock()
	sess, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	backend, ok := p.cfg.BackendByName(backendName)
	if !ok {
		return fmt.Errorf("backend %q not found in config", backendName)
	}
	go sess.transferTo(backend)
	return nil
}

// acceptLoop runs until the listener is closed.
func (p *Proxy) acceptLoop() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if p.closed.Load() == 1 || errors.Is(err, net.ErrClosed) {
				return
			}
			p.log.Warnw("Accept error", zap.Error(err))
			continue
		}

		if p.closed.Load() == 1 {
			_ = conn.Close()
			return
		}

		mcConn := conn.(*minecraft.Conn)
		remoteAddr := mcConn.RemoteAddr().String()

		if !p.limiter.Allow(remoteAddr) {
			p.log.Warnw("Rate limit exceeded, dropping connection", "remote", remoteAddr)
			_ = p.listener.Disconnect(mcConn, "Too many connections.")
			continue
		}

		p.wg.Add(1)
		go p.handleConnection(mcConn)
	}
}

// handleConnection runs the full lifecycle for one incoming client.
func (p *Proxy) handleConnection(clientConn *minecraft.Conn) {
	defer p.wg.Done()

	remoteAddr := clientConn.RemoteAddr().String()
	defaultBackend := p.cfg.DefaultBackend()

	var serverConn *minecraft.Conn
	var err error
	if pl, ok := p.pools[defaultBackend.Name]; ok {
		serverConn, err = pl.Acquire(clientConn)
	} else {
		serverConn, err = fastDialBackend(clientConn, defaultBackend.Address)
	}
	if err != nil {
		p.log.Errorw("Failed to connect to backend",
			"backend", defaultBackend.Name,
			"remote", remoteAddr,
			zap.Error(err))
		_ = p.listener.Disconnect(clientConn, "Unable to connect to server. Try again later.")
		return
	}

	gameData := serverConn.GameData()
	if err := clientConn.StartGame(gameData); err != nil {
		p.log.Errorw("StartGame failed", "remote", remoteAddr, zap.Error(err))
		_ = clientConn.Close()
		_ = serverConn.Close()
		return
	}

	identityData := clientConn.IdentityData()
	clientData := clientConn.ClientData()
	sessID := uuid.New().String()

	info := middleware.PlayerInfo{
		SessionID:    sessID,
		Username:     identityData.DisplayName,
		XUID:         identityData.XUID,
		DeviceID:     string(clientData.DeviceID),
		SelfSignedID: identityData.Identity,
		RemoteAddr:   remoteAddr,
	}

	sessCtx, sessCancel := context.WithCancel(p.ctx)

	up := &Upstream{
		conn:    serverConn,
		name:    defaultBackend.Name,
		address: defaultBackend.Address,
	}
	sessLog := p.log.With("player", info.Username, "xuid", info.XUID)

	sess := newSession(sessID, p, clientConn, up, info, sessLog, sessCtx, sessCancel)

	p.addSession(sess)
	defer p.removeSession(sess.id)

	p.log.Infow("Player connected",
		"player", info.Username,
		"xuid", info.XUID,
		"backend", defaultBackend.Name,
		"remote", remoteAddr)

	p.middleware.FirePlayerJoin(info)
	sess.run()
	p.middleware.FirePlayerLeave(info)

	p.log.Infow("Player disconnected", "player", info.Username, "xuid", info.XUID)
}

func (p *Proxy) addSession(s *Session) {
	p.mu.Lock()
	p.sessions[s.id] = s
	p.mu.Unlock()
}

func (p *Proxy) removeSession(id string) {
	p.mu.Lock()
	delete(p.sessions, id)
	p.mu.Unlock()
}

// statusProvider implements minecraft.ServerStatusProvider.
type statusProvider struct {
	motd       string
	maxPlayers int
}

func newStatusProvider(motd string, maxPlayers int) *statusProvider {
	return &statusProvider{motd: motd, maxPlayers: maxPlayers}
}

func (sp *statusProvider) ServerStatus(playerCount, maxPlayers int) minecraft.ServerStatus {
	return minecraft.ServerStatus{
		ServerName:  sp.motd,
		PlayerCount: playerCount,
		MaxPlayers:  sp.maxPlayers,
	}
}

// fastDialBackend opens a connection to addr using the real player's identity.
func fastDialBackend(clientConn *minecraft.Conn, addr string) (*minecraft.Conn, error) {
	dialer := minecraft.Dialer{
		IdentityData: clientConn.IdentityData(),
		ClientData:   clientConn.ClientData(),
		DownloadResourcePack: func(_ uuid.UUID, _ string, _, _ int) bool {
			return false
		},
	}
	conn, err := dialer.Dial("raknet", addr)
	if err != nil {
		return nil, fmt.Errorf("dial backend %s: %w", addr, err)
	}
	return conn, nil
}

// loadResourcePacks reads each path and returns the resource pack slice.
func loadResourcePacks(paths []string) ([]*resource.Pack, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	packs := make([]*resource.Pack, 0, len(paths))
	for _, p := range paths {
		pack, err := resource.ReadPath(p)
		if err != nil {
			return nil, fmt.Errorf("read resource pack %q: %w", p, err)
		}
		packs = append(packs, pack)
	}
	return packs, nil
}
