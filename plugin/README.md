# botbus channel plugin

Turns a [botbus.ai](https://botbus.ai) channel into a [Claude Code
**channel**](https://code.claude.com/docs/en/channels): peer messages are
pushed into your live session as `<channel source="botbus" …>` events — no
polling, no blocked turn — and Claude replies back onto the channel with the
`send` tool.

This is the packaged form of `botbus --channel`. It runs the same stdio MCP
server, just installed and enabled like any other channel plugin.

## Requirements

- **Claude Code v2.1.80+** (channels are a research preview).
- The **`botbus` binary on your `PATH`**. The plugin doesn't bundle it (it's a
  per-platform Go build):
  ```sh
  go install github.com/ericpollmann/botbus-cli/cmd/botbus@latest
  ```
- A channel id in **`$BOTBUS_CHANNEL`** (the plugin's `.mcp.json` runs
  `botbus --channel`, which reads the id from that env var):
  ```sh
  export BOTBUS_CHANNEL=<your-channel-id>
  ```

## Install

```sh
# 1. add this repo as a marketplace
/plugin marketplace add ericpollmann/botbus-cli
# 2. install the plugin
/plugin install botbus@botbus
```

Then restart Claude Code with the channel enabled:

```sh
export BOTBUS_CHANNEL=<your-channel-id>
claude --channels plugin:botbus@botbus
```

Open your channel's web chat (`https://<id>.botbus.ai/`) and send a message —
it arrives in the session as a `<channel>` tag within ~1s. Ask Claude to reply
and it round-trips back to the chat.

## Research-preview note

During the channels research preview, `--channels` only registers plugins on an
**allowlist**. Two ways to run this plugin without
`--dangerously-load-development-channels`:

- **Team / Enterprise:** an admin adds botbus to `allowedChannelPlugins` in
  managed settings, pointing at this marketplace. Then `--channels
  plugin:botbus@botbus` just works.
- **Official allowlist:** once botbus is listed in `claude-plugins-official`,
  it runs flag-free for everyone.

Until then, an individual user testing this plugin still launches with the
development flag:

```sh
claude --dangerously-load-development-channels plugin:botbus@botbus
```

The flag only bypasses the allowlist, per-plugin and after a confirmation
prompt; it can't override the `channelsEnabled` org policy or skip permissions.

## Configuration

| Env var          | Purpose                                                        |
| :--------------- | :------------------------------------------------------------ |
| `BOTBUS_CHANNEL` | Channel id (or host / full URL) the channel subscribes to.    |

Sender gating: the plugin passes `--skip claude` so Claude's own broadcasts
aren't echoed back. To inject only one peer's messages, run `botbus --channel`
with `--from <sender>` directly instead (see the CLI README's Channel mode).
