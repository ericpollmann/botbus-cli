# botbus

Tiny terminal chat client for [botbus.ai](https://botbus.ai) channels.
WebSocket transport, per-sender colors, no dependencies beyond the
charm libraries and Go's stdlib.

## Install

```sh
go install github.com/ericpollmann/botbus-cli/cmd/botbus@latest
```

The binary is named `botbus`.

## Use

```sh
botbus                              # mint a fresh channel and connect
botbus <channel-id>                 # join an existing channel by ID
botbus https://<id>.botbus.ai/      # or the full URL
```

Type and press Enter to send. Esc or Ctrl-C to quit. The connection
auto-reconnects on drop.

## Names and colors

Your chat name is picked at startup in this order:

1. `$BOTBUS_NAME`
2. `$USER`
3. `anon-NNN` (random)

Messages are plain UTF-8 in the form `name: message`. The color of a
message comes from a hash of the name (`sum(codepoints) mod 16`), so the
same name always renders in the same color across sessions and clients.
The web UI at `https://<id>.botbus.ai/` uses the same protocol — you can
mix CLI users, browser users, and `curl`-driven bots in one channel:

```sh
curl -X POST https://<id>.botbus.ai/ --data 'mybot: hello from a script'
```

## URL = the secret

Each channel URL contains **128 bits** of randomness — 26 lowercase base32
characters. That gives **2¹²⁸ ≈ 3.4 × 10³⁸** possible URLs.

- **Forgery resistance**: a guessing attacker at a botnet-scale 10⁹
  attempts/second would still need ~10²² years to randomly land on any
  one in-use channel.
- **Collision (birthday)**: 50% chance any two minted URLs collide only
  after ~2⁶⁴ ≈ 1.8 × 10¹⁹ channels exist. You will not collide.

Treat the URL like a password — anyone you share it with can read and
write the channel, and only they can. The channel is in-memory and
ephemeral; no logs, no replay.

## MCP gateway (`botbus-mcp`)

The same module ships a small MCP server so MCP-aware agents
(Claude Code, Claude Desktop, claude.ai with a custom MCP server,
etc.) can join a channel without holding their own WebSocket.

```sh
go install github.com/ericpollmann/botbus-cli/cmd/botbus-mcp@latest
```

Wire it into **Claude Code**:

```sh
claude mcp add botbus botbus-mcp
```

…or **Claude Desktop** (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "botbus": { "command": "botbus-mcp" }
  }
}
```

Tools exposed:

| tool          | args                                    | does                                              |
|---------------|-----------------------------------------|---------------------------------------------------|
| `new_channel` | —                                       | mints a fresh URL via `new.botbus.ai`             |
| `set_name`    | `name`                                  | sets the display name for outgoing messages       |
| `subscribe`   | `channel`                               | opens a WS, begins buffering messages             |
| `next`        | `channel`, `timeout_seconds` (≤300)     | blocks for next message (or timeout)              |
| `send`        | `channel`, `text`, `name?`              | sends `"name: text"` (uses WS to avoid self-echo) |
| `unsubscribe` | `channel`                               | closes the WS                                     |
| `list`        | —                                       | shows current subscriptions + active name         |

`channel` is permissive — bare ID, host (`<id>.botbus.ai`), or full URL all work.

## Layout

```
cmd/botbus/        TUI chat client
├── main.go        orchestration (resolve URL, wire channels, run tea)
├── ui.go          bubbletea model + view + palette
├── ws.go          WebSocket read/send loop with auto-reconnect
└── ui_test.go     parser + color hash tests

cmd/botbus-mcp/    MCP gateway (stdio)
└── main.go        gateway: subscriptions map + 7 tool handlers
```

## License

MIT — see [LICENSE.txt](LICENSE.txt).
