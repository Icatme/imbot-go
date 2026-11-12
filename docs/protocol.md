# WeChat iLink Bot API Protocol Reference

Base URL: `https://ilinkai.weixin.qq.com`
CDN URL: `https://novac2c.cdn.weixin.qq.com/c2c`

## Authentication (QR Login)

### Step 1: Get QR Code

```
POST /ilink/bot/get_bot_qrcode?bot_type=3
Body: { local_token_list?: ["<previous_bot_token>"] }
→ { qrcode: "<token>", qrcode_img_content: "<url>" }
```

The protocol accepts up to 10 local tokens. This SDK's public login flow sends
no historical tokens: normal login restores stored credentials before requesting
a QR, and explicit reauthentication must obtain a fresh session.

### Step 2: Poll Status

```
GET /ilink/bot/get_qrcode_status?qrcode=<token>[&verify_code=<digits>]
Headers: { "iLink-App-ClientVersion": "<computed_from_module_version>" }
→ { status: "wait" | "scaned" | "confirmed" | "expired" | "scaned_but_redirect"
          | "binded_redirect" | "need_verifycode" | "verify_code_blocked",
    bot_token?, ilink_bot_id?, ilink_user_id?, baseurl?, redirect_host? }
```

- `need_verifycode`: call `OnVerifyCode(ctx, isRetry)` for the code shown on the
  user's phone. A submitted code followed by another challenge sets `isRetry`
  to true. Keep the code on status polls until `scaned` accepts it.
- `verify_code_blocked`: request a new QR and clear the pending code and retry
  state. Blocked and expired QRs share the limit of three QR requests per login.
- `scaned_but_redirect`: continue status polling at `https://<redirect_host>`.
- `binded_redirect`: return an error in a QR login flow; existing credentials
  are restored before QR acquisition and never reused for explicit reauthentication.

The callback receives the login context and must honor its cancellation. Input
errors or empty trimmed codes stop login. Poll intervals are also cancellable.

## Common Headers (all POST requests)

```
Content-Type: application/json
iLink-App-Id: bot
iLink-App-ClientVersion: <computed_from_module_version>
AuthorizationType: ilink_bot_token
Authorization: Bearer <bot_token>
X-WECHAT-UIN: <base64(String(randomUint32))>
```

All POST bodies include: `base_info: { channel_version: "<module_version>", bot_agent?: "<agent>" }`. Development builds fall back to `0.9.1`.

## Get Updates (Long Poll)

```
POST /ilink/bot/getupdates
Body: { get_updates_buf: "<cursor>", base_info: {...} }
Timeout: 35s (server holds connection)
→ { ret: 0, msgs: [], get_updates_buf: "<new_cursor>" }
```

Error: `ret: -14` or `errcode: -14` = session expired (explicit reauthentication needed)

## Send Message

```
POST /ilink/bot/sendmessage
Body: {
  msg: {
    from_user_id: "",
    to_user_id: "<user_id>",
    client_id: "<stable_client_id>",
    run_id: "<optional_agent_run_id>",
    message_type: 2,        // BOT
    message_state: 2,       // FINISH
    context_token: "<from_inbound_msg>",
    item_list: [{ type: 1, text_item: { text: "..." } }]
  },
  base_info: {...}
}
```

`SendMessage` / `ReplyMessage` each send exactly one item and return the effective
`client_id`, `run_id`, and the server-assigned `message_id` when present.
A caller-supplied `client_id` is preserved; when the SDK
generates one, it is returned even if the HTTP attempt fails so an explicit retry
can reuse the same identity. Text chunking and media captions use the convenience
APIs and may produce multiple independent requests.

Successful responses may contain `{"ret":0,"message_id":18446744073709551615}`.
The receipt is exposed as `SendResult.MessageID` and in `AfterSend`; an absent ID
does not turn a successful send into a failure. Both `ret` and `errcode` are
checked before accepting a receipt.

## Message IDs and References

`message_id`, item `msg_id`, and reference `svr_id` accept unsigned 64-bit JSON
numbers or opaque JSON strings. The Go `MessageID` type stores them as strings
and marshals strings, without a floating-point conversion. Missing/null/zero
IDs decode as empty. Negative, fractional, or out-of-range numeric IDs fail
decoding; malformed messages still stop delivery without committing the cursor.

Newer clients may omit `ref_msg.message_item` and send only an ID:

```json
{
  "type": 1,
  "text_item": {"text": "What does this part mean?"},
  "ref_msg": {
    "svr_id": "18446744073709551615",
    "partial_text": {
      "start": "start marker",
      "end": "end marker",
      "startindex": 0,
      "endindex": 1,
      "quotemd5": "<MD5 of selected UTF-8 text>"
    }
  }
}
```

The SDK stores incoming text and accepted outgoing text under the authenticated
account, remote peer, and server ID. It resolves references before `AfterReceive`
and the Handler, preserving the original wire message. Outbound cache entries
use the final post-hook/normalized text and the server receipt, not `client_id`.
Context tokens, media URLs, keys, and media bytes are not part of the quote cache.

For incoming history, prefer the top-level `message_id`; if absent, use the first
nonempty/nonzero item `msg_id`. References prefer `svr_id`, then the nested
`message_item.msg_id`. Complete inline text, titles, and media information remain
authoritative; an item carrying only a type/ID is a placeholder, so it queries
the cache and reports `missing` when no record exists.

Partial indexes count non-overlapping occurrences of the boundary markers,
including both markers in the selected text. Global indexes apply without a
hash. When `quotemd5` is present, it validates the UTF-8 substring and can also
disambiguate the observed end-index variant relative to the selected start.
Missing/null/negative indexes or a hash mismatch never produce guessed text.
`QuotedMessage.Resolution` distinguishes embedded/cached content, missing history,
disabled/unavailable storage, and invalid partial references.

Cache errors are reported as `*QuoteCacheError` through error callbacks and do
not change delivery outcomes or advance/roll back cursor and replay state.
Storage/configuration limits and migration details are in the [README](../README.md).
Only text (including voice transcription) is restored; ID-only media references
remain unresolved in this implementation.

Upstream reference: [Tencent/openclaw-weixin PR #296](https://github.com/Tencent/openclaw-weixin/pull/296)
and its [partial-quote implementation](https://github.com/Tencent/openclaw-weixin/blob/a9ff7932d588d48a8a2b08c715fbefe4549ffcf6/src/messaging/partial-quote.ts).

## Send Typing

```
POST /ilink/bot/getconfig
→ { typing_ticket: "<ticket>" }

POST /ilink/bot/sendtyping
Body: { ilink_user_id: "<id>", typing_ticket: "<ticket>", status: 1|2, base_info: {...} }
```

## Media Upload

```
POST /ilink/bot/getuploadurl
→ { upload_param: "<encrypted>" }

POST <cdn>/upload?encrypted_query_param=<param>&filekey=<key>
Content-Type: application/octet-stream
Body: AES-128-ECB encrypted bytes
Response Header: x-encrypted-param → download param
```

## Media Download

```
GET <cdn>/download?encrypted_query_param=<param>
→ AES-128-ECB encrypted bytes → decrypt with aes_key
```

The Go SDK exposes inline referenced media through `QuotedMessage.Media` and
`DownloadQuoted(ctx, incoming)`. These are local SDK APIs, not new wire fields.
For ID-only `ref_msg.svr_id` references, locally retained media can be read after
restart; missing, expired, or evicted payloads match `ErrQuoteMediaUnavailable`.
Retention does not persist CDN credentials and does not automatically download
attachments while polling. `Download` continues to refer to current-message media.

## Message Item Types

| Type | Value | Description |
|------|-------|-------------|
| TEXT | 1 | Text content |
| IMAGE | 2 | Image with CDN media |
| VOICE | 3 | Voice with optional transcription |
| FILE | 4 | File attachment |
| VIDEO | 5 | Video with optional thumbnail |
| TOOL_CALL_START | 11 | Tool invocation started (`tool_name`, `tool_call_id`) |
| TOOL_CALL_RESULT | 12 | Tool invocation completed with status |

Message items may also carry `create_time_ms`, `update_time_ms`, `is_completed`, and `msg_id`.
Messages may carry `session_id`, `group_id`, and `run_id` for Agent correlation.

## AES Key Formats

| Format | Example | Usage |
|--------|---------|-------|
| base64(raw 16 bytes) | `ABEiM0RVZneImaq7zN3u/w==` | CDNMedia.aes_key (format A) |
| base64(hex string) | `MDAxMTIyMzM0NDU1NjY3Nzg4OTlhYWJiY2NkZGVlZmY=` | CDNMedia.aes_key (format B) |
| direct hex (32 chars) | `00112233445566778899aabbccddeeff` | image_item.aeskey |

## Error Codes

| Code | Meaning | Action |
|------|---------|--------|
| `ret: 0` | Success | — |
| `ret: -14` or `errcode: -14` | Session expired | Call `Reauthenticate` explicitly |
| `ret: -2` | Parameter error | Check request |

`APIError` preserves the endpoint, HTTP status, `ret`, and `errcode` separately;
use `errors.As` to inspect it through wrapped SDK errors.

## context_token

- **Required** for every reply — routes messages to the correct conversation
- Cache per `(accountId, userId)` pair
- Persist across restarts
- Clear on session expiry / explicit reauthentication. A durable SDK-local marker (not a wire-protocol field) blocks ordinary login across process restarts until fresh credentials and cleared context state are installed.
