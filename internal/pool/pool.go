// Package pool implements a per-backend connection pre-warm pool for the
// Bedrock proxy.
//
// # Problem
//
// When a player connects to the proxy, the proxy must dial the backend Dragonfly
// server and complete the full MCBE handshake before the player can enter the
// game. On the first dial this involves:
//
//   - RakNet UDP session establishment (1–3 round trips)
//   - MCBE Login / PlayStatus / ResourcePacksInfo exchange
//   - Optional resource pack metadata negotiation
//
// Even on localhost this can take hundreds of milliseconds. If the backend has
// resource packs configured the default gophertunnel Dialer downloads them,
// which can take 10–30 seconds.
//
// # Solution
//
// Pool maintains a fixed-size buffer of pre-dialed, authenticated
// *minecraft.Conn connections per backend. A background goroutine keeps the
// buffer full. When a player connects, Acquire() immediately returns a ready
// connection.
//
// Additionally, every Dialer created by this package sets
//
//	DownloadResourcePack: func(...) bool { return false }
//
// which instructs gophertunnel to skip downloading resource packs from the
// backend.
package pool

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
)

// Pool maintains a buffer of pre-dialed backend connections. Once created,
// it refills itself in the background until Close() is called.
type Pool struct {
	address string
	size    int
	warm    chan *minecraft.Conn

	// filling tracks how many fillOne goroutines are currently running,
	// preventing goroutine explosion when the backend is slow or down.
	filling atomic.Int32

	retryDelay    time.Duration
	maxRetryDelay time.Duration

	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a Pool for the given backend address and starts the background
// refill goroutine.
func New(ctx context.Context, address string, size int) *Pool {
	if size <= 0 {
		size = 2
	}
	poolCtx, cancel := context.WithCancel(ctx)
	p := &Pool{
		address:       address,
		size:          size,
		warm:          make(chan *minecraft.Conn, size),
		retryDelay:    2 * time.Second,
		maxRetryDelay: 30 * time.Second,
		ctx:           poolCtx,
		cancel:        cancel,
	}
	go p.fillLoop()
	return p
}

// Acquire returns a backend *minecraft.Conn authenticated with the real player's
// credentials. Falls back to a direct dial if the pool is empty.
func (p *Pool) Acquire(clientConn *minecraft.Conn) (*minecraft.Conn, error) {
	select {
	case warmConn := <-p.warm:
		go func() { _ = warmConn.Close() }()
		go p.fillOne()
	default:
	}

	conn, err := fastDial(clientConn, p.address)
	if err != nil {
		return nil, fmt.Errorf("acquire backend conn: %w", err)
	}
	return conn, nil
}

// Close stops the background refill goroutine and drains the warm channel.
func (p *Pool) Close() {
	p.cancel()
	for {
		select {
		case c := <-p.warm:
			_ = c.Close()
		default:
			return
		}
	}
}

// Len returns the number of pre-dialed connections currently available.
func (p *Pool) Len() int { return len(p.warm) }

// fillLoop keeps the warm channel full.
func (p *Pool) fillLoop() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}

		needed := p.size - len(p.warm) - int(p.filling.Load())
		for i := 0; i < needed; i++ {
			p.filling.Add(1)
			go p.fillOne()
		}
	}
}

// fillOne dials one warmup connection using a placeholder identity.
// The placeholder DisplayName is "w" + first 8 hex chars of a UUID (9 chars, ≤16 limit).
func (p *Pool) fillOne() {
	defer p.filling.Add(-1)

	warmID := uuid.New()
	identity := login.IdentityData{
		XUID:        "",
		Identity:    warmID.String(),
		DisplayName: "w" + warmID.String()[:8], // 9 chars — always valid
	}
	clientData := login.ClientData{}

	delay := p.retryDelay
	for {
		conn, err := warmDial(p.ctx, identity, clientData, p.address)
		if err == nil {
			select {
			case p.warm <- conn:
			default:
				_ = conn.Close()
			case <-p.ctx.Done():
				_ = conn.Close()
			}
			return
		}

		select {
		case <-p.ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < p.maxRetryDelay {
			delay *= 2
			if delay > p.maxRetryDelay {
				delay = p.maxRetryDelay
			}
		}
	}
}

// fastDial dials the backend with the real player's identity, skipping resource pack downloads.
func fastDial(clientConn *minecraft.Conn, address string) (*minecraft.Conn, error) {
	dialer := minecraft.Dialer{
		IdentityData: clientConn.IdentityData(),
		ClientData:   clientConn.ClientData(),
		DownloadResourcePack: func(_ uuid.UUID, _ string, _, _ int) bool {
			return false
		},
	}
	conn, err := dialer.Dial("raknet", address)
	if err != nil {
		return nil, fmt.Errorf("fast-dial %s: %w", address, err)
	}
	return conn, nil
}

// warmDial dials the backend with a placeholder identity for pre-warming.
func warmDial(ctx context.Context, identity login.IdentityData, clientData login.ClientData, address string) (*minecraft.Conn, error) {
	dialer := minecraft.Dialer{
		IdentityData: identity,
		ClientData:   clientData,
		DownloadResourcePack: func(_ uuid.UUID, _ string, _, _ int) bool {
			return false
		},
	}

	type result struct {
		conn *minecraft.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, e := dialer.Dial("raknet", address)
		ch <- result{c, e}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}
