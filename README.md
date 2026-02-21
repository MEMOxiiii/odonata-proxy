# Odonata Proxy

> A high-performance, extensible **Minecraft Bedrock Edition** proxy written in Go.  
> Built on [gophertunnel](https://github.com/sandertv/gophertunnel) and [go-raknet](https://github.com/sandertv/go-raknet).

---

## Features

- **Zero loading-screen server switching** — players are silently moved between backends without the "Generating World / Building terrain" dialog
- **Middleware / Hook system** — intercept join, leave, packets, commands, and transfers without touching core code
- **Per-IP rate limiting** — token-bucket limiter with configurable cleanup
- **Connection pre-warm pool** — optional pre-dialed backend connections for near-zero connection latency
- **Resource pack forwarding** — serve `.mcpack` files from the proxy layer, not individual backends
- **Structured logging** — console + JSON output via [go.uber.org/zap](https://github.com/uber-go/zap)
- **Graceful shutdown** — waits for all sessions to close before exiting

---

## Requirements

- Go 1.24+
- One or more [Dragonfly](https://github.com/df-mc/dragonfly) backend servers with **authentication disabled** on each backend

---

## Quick start

```bash
# Clone
git clone https://github.com/MEMOxiiii/odonata-proxy.git
cd odonata-proxy

# Download dependencies
go mod tidy

# Edit config
cp config.yaml config.yaml   # already present — edit as needed

# Build
go build -o odonata-proxy.exe .

# Run
./odonata-proxy.exe
```

---

## Configuration (`config.yaml`)

```yaml
proxy:
  listen: "0.0.0.0:19132"    # UDP address the proxy binds to
  motd: "§6Odonata §7Proxy"  # Server list name
  max_players: 1000
  auth_disabled: true         # Disable Xbox Live auth at the proxy
  texture_packs_required: false
  resource_packs: []          # Paths to .mcpack files

backends:
  - name: lobby
    address: "127.0.0.1:19133"
    default: true             # First server players connect to
    warm_pool_size: 0         # Pre-dialed connections (0 = disabled)

  - name: survival
    address: "127.0.0.1:19134"
    default: false
    warm_pool_size: 0

rate_limit:
  enabled: true
  packets_per_second: 400
  burst_size: 600
  cleanup_interval: 30s

logging:
  level: info        # debug | info | warn | error
  format: console    # console | json
  output_file: ""    # optional log file path
```

---

## Hook system

Implement `middleware.Hook` (embed `middleware.NoopHook` to skip unused methods) and register before starting the proxy:

```go
type ChatLogger struct{ middleware.NoopHook }

func (h *ChatLogger) OnPlayerJoin(info middleware.PlayerInfo) {
    fmt.Printf("%s joined (%s)\n", info.Username, info.RemoteAddr)
}

func main() {
    cfg, _ := config.Load("config.yaml")
    log  := logger.Must(logger.Config{Level: "info", Format: "console"})
    px, _ := proxy.New(cfg, log)

    px.Middleware().Register(&ChatLogger{})

    log.Info("Odonata Proxy starting")
    if err := px.ListenAndServe(); err != nil {
        log.Fatal(err)
    }
}
```

### Available hook methods

| Method | Fired when |
|--------|-----------|
| `OnPlayerJoin(PlayerInfo)` | Player finishes login |
| `OnPlayerLeave(PlayerInfo)` | Player disconnects |
| `OnPacketReceive(*PacketContext)` | Client → backend packet (set `ctx.Blocked = true` to drop) |
| `OnPacketSend(*PacketContext)` | Backend → client packet |
| `OnCommand(*CommandContext)` | Player sends a `/command` (set `ctx.Handled = true` to consume) |
| `OnServerTransfer(*TransferContext)` | Server switch begins |

---

## Switching servers

```go
// From inside a hook (you have access to px via closure):
px.TransferPlayer(info.SessionID, "survival")

// Block same-server transfer in OnCommand:
func (h *MyHook) OnCommand(ctx *middleware.CommandContext) {
    parts := strings.Fields(ctx.Command)
    if len(parts) < 2 || parts[0] != "server" { return }
    target := parts[1]
    if current, _ := px.PlayerBackend(ctx.Player.SessionID); current == target {
        px.SendPlayerMessage(ctx.Player.SessionID, "§cAlready on §e"+target+"§c!")
        ctx.Handled = true
        return
    }
    px.TransferPlayer(ctx.Player.SessionID, target)
    ctx.Handled = true
}
```

---

## Project structure

```
odonata-proxy/
├── main.go                        # Entry point & hook registration
├── config.yaml                    # Default configuration
├── internal/
│   ├── config/config.go           # YAML config loader & validator
│   ├── middleware/middleware.go   # Hook interface & registry
│   ├── pool/pool.go               # Connection pre-warm pool
│   ├── proxy/
│   │   ├── proxy.go               # Core engine
│   │   ├── session.go             # Per-player session & forwarding
│   │   ├── transfer.go            # Zero-loading-screen server switch
│   │   └── upstream.go            # Backend connection wrapper
│   └── ratelimit/ratelimit.go     # Per-IP token-bucket limiter
└── pkg/
    └── logger/logger.go           # Zap SugaredLogger wrapper
```

---

## License

MIT — see [LICENSE](LICENSE).
