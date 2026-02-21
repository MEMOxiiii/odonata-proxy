// Package ratelimit provides a per-IP token-bucket rate limiter.
package ratelimit

import (
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Config controls rate-limiting behaviour.
type Config struct {
	Enabled          bool
	PacketsPerSecond float64
	BurstSize        int
	CleanupInterval  time.Duration
}

// Limiter manages per-IP token buckets.
type Limiter struct {
	cfg     Config
	mu      sync.Mutex
	buckets map[string]*entry
	stopCh  chan struct{}
}

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// New constructs a Limiter and starts the background cleanup goroutine.
func New(cfg Config) *Limiter {
	l := &Limiter{
		cfg:     cfg,
		buckets: make(map[string]*entry),
		stopCh:  make(chan struct{}),
	}
	go l.janitor()
	return l
}

// Allow returns true if the packet from addr may be processed.
func (l *Limiter) Allow(addr string) bool {
	if !l.cfg.Enabled {
		return true
	}
	ip := extractIP(addr)
	l.mu.Lock()
	e, ok := l.buckets[ip]
	if !ok {
		e = &entry{
			limiter: rate.NewLimiter(
				rate.Limit(l.cfg.PacketsPerSecond),
				l.cfg.BurstSize,
			),
		}
		l.buckets[ip] = e
	}
	e.lastSeen = time.Now()
	lim := e.limiter
	l.mu.Unlock()
	return lim.Allow()
}

// Close stops the background janitor goroutine.
func (l *Limiter) Close() {
	select {
	case <-l.stopCh:
	default:
		close(l.stopCh)
	}
}

// Len returns the number of tracked IP addresses.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func (l *Limiter) janitor() {
	interval := l.cfg.CleanupInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.evictOld(interval)
		case <-l.stopCh:
			return
		}
	}
}

func (l *Limiter) evictOld(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, e := range l.buckets {
		if e.lastSeen.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
}

func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
