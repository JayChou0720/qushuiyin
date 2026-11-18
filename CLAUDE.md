# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) and other coding agents (`AGENTS.md` points here) when working with code in this repository.

## Project status

A WeChat mini-program that parses share links from Douyin, Xiaohongshu, Kuaishou and Bilibili to get the platform's own video, image-set, cover and caption, then saves them to the user's album. "去水印" here means resolving the media source. It does **not** mean pixel inpainting or removing logos and subtitles burned into the video.

No real code exists yet. `qushuiyin_miniapp/` is the untouched WeChat TS quickstart. `qushuiyin_service/` contains only `doc/` (the Go backend is built from scratch per spec V1.1). The repository root is the single git repo; the old per-folder repos and the obsolete Spring Boot template were moved to `D:\Projects\去水印-backup\` (don't touch it). Remote: `https://github.com/JayChou0720/qushuiyin.git` (branch main, reached through the repo-level proxy `127.0.0.1:7897`). Commit locally freely; push only when the owner asks.

## Spec documents (read before implementing anything)

- `03-去水印小程序-开发执行说明.md`: **how to execute**: repo layout, Go/frontend conventions and chosen libraries, local dev (dev login stub, `PARSE_MODE=mock`), the `qushuiyin-server` subcommands, execution order, owner-only steps H1–H7, and `open-questions.md` usage.
- `01-去水印小程序-功能大纲与开发路线.md`: scope, feature IDs (F01–F20 user-facing, B01–B16 backend), page list, milestones M0–M5, work packages T01–T17.
- `02-去水印小程序-功能说明与开发规格.md`: the detailed spec. **If it conflicts with doc 01, doc 02 wins.** Key chapters:
  - §1.2 fixed tech decisions, §2.2 platform capability matrix, §3 routes
  - §11 state machines and idempotency, §12 parser library integration and unified result model
  - §13 media preparation, tickets and Range, **§13.7 download mode (direct-first, relay fallback)**, §14 HTTP API contract, §15 MySQL schema, §15.6 in-process state (replaces Redis)
  - §16 config defaults, the **single source of every numeric default**. Do not hardcode limits, timeouts or TTLs in pages or endpoints.
  - §18 error codes and event names, §20 acceptance cases, §21.1 code layout
- All three docs are at V1.1. `qushuiyin_service/doc/` holds CRLF copies of 01 and 02 plus `watermark-miniapp-prototype.html` (a UI prototype). The root files are LF. After editing a root doc, regenerate its copy (for example `sed 's/$/\r/' <file> > qushuiyin_service/doc/<file>`) and check with `diff --strip-trailing-cr`.

When behaviour changes, update the functional rule, API contract, config and tests together (spec §22.3). Tag new requirements P0, P1 or P2.

## Stack (spec §1.2, §19.1, §21.1)

| Part | Choice |
| --- | --- |
| Mini-program | Native WeChat mini-program, TypeScript (`qushuiyin_miniapp/`) |
| Backend | One Go service compiled to a single binary (`qushuiyin_service/`, packages under `internal/{auth,parse,media,feedback,admin}`) |
| Parsing | [parse-video](https://github.com/wujunwei928/parse-video) Go library, imported as a module pinned to commit `00e8e11`. No separate parse service |
| Admin | `/admin/v1` API (own admin accounts, argon2id, cookie sessions, roles; spec §14.13) + Vue 3 + Element Plus frontend in `qushuiyin_service/web/admin/`, built to `qushuiyin_service/internal/admin/dist` and embedded (go:embed cannot reach outside the package dir) |
| Storage | MySQL only (tasks, results, batches, feedback, sessions, daily usage counters; task queue = DB + bounded worker pool). **No Redis in V1**: session cache, parse cache, rate limits, circuit breakers, transfer leases and ticket revocations live in process memory with caps (spec §15.6), so the service must run as a single instance |
| Media | Mostly direct download from platform CDNs (see invariants). Relayed files sit in a temp directory and are served through signed short-lived tickets; they are shared across users (`media_file`), with a per-user grant (`media_object`) and per-user tickets (spec §13.2). `ffprobe` (external process) validates relayed files; WebP conversion happens in-process |

V1 platforms: Douyin, Xiaohongshu and Kuaishou (video + image sets) and Bilibili (video only: low-quality single-file MP4, first part only). Bilibili HD (DASH + FFmpeg merge) and yt-dlp as a second source are P1. No platform account cookies in V1.

Target server: 2 cores / 2 GB / 50 GB SSD with MySQL on the same box (tuning and memory estimate in spec §19.2). Bandwidth is sized by relay traffic, not total downloads. Local toolchain: MySQL 8.4, Node 24 and WeChat DevTools are installed. Go 1.27.4 is at `D:\Go` (GOPROXY=https://goproxy.cn,direct) and FFmpeg 9.0.2 with ffprobe at `D:\FFmpeg\bin`; both are on the user PATH (H3 done; new shells/agents must be restarted to see PATH). Windows Smart App Control is off so unsigned Go binaries can run.

## Rules for the parse-video dependency (spec §12.1)

- **Never modify upstream source.** No fork and no `replace` directive pointing at patched code. Upgrading means bumping the pinned version in `go.mod` and re-running the per-platform sample regression.
- Only `internal/parse` may import `github.com/wujunwei928/parse-video/parser`. Call `ParseVideoShareUrlContext(ctx, url)` with a task-deadline context. Never use upstream's `cmd` package (Gin server, CLI, downloader).
- Our adapter decides the platform first and only passes enabled platforms to the library. Upstream errors are mostly untyped strings, so map only what can be detected (unsupported URL, context timeout or cancel) and treat the rest as parse failure. Raw error text goes only to redacted server logs.
- Upstream returns few fields (no content ID, no duration, title and caption merged). §12.1 defines the mapping and which fields are null.
- Upstream's outbound limits (domain allowlist, dial-time private-IP rejection, 16 MiB response cap, 30 s parse timeout, 10 redirects) must not be bypassed. Leave `PARSE_VIDEO_DOUYIN_COOKIE` unset.
- If a platform breaks upstream and must be fixed before upstream does, swap in our own implementation for that platform inside the adapter. Record the swap and remove it after upgrading.

## Mini-program commands

- Source root is `qushuiyin_miniapp/miniprogram/` (see `project.config.json`). TS is compiled by WeChat DevTools' built-in `typescript` plugin. There is no npm build, lint or test script.
- Build, preview and upload through WeChat DevTools. Type-check from the CLI with `npx tsc --noEmit -p qushuiyin_miniapp` (run `npm install` first for `miniprogram-api-typings`). tsconfig is strict, with `noUnusedLocals` and `noUnusedParameters` enabled.
- Uses `glass-easel` with Skyline rendering enabled and `lazyCodeLoading: requiredComponents`.
- Target routes (spec §3.1): `pages/home/index`, `pages/result/index`, `pages/history/index`, `pages/help/index`, `pages/feedback/{create,list,detail}`, `pages/privacy/index`. Single home page, no TabBar.
- `pages/` holds UI state only. `services/` holds api, session, privacy, download, album, history and telemetry. Platform parsing rules never live in the client.

## Invariants that run through the whole spec

- **Downloads are direct-first, relay as fallback (spec §13.7).** Per item, the server decides: **direct** if the final CDN host (after following redirects server-side) is `registered` in `download_domain` and a light probe passes; otherwise a per-batch bounded re-parse, then **relay** through our own domain (size-capped), otherwise the item FAILS with `copyLinkAvailable`. Upstream URLs reach the client through exactly **three outlets**: direct download URL (tickets), direct preview URL (preview-tickets, behind `direct_preview_enabled`) and copy-link. Each goes only to the owner, one URL per resource (at most `max_image_batch_count` per call), and is rate-limited. None of them appears in results, batch queries, page params, local history, events or logs. URLs needing cookies or custom headers never leave the server. A failed direct download goes through the `fallback` endpoint, at most once per item. The server never accepts arbitrary user URLs (it is not an open proxy), and each redirect hop is validated.
- **Success is staged:** parsed ≠ prepared ≠ downloaded ≠ saved to album. Report each stage separately in UI state, metrics and errors.
- **Capabilities are switched per platform and per content type** (`status` plus `videoEnabled`, `imageEnabled`, `coverEnabled` and `textEnabled`). Unverified capabilities default to off and are enforced server-side, not just hidden in the UI.
- **Concurrency is bounded everywhere:** parse workers, media workers, goroutines and connections all have limits from §16. Large-file transfer must not starve ordinary API handling.
- **API envelope** (§14.1): `{code, message, requestId, data, error?}`. `code=OK` means success; error codes are stable English strings with Chinese `message`. All IDs are strings, times are RFC 3339 UTC, sizes are in bytes, durations are in ms. Pagination uses cursors (default 20, max 50). Admin endpoints live under `/admin/v1` with separate admin auth.
- **DB conventions** (§15.1): `id VARCHAR(64)` holds random opaque IDs, never auto-increment values used for authorization. Columns are snake_case in the DB and camelCase in the API, with `DATETIME(3)` in UTC and utf8mb4. Every user-data query filters by `user_id`. Stateful tables carry a `version` column for optimistic locking.
- **External tools run without a shell:** pass `ffprobe` arguments as an array and never splice user content into a command line.
- **No sensitive values in logs:** source URL signatures, cookies, AppSecret, ticket query strings and album file paths.
