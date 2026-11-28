# Linux VM Production & Low-Memory Tuning

This fork (`flipped0119/magpie`) adds AsterGate-inspired production hardening and
Linux low-memory controls on top of upstream Magpie. All new controls preserve
upstream defaults unless configured (except safe bug fixes and zero-cost
`cache_control` hygiene).

## Environment variables

| Variable | Default | Effect |
| --- | --- | --- |
| `MAGPIE_LOW_MEMORY` | `off` | Enables Linux low-memory profile (`1`/`true`/`yes`/`on`): sets Go soft memory limit to 20% of cgroup `memory.max` or `MemTotal` (clamped to `256–1024 MiB`) and `GOGC=50` when `GOMEMLIMIT`/`GOGC` are unset; caps idle Claude subprocesses to `2` (`5m` idle TTL with automatic session shelving to preserve Anthropic 1h prompt cache), parked tool-waiting runs to `6`, per-run RSS recycle floor to `384 MiB` (down from `1 GiB`), Codex inline image decode budget to `128 MiB` (down from `512 MiB`), and usage price-block linger to `5s` (down from `30s`). |
| `MAGPIE_MEM_LIMIT_MB` | `0` | Explicit Go soft memory limit in MiB (ignored if `GOMEMLIMIT` is set). Recommended `512` on 4–8 GiB VMs so child CLIs (`claude`, `agy`) keep the remaining RAM. |
| `MAGPIE_CLAUDE_IDLE_MAX` | `6` (`2` in low-mem) | Max idle Claude Code subprocesses kept resident. |
| `MAGPIE_CLAUDE_PARKED_MAX` | `16` (`6` in low-mem) | Max tool-waiting Claude Code subprocesses kept parked. |
| `MAGPIE_CLAUDE_IDLE_TTL` | `1h` (`5m` in low-mem) | Idle subprocess lifetime before shelving session ID for `--resume`. |
| `MAGPIE_CLAUDE_RUN_RSS_MB` | `1024` (`384` in low-mem) | Per-subprocess RSS threshold (MiB) to recycle and shelve after a turn. |
| `MAGPIE_EGRESS_REQUIRE_PROXY` | `off` | Fail-closed egress guard: refuses any non-loopback, non-LAN direct dial (`ErrDirectEgressRefused`), forces `DeadProxy` on child CLIs lacking a proxy, and upgrades exported `socks5://` to `socks5h://` to prevent local DNS leaks. |
| `MAGPIE_FORCE_SOCKS5H` | `off` | Upgrades exported `socks5://` to `socks5h://` for child CLIs even without `MAGPIE_EGRESS_REQUIRE_PROXY`. |
| `MAGPIE_CLAUDE_RANDOM_GATE` | `""` | Deterministic per-account, per-window randomized Claude stop line (e.g. `93-97` or `85-95`, FNV-64a over `(account, window, resetHour)`). Explicit per-account/per-window caps (and `100` exemptions) always take precedence. |
| `MAGPIE_AFFINITY_DETOUR_HOLD` | `on` | Keeps warm session bindings intact during brief non-quota detours (`<=5m` Claude / `<=10m` Codex) or lane saturation (`laneMate`); set `off` to disable. |
| `MAGPIE_CLAUDE_CONTEXT_BREAKPOINT` | `on` | Byte-splices a `1h` `cache_control` breakpoint onto the last leading `<system-reminder>` block in `messages[0].content` when all existing markers are `1h` and count is `1–3` so parallel subagents share workspace context cache; set `off` to disable. |
| `MAGPIE_ANTHROPIC_OUTBOUND_GZIP` | `auto` | Outbound gzip compression for Anthropic `/v1/messages` and `count_tokens` bodies `>= minBytes` (`auto`: official Anthropic hosts; `all`: also third-party relays; `off`: disabled). Automatically retries uncompressed on 415, non-origin 403, or 400/501 encoding rejection and latches the provider. |
| `MAGPIE_ANTHROPIC_OUTBOUND_GZIP_LEVEL` | `6` | Outbound gzip compression level (`1`–`9`; `3` for lower CPU, `9` for maximum traffic savings). |
| `MAGPIE_RESPONSES_OUTBOUND_ZSTD` | `auto` | Outbound zstd compression for OpenAI `/v1/responses` bodies `>= minBytes` outside ChatGPT Codex (`auto`: official `api.openai.com`; `all`: also third-party relays such as AsterGate; `off`: disabled). Automatically retries uncompressed on rejection and latches the provider. |
| `MAGPIE_RESPONSES_OUTBOUND_ZSTD_LEVEL` | `default` | Outbound zstd level (`fastest`/`default`/`better`/`best` or `1`–`19`; default `SpeedDefault` ≈ level 3). |
| `MAGPIE_OUTBOUND_COMPRESS_MIN_BYTES` | `2048` | Minimum uncompressed request body size (bytes) before outbound gzip/zstd compression is applied. |

## Linux VM `systemd` baseline (4–8 GiB RAM)

```ini
[Service]
Type=simple
EnvironmentFile=%h/.config/magpie/env
ExecStart=%h/.local/bin/magpie serve --web=127.0.0.1:3430
Restart=always
RestartSec=3
TimeoutStopSec=300
KillMode=mixed
LimitNOFILE=65536
MemoryHigh=5.5G
MemoryMax=7G
```

Recommended `~/.config/magpie/env` additions:

```bash
MAGPIE_LOW_MEMORY=1
MAGPIE_MEM_LIMIT_MB=512
MAGPIE_EGRESS_REQUIRE_PROXY=1
MAGPIE_CLAUDE_RANDOM_GATE=93-97
```

## Compatible endpoints

- `GET /me/usage` and `GET /v1/me/usage`: AsterGate-compatible self-usage payload (`accounts[]`, `windows[].used_ratio` in `0..1`, `current` resolved from `X-Claude-Code-Session-Id`) usable directly by `astergate-cc-mods` `statusline` and `cache-keeper`.
- `GET /managed/settings` (ETag + `304`) and `POST /api/event_logging[/batch|/v2/batch]` (`200 OK`): zero-404 Claude Code CLI control surface.
