# PO Agent Worklog

## 2026-09-28

- Followed Tencent/openclaw-weixin #296 for lossless message identifiers, server send receipts, and text-only quote restoration. `MessageID` now has a string representation across message/item/reference IDs; JSON uint64 values remain exact, existing nonzero numeric replay keys remain unchanged, and `SendResult`/`AfterSend` expose the server-assigned ID. Updated callers/tests and documented the source-level migration without legacy ID fields.
- Added an optional, default-enabled atomic JSON quote snapshot using existing persistence code and no new dependency. Entries are scoped by authenticated account and remote peer; defaults are 30 days and 10,000 messages/account, with a 64 KiB text-entry limit and 16 MiB aggregate content/identity budget. Corrupt snapshots are retained and unavailable rather than reset; cache errors are typed and reported separately from successful sends and Ack/Retry/Drop outcomes. Added quote-path collision protection to login/reauthentication state validation.
- Restore ID-only text references before receive hooks/handlers and preserve raw wire input; cache exact inbound text and accepted post-hook/normalized outbound text. Validate partial-reference occurrence indexes and optional MD5, distinguish missing/disabled/unavailable/invalid-partial states, and never substitute full or guessed text on validation failure. Media persistence and native voice-bubble sending remain outside this text-reference change.
- Fixed both quote review findings: inbound cache identity now prefers the top-level message ID and then the first valid item ID; reference identity prefers `svr_id` and then the nested item ID. ID/type-only placeholders resolve through the cache and report `missing` on a miss, while actual embedded text, titles, and media remain authoritative. Added regressions for numeric/string IDs, zero/blank IDs, precedence, restart recovery, and placeholder states; reproduced the failures before the fixes, then passed the full race suite, vet, and whitespace checks.
- Validation on Windows with Go 1.26.4: `go test ./...`, `go test -race ./...`, `go build ./...`, `go vet ./...`, `go mod verify`, and `git diff --check` passed. Regressions cover uint64 extremes, numeric/string IDs, receipts, restart recovery, account/peer isolation, partial quotes, failed sends, cache corruption/write failures, retention/capacity, and concurrent persistence. No live WeChat or UI acceptance was performed; no release/tag/push was made.
- Prepared the user-requested v0.5.0 source-only Go module release for the reviewed quote-context changes and fixes. Bumped the development channel-version fallback and its assertion, promoted the README feature section to release documentation, and documented the source-level MessageID migration in the release notes. The minor-version bump reflects the public ID type change; no dependencies were added or upgraded.
- Release CI exposed a scheduling assumption in `TestRunNormalizesExpiredSessionAfterConcurrentRecovery`: the mock second poll could expire the session before the old handler started. Gate that response on handler entry and cancel pending test work during cleanup, so the fixture deterministically exercises concurrent recovery without changing runtime behavior or increasing timeouts. The targeted race test passed 100 repetitions, followed by the full race suite, vet, and whitespace checks.

## 2026-08-11

- Hardened the Go CI matrix: PRs now run for every base branch, setup-go reads `go.mod`, all platforms build/test, and Linux additionally verifies modules, vets, and runs race-enabled tests.

## 2026-06-23

- Bumped SDK fallback protocol version to `0.3.0` and hardened runtime bug fixes across root SDK files: login/session send guards, UTF-8 text chunking, QR/API decode errors, CDN/remote HTTP client reuse, post-login account state binding, and matching tests/docs.

## 2026-08-08

- Hardened inbound delivery and outbound validation: added 24-hour persistent replay dedupe, decoupled long polling from ordered handler execution, committed cursors only after batch processing, and rejected missing `context_token` before downloads, uploads, or sends; added focused regression coverage.

## 2026-08-09

- Replaced fire-and-forget inbound callbacks with one context-aware Handler pipeline and explicit Ack/Retry/Drop outcomes; retry and hook failures now leave replay/cursor state uncommitted, handler panics request redelivery, and concurrent Run calls are rejected with lifecycle state reset on exit.

- Added a bounded keyed dispatcher with same-user ordering, cross-user concurrency, bounded uncommitted batches, and an ordered cursor-completion frontier; cursor state now fails closed on load/write errors, concurrent context-token persistence and hook registration are synchronized, and the echo example is concurrency-safe.

- Aligned the public message envelope with official Agent fields: message/item timestamps, session/group/run correlation IDs, and tool-call start/result item types 11/12, with parsed correlation metadata and protocol tests.

- Added a one-item outbound contract with caller-stable/generated `client_id`, failure-safe `SendResult`, per-message `run_id`, tool-call progress builders, and wire-level BeforeSend/AfterSend hooks across text and media sends.

- Preserved endpoint, HTTP status, `ret`, and `errcode` independently in the public API error type; wrapped errors and HTTP error envelopes now retain reliable `-14` classification.

- Replaced the timed `-14` pause with a fail-closed reauthentication state: all authenticated API paths invalidate credentials/config/context once, cancel Run without auto-QR or token reuse, preserve cursor/replay state, recover only through explicit fresh QR reauthentication, and isolate stale auth/context work from the new session with generation checks plus a cancellable request gate.

## 2026-08-11

- Closed reliable-delivery failure gaps: malformed wire messages now fail closed, replay identities are conversation-scoped and committed as one alias set, and credential/replay/cursor/context JSON state uses atomic replacement with disk-before-memory publication.

- Made explicit reauthentication recoverable across process crashes after the durable commit point: a per-credential marker blocks normal login while a transition is active, records every context path, survives fresh credential acquisition, and is removed only immediately before publishing the validated fresh session. Sudden-power-loss directory durability remains outside the atomic-write contract.

- Bumped the SDK fallback protocol version to `0.4.0` and prepared the source-only Go module release after the reliable-delivery, Agent-envelope, outbound-identity, typed-error, and explicit-reauthentication stack merged to `main`.

- Updated README for the `v0.4.0` delivery/session changes and migration notes; replaced promotional repository comparisons with neutral scope, provenance, and synchronization statements.

## 2026-08-12

- Split the 1,804-line root `bot.go` into same-package responsibility files for login, session gating, polling, content, media, message parsing, handlers, and logging; preserved declarations, public API, control flow, and lock ordering, and refreshed the documented Go file structure.

## 2026-08-20

- Normalized unsafe bare `<` characters independently in every outbound wire message and referenced text after hooks on a request-owned copy, so caller templates remain unchanged and concurrency-safe; an explicit small HTML-tag set and line-aware fenced code remain preserved, with Markdown stripping and chunking before the final wire-boundary guard.

- Adapted upstream sensitive-data hardening to the Go SDK: centralized recursive and malformed-input redaction (including quoted/delimited credentials and authority-form URLs), capped upstream API error messages at 4 KiB of valid UTF-8, rebuilt unsafe error wrappers while preserving safe causes, and added API/CDN/QR/remote-media/log leakage regressions.

- Bumped the SDK fallback protocol version to `0.4.1` and prepared the source-only patch release covering sensitive-error redaction, safe outbound bare-`<` normalization, concurrency-safe request copies, and the same-package root implementation split.

## 2026-09-25

- Prepared v0.4.2 long-poll timeout correction: getUpdates request deadline is at least 65 seconds and at least 15 seconds beyond the server-suggested wait; the HTTP client's overall cap is 2 minutes. Extended the existing protocol test to inspect request deadlines without live network use. Updated channel-version fallback and release documentation. OC AI Go's polling notices and actual-send guidance are tracked in its shared worklog.

## 2026-10-01

- Selected the useful CoreSpeed upstream follow-ups: pairing-code retry feedback and multi-account operating guidance. Changed `Options.OnVerifyCode` directly to `func(context.Context, bool) (string, error)` without a legacy adapter, and documented this source-level migration separately from the published v0.5.0 release. Callbacks receive the login context and must honor cancellation; the boolean distinguishes rejected-code re-entry from an initial challenge.
- Keep the pending code until the server accepts it with `scaned`, reset pairing state for each fresh QR, trim input, reject empty input, and stop on callback errors. Pairing-code blocks and QR expiry share the existing three-QR limit. Replace uncancellable QR polling sleeps with context-aware waiting, and refuse to submit input returned after cancellation.
- Removed unreachable historical-token and `binded_redirect` credential-reuse branches from QR login while preserving explicit fresh-session reauthentication. Ordinary credential-load errors now stop before QR acquisition and retain the original file instead of silently falling through to a login that could overwrite it. The lower-level protocol still accepts `local_token_list`; public login does not advertise historical tokens.
- Added state-machine regressions for rejected/accepted codes, blocked-code QR renewal and limits, missing/error/blank input, cancellation while awaiting input or polling, and corrupt credentials. A Bot-level regression verifies that canceled pairing keeps the old session gated and its durable reauthentication marker intact, then allows a fresh explicit pairing attempt to recover.
- Updated README, protocol, architecture, and roadmap documentation: separate business tenant directories from the expected WeChat bot ID, isolate all state paths, keep one active poller/writer per account across the deployment, and let applications enforce ownership through leases or database locks.
- Validation on Windows with Go 1.26.4: focused auth/protocol and login tests, `go test -race ./...`, `go build ./...`, `go vet ./...`, `go mod verify`, and `git diff --check` passed. No new dependency, version/tag/release, commit, or push was made. No live WeChat or UI acceptance was performed.
- Made the poll-interval cancellation regression deterministic with Go's standard `testing/synctest`: wait until Login has decoded the response and blocked on its interval before canceling, then assert that no fake time advances while draining the login goroutine. The focused race test passed 100 repetitions; an isolated copy replacing the runtime wait with `time.Sleep(2*time.Second)` failed all 20 repetitions with the expected 2-second delay. The full race suite, vet, formatting, and whitespace checks passed after this test-only fix; runtime code was not changed.
- Prepared the user-requested v0.6.0 version bump, commit, and push on `main`, including the Go module version tag. The minor bump reflects the public verification callback signature change. Updated the development channel-version fallback and its assertion, promoted the migration notes to version documentation, and kept prior releases under history. No dependencies were added or upgraded. Repeated `go test -race ./...`, `go build ./...`, `go vet ./...`, and `git diff --check` after the version bump; all passed.
