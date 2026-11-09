# @ftptech/x402-canton-next

## 0.2.8

### Patch Changes

- 0bf13cf: Express refuses at startup a route key that is a pattern (`:id`, `*`, a `[slug]` segment), has no method, or holds `?`, `#` or a space: such a key matched no request, so the paid route was served free. The payment of a payer who leaves during /settle still settles but stays unspent, so a retry of the same payment is served. A connection that drops after the handler started and before any header went out, a client or proxy timeout included, gives the payment back once per process. A retry is then served, and one payment runs the handler at most twice in a process. Responses the middleware sends on a paid route carry `Cache-Control: no-store`. The paid response gets `private` merged into its own Cache-Control, also when headers are passed to `writeHead` as an object or a raw array, so a shared cache cannot serve it to the next caller. Next gives the payment back when the handler returns a 5xx, as it already did when the handler threw, and when the request was aborted before the handler ran.
- db1d351: The express and next middlewares understand the facilitator's scheme answers. A repeat of a settled payment, refused with its `updateId`, is served through the redeemed store: once, or again after a failed delivery gave the payment back. `settlement_pending`, as a 503 with its body or a 200, is retried with the same payment for about 7 s, then answered 502 "do NOT pay again", never 402. The client reads the reason from PAYMENT-RESPONSE on a 402, where a server on the official x402 SDK puts it, and stops without paying again on `settlement_pending`. A failed SettleResponse in core carries `transaction`, `network` and `payer`.
- Updated dependencies [db1d351]
- Updated dependencies [fb41063]
- Updated dependencies [672bf45]
- Updated dependencies [cddefd2]
- Updated dependencies [7f6f15b]
  - @ftptech/x402-canton-core@1.9.0

## 0.2.7

### Patch Changes

- 8f8e6bf: A PAYMENT-SIGNATURE that decodes to null, an array or a primitive is refused with 400 `malformed PAYMENT-SIGNATURE header`. Before, null threw while reading `accepted`; on Express 4, which does not await middleware, that ended the merchant's process. The Express middleware now also passes any unexpected error to `next(err)`.

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
