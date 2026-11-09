# @ftptech/x402-canton-express

## 0.2.9

### Patch Changes

- 0bf13cf: Express refuses at startup a route key that is a pattern (`:id`, `*`, a `[slug]` segment), has no method, or holds `?`, `#` or a space: such a key matched no request, so the paid route was served free. The payment of a payer who leaves during /settle still settles but stays unspent, so a retry of the same payment is served. A connection that drops after the handler started and before any header went out, a client or proxy timeout included, gives the payment back once per process. A retry is then served, and one payment runs the handler at most twice in a process. Responses the middleware sends on a paid route carry `Cache-Control: no-store`. The paid response gets `private` merged into its own Cache-Control, also when headers are passed to `writeHead` as an object or a raw array, so a shared cache cannot serve it to the next caller. Next gives the payment back when the handler returns a 5xx, as it already did when the handler threw, and when the request was aborted before the handler ran.
- db1d351: The express and next middlewares understand the facilitator's scheme answers. A repeat of a settled payment, refused with its `updateId`, is served through the redeemed store: once, or again after a failed delivery gave the payment back. `settlement_pending`, as a 503 with its body or a 200, is retried with the same payment for about 7 s, then answered 502 "do NOT pay again", never 402. The client reads the reason from PAYMENT-RESPONSE on a 402, where a server on the official x402 SDK puts it, and stops without paying again on `settlement_pending`. A failed SettleResponse in core carries `transaction`, `network` and `payer`.
- Updated dependencies [db1d351]
- Updated dependencies [fb41063]
- Updated dependencies [672bf45]
- Updated dependencies [cddefd2]
- Updated dependencies [7f6f15b]
  - @ftptech/x402-canton-core@1.9.0

## 0.2.8

### Patch Changes

- 8f8e6bf: A PAYMENT-SIGNATURE that decodes to null, an array or a primitive is refused with 400 `malformed PAYMENT-SIGNATURE header`. Before, null threw while reading `accepted`; on Express 4, which does not await middleware, that ended the merchant's process. The Express middleware now also passes any unexpected error to `next(err)`.
- 5fc2c87: A paid path whose URL holds repeated slashes gets 400 and is neither charged nor passed on. On Express 4, GET /api//data served a paid route on a mounted Router for free, or charged for a 404 when the middleware was mounted at /api. On Express 5, GET /api// served the paid root of a mounted Router for free. A request target that leaves Express no path, such as `foo://h/api` under a mount, also gets 400; it used to end the process on Express 4.
- 86f5c0c: With `strict routing` or `case sensitive routing` on the app, a spelling of a paid route that the app's flags reject, such as GET /api/data/ or /api/DATA, gets 404 with no charge. That holds wherever the middleware sits, for a request that reaches it; /api//data/ gets 400. With `strict routing` on, a paid mount root, "GET /api" with the middleware at /api, is charged at /api and refused at /api/. On the app, the middleware no longer lets a paid route on an express.Router() serve such a spelling for free; mounted under a path, it no longer charges for one the app then answers 404. Inside an express.Router() it used to charge and serve such a spelling; now only the spelling the flags accept is charged. Such a spelling no longer reaches anything after the middleware: a free route that differs from a paid key only by a trailing slash or case, a parameter route, static files, or a redirect to the canonical path. The flags now count as on for any truthy value, as Express reads them; `app.set("strict routing", "true")` used to charge for a path the app then answered 404.

  A paid path sent with dot segments or percent-escapes, such as GET /files/./report.pdf, /files/x/../report.pdf or /files/%72eport.pdf, gets 400 instead of the file for free from express.static, which resolves these spellings and the middleware did not. Write keys as the URL is sent ("GET /files/my%20report.pdf"). A key written decoded ("GET /files/my report.pdf") now gets 400 for the encoded request, and so does a request that encodes the name differently from the key, such as %2B for +. Two keys with different terms that are one file to express.static refuse to start.

## 0.2.7

### Patch Changes

- 6a0f73a: Content served by app.use, express.static or router.use keeps the redemption ticket spent, so a payment cannot be replayed. The ticket comes back only when nothing was delivered: no response, a 5xx, a 404 from middleware, or a redirect that fetch follows back through this gate. Any other answer, such as a 401 from auth middleware, keeps it spent.
- Updated dependencies [580bff2]
  - @ftptech/x402-canton-core@1.5.0

## 0.2.6

### Patch Changes

- Verify-before-sign learns the shapes a Tradecraft swap needs, and pins who owns
  what a transfer creates.

  - `allowRegistryOffer` (opt-in, never inferred from the relay) admits the DA
    Registry Utility two-step offer shape — `AllocationFactory_TransferInternal`
    plus exactly one `TransferOffer` create — which is how a registry transfer to a
    receiver with no preapproval settles, and therefore how a swap to a pool
    settles. Callers that do not opt in keep the strict behaviour byte for byte.
  - A party minted under a TRUSTED instrument admin's key-fingerprint namespace is
    admitted alongside the enumerated trusted parties. Registrars operate their
    tokens through such parties (onRails puts per-user identities inside otherwise
    ordinary holdings), and a relay cannot forge one.
  - Ownership is pinned separately from that trust: every registry `Holding` a
    transfer creates is either the receiver's delivery or the sender's change, and
    must name one of them. Without this the namespace admission alone would let a
    relay re-own the sender's change — everything the transfer did not send.
  - The accept path admits the value-less `ExecutedTransfer` receipt some registrar
    versions create, without relaxing the owner pin on the holding beside it.

  The merchant SDK READMEs document the payment header's real size: a server must
  accept at least 16 KiB end to end.

- Updated dependencies
  - @ftptech/x402-canton-core@1.2.0

## 0.2.5

### Patch Changes

- Updated dependencies [6428700]
  - @ftptech/x402-canton-core@1.0.0

## 0.2.3

### Patch Changes

- Ship the fixes the registry never got.

  Four packages carried changed code under a version number npm already had, so
  `changeset publish` skipped them silently and no consumer could ever receive the
  fix. Verified by unpacking the published tarballs and hashing every `dist/*.js`
  against the local build: core differs in 2 files, client in 1, express in 1,
  next in 1; ledger is byte-identical and needs nothing.

  **core** — the published 0.7.0 decoder reads protobuf tags and lengths with the
  64-bit varint reader. The bound that refuses an over-32-bit tag (`readVarint32`,
  `> 0xffffffff`) exists only in the working tree, and that module IS
  verify-before-sign: agent-wallet re-exports `assertPreparedTransferMatches` from
  here. A participant's `readRawVarint32` and protobufjs both truncate such a tag
  and parse the field, so bytes the agent's validator skips as an unknown field are
  fully effective on execute and covered by the hash it signs. Every payer
  installed from npm runs without that bound today.

  The published 0.7.0 also ships no `network-failure.js` at all, while express and
  next now import `connectionNeverEstablished` from it — so this bump is a
  prerequisite for publishing them, not an optional companion.

  **client** — the ambiguity fixes in `fetch.ts`: the guard ordered ahead of the
  retry-budget branch, and the machine-readable `PAYMENT_UNCONFIRMED` code.

  **express** — the payment gate now covers every spelling the router will serve:
  `baseUrl + path` as well as the mount-relative path, and both the app's own
  normalisation and the loosest one (an `express.Router()` does not inherit the
  app's `strict routing` / `case sensitive routing`). Each of those gaps served a
  paid resource for free, silently. The default redeemed store is now one per
  process rather than one per middleware instance, and a settle reported successful
  without a transaction id is refused rather than deduped under a shared empty key.

  **next** — the same unidentifiable-settle refusal, so the twins stay in step.

- Updated dependencies
  - @ftptech/x402-canton-core@0.7.1
