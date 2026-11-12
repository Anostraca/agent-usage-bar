# claude-usage-bar

A [SwiftBar](https://github.com/swiftbar/SwiftBar) plugin that shows your Claude and Codex subscription
limits in the macOS menu bar. You get one small battery per Claude Code or Codex account, and it drains
as you use the account.

Not affiliated with Anthropic or OpenAI. See [Disclaimer](#disclaimer).

## What it shows

```
 W ╭━━━━━━━━━━┄┄┄┄╮       frame: weekly limit left (lit part, from the left)
   ┃ ███████      ┊▌      cell:  5-hour session limit left
   ╰━━━━━━━━━━┄┄┄┄╯       nub:   lit while the week is untouched
 ^
 account initial
```

- **Frame**: what is left of the 7-day limit. The lit part shrinks from the right as the week is used.
- **Cell**: what is left of the 5-hour session limit. A session that hasn't started shows a full cell.
- **Colours** follow usage: amber at 75 % used, red at 90 % used, so a nearly empty battery turns
  amber, then red.
- **Plans without a 5-hour limit**: the whole battery drains with the week. The frame and the cell
  both show what is left of it.
- **Faded** battery: the data is stale, e.g. the API failed or the login token has expired.
  **Outline with "?"**: no data yet.

Click the icon for details on each account:

```
work · team 5x · you@example.com
5h    █████████████ 100% left   idle · starts with next message
week  ████████░░░░░  65% left   resets Sat 20:59 (in 5d 4h)
Fable █████████████ 100% left   resets Sat 21:00 (in 5d 4h)
● 3 sessions · 2 busy, 1 idle          ▸ name, folder, status
Today   522M tok · 3.1M out · 2.7k msgs · cache 98%
7 days  2.8B tok · 13M out · 12.6k msgs
today ▂▁▁▁▁▁▂▁▁▂▂▃▆█▄▂▂                 output tokens per hour
Top projects today                     ▸
Models · 7 days                        ▸
```

The limit rows come from Anthropic's usage API, with a row for each per-model weekly limit. Sessions
and token numbers come from the files Claude Code keeps on your Mac.

Codex accounts (the ChatGPT login of the Codex CLI or app) show the same block. The rows come from
ChatGPT's usage API and are labelled by window length (`5h`, `week`), with extra rows for code review
and other limits when the plan has them. Token numbers come from Codex's session logs. There is no
running-sessions line for Codex.

Each account ends with a **Show in menu bar** item. Untick it to keep that account in the dropdown
only. With every account hidden, the menu bar shows the text `AI`.

## Requirements

- macOS (the login is read from the Keychain)
- [SwiftBar](https://github.com/swiftbar/SwiftBar)
- [Claude Code](https://docs.claude.com/en/docs/claude-code) logged in with a Claude Pro, Max or Team
  subscription (not an API key), and/or
- [Codex](https://developers.openai.com/codex) logged in with a ChatGPT plan (`codex login`, not an API
  key)

## Install

A. With Go 1.27 or newer:

```sh
go install github.com/Anostraca/claude-usage-bar@latest
claude-usage-bar --install-plugin
```

B. Release binary: download `claude-usage-bar-<version>-darwin-<arm64|amd64>` from the releases page,
then:

```sh
chmod +x claude-usage-bar-*-darwin-*
mv claude-usage-bar-*-darwin-* ~/.local/bin/claude-usage-bar
xattr -d com.apple.quarantine ~/.local/bin/claude-usage-bar 2>/dev/null
~/.local/bin/claude-usage-bar --install-plugin
```

C. From source:

```sh
git clone https://github.com/Anostraca/claude-usage-bar && cd claude-usage-bar
make install        # builds, installs to ~/.local/bin, writes the SwiftBar plugin
make test
```

`--install-plugin` writes `claude-usage-bar.1m.sh` into SwiftBar's plugin folder. It finds the folder
with `defaults read com.ameba.SwiftBar PluginDirectory` and falls back to `~/SwiftBar`. SwiftBar then
runs the plugin every minute. If you move the binary, run `--install-plugin` again.

## Config

Optional: `~/.config/claude-usage-bar/config.json` (or `$XDG_CONFIG_HOME/claude-usage-bar/config.json`).
List one entry per login:

```json
{
  "accounts": [
    {"name": "work", "config_dir": "~/.claude"},
    {"name": "personal", "config_dir": "~/.claude-personal", "bar": false},
    {"name": "codex", "provider": "codex"}
  ]
}
```

- `provider`: `claude` (the default) or `codex`. Entries with another provider are skipped.
- `config_dir`: the `CLAUDE_CONFIG_DIR` (or `CODEX_HOME`) that login uses. Empty means `~/.claude` or
  `~/.codex`. Relative paths are skipped.
- `bar`: `false` shows the account in the dropdown only. The default is `true`.

Without the file you get `claude` with `~/.claude`, plus `codex` with `~/.codex` when
`~/.codex/auth.json` exists. The first letter of each name appears in the menu bar.

The **Show in menu bar** item runs `--toggle-bar`, which rewrites this file: it sets `"bar": false` or
removes the key, and keeps everything else. Without a file it writes the default accounts. A file that
isn't valid JSON is left alone.

## Data sources

- **Login**: the OAuth access token Claude Code keeps in the Keychain item `Claude Code-credentials`.
  Other config dirs use `Claude Code-credentials-<first 8 hex chars of sha256(config_dir)>`. If the
  Keychain has nothing, it reads `<config_dir>/.credentials.json`.
- **Limits**: `GET https://api.anthropic.com/api/oauth/usage`, at most every 3 minutes per account.
- **Email and plan**: `~/.claude.json` (default dir) or `<config_dir>/.claude.json`, and the Keychain
  item.
- **Sessions**: `<config_dir>/sessions/<pid>.json`, only for processes that are still running.
- **Tokens**: assistant-message usage numbers from `<config_dir>/projects/**/*.jsonl` changed in the
  last 7 days. These are recomputed at most every 2 minutes, and each run only reads lines added
  since the last one.
- **Codex login**: `tokens.access_token` and `tokens.account_id` from `<config_dir>/auth.json`. The
  expiry, email and plan are read from the token's claims (the signature is not checked).
- **Codex limits**: `GET https://chatgpt.com/backend-api/wham/usage`, at most every 3 minutes per
  account.
- **Codex tokens**: `token_count` events in `<config_dir>/sessions/YYYY/MM/DD/rollout-*.jsonl`, with
  the model and folder of the latest turn. Same 7 days and incremental reads as above.

Caches live in `~/Library/Caches/claude-usage-bar/`.

Flags: `--force` (fetch now, print nothing), `--png FILE` (write the menu bar image), `--json` (dump
the parsed state), `--toggle-bar ID` (show or hide an account in the menu bar), `--install-plugin`,
`--version`.

## Privacy

- Tokens are read-only from the Keychain (or Claude Code's `.credentials.json` fallback) and Codex's
  `auth.json`, never stored, logged or refreshed. Refresh tokens are never used, and nothing is ever
  written to the Keychain or `auth.json`. When a token has expired, open `claude` or `codex` on that
  account and it renews the login itself.
- Transcripts are only read for usage numbers: token counts, model name, time, and the base name of
  the working folder. Message text is never stored or shown.
- Caches hold usage percentages, reset times, token counts, model and folder names. No tokens, no
  message content.
- Network requests go only to `api.anthropic.com` and `chatgpt.com`.
- The config file is only written by the **Show in menu bar** item.

## Disclaimer

`/api/oauth/usage` and `/backend-api/wham/usage` are undocumented, unofficial endpoints. Claude Code and
Codex use them, but they may change or disappear at any time. This project is not affiliated with or
endorsed by Anthropic or OpenAI.

## License

MIT
