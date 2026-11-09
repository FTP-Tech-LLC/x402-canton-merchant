/**
 * cantonPaymentMiddleware — Express middleware that gates routes
 * behind Canton x402 payment via a facilitator.
 *
 * Per-request flow:
 *
 *   1. Match `${req.method} ${req.path}` against `routes`. If no
 *      match, call next() (route not gated).
 *   2. No PAYMENT-SIGNATURE header → respond 402 with
 *      PAYMENT-REQUIRED carrying the route's accepts[] and an
 *      auto-derived resource URL.
 *   3. PAYMENT-SIGNATURE present → decode the PaymentPayload envelope.
 *      Call POST {facilitatorUrl}/verify. If isValid: false →
 *      respond 402 with the invalidReason.
 *   4. isValid: true → call POST {facilitatorUrl}/settle. If
 *      success: false → respond 402 with the errorReason. If
 *      success: true → set PAYMENT-RESPONSE header with settlement
 *      metadata and call next() so the handler runs.
 *
 * Settle happens BEFORE the handler. The trade-off:
 *   pre-handler settle: if the handler crashes after settle, the
 *     payment landed on-ledger but the resource wasn't delivered.
 *     Merchant-friendly; bad UX in the (rare) crash case.
 *   post-handler settle: would require monkey-patching res.send to
 *     await settle before flushing — more complex, more failure
 *     modes (e.g. supertest hangs on async send wrap).
 *
 * For v0.1 we ship pre-handler settle. Post-handler settle is
 * tracked in BACKLOG.md if a merchant needs it.
 *
 * Failure modes:
 * - Facilitator unreachable on verify or settle → 502.
 * - Facilitator returns malformed JSON → 502.
 *
 * Versioning: x402 v2 wire format only. v1 is BACKLOG.md.
 */

import type { Request, RequestHandler, Response } from "express";
import {
  HEADER_PAYMENT_REQUIRED_V2,
  HEADER_PAYMENT_SIGNATURE_V2,
  HEADER_PAYMENT_RESPONSE_V2,
  encodeBase64Json,
  decodeBase64Json,
  selectServerRequirements,
  assertAssetInstrumentConsistency,
  createInMemoryRedeemedStore,
  type RedeemedStore,
  type PaymentRequirements,
  type X402ResourceInfo,
  connectionNeverEstablished,
} from "@ftptech/x402-canton-core";

export interface RouteConfig {
  /** Entries advertised in 402.accepts[] for this route. */
  accepts: PaymentRequirements[];
  /** Description shown to clients in the PAYMENT-REQUIRED body. */
  description?: string;
  /** Content-Type the protected handler will produce. */
  mimeType?: string;
}

export interface CantonPaymentMiddlewareOptions {
  /** Keys are `"<METHOD> <PATH>"` strings, e.g. `"GET /api/data"`. */
  routes: Record<string, RouteConfig>;
  /** Base URL of the Canton x402 facilitator (no trailing slash). */
  facilitatorUrl: string;
  /** Injectable fetch — defaults to `globalThis.fetch`. */
  fetch?: typeof globalThis.fetch;
  /**
   * One payment, one delivery. A settle updateId may be redeemed ONCE; the
   * replay of an already-redeemed payment is answered 402 rather than served
   * again. Defaults to ONE store shared by every middleware instance in this
   * process — pass a SHARED implementation (Redis, a table) when running more
   * than one PROCESS, or each process will serve the same replay once.
   * Explicit `null` disables the check and restores the old
   * unlimited-redemption behaviour.
   */
  redeemed?: RedeemedStore | null;
}

interface PaymentPayloadEnvelope {
  x402Version: number;
  scheme: string;
  accepted: PaymentRequirements;
  resource: X402ResourceInfo;
  payload: unknown;
  extensions?: Record<string, unknown>;
}

/**
 * ONE default store for the whole process, not one per middleware instance.
 *
 * Scoping middleware to a subtree — one instance per route group, or per
 * facilitator, which this package's own suite exercises — used to give each
 * instance its own Map. A settled payment was then redeemable once PER
 * INSTANCE: replay the identical PAYMENT-SIGNATURE header against a route
 * gated by another instance and it served, because the facilitator answers a
 * repeat /settle of the same bytes from its idempotency record with the
 * ORIGINAL updateId (as `success:true`, or under the scheme's answer as a
 * refusal this middleware reads the same way), so instance #2 sees a fresh,
 * valid ticket its own Map has never held.
 *
 * The ticket has to be global, because the thing it identifies — a settle
 * updateId — is global. packages/next reached this conclusion first (see
 * processRedeemed there); this is the same rule, not a second one.
 *
 * A merchant on several PROCESSES still needs a shared store (Redis, a table)
 * and can pass one; this only fixes the in-process default, which was wrong
 * even for a single process.
 */
let processRedeemed: RedeemedStore | undefined;

/** Waits before each retry of a `settlement_pending` settle: about 7 s in all. */
const PENDING_RETRY_MS = [1000, 2000, 4000];

/** The body of a 503 /settle when it is the scheme's `settlement_pending`
 *  answer, else null (a proxy error page, or a facilitator that cannot tell). */
async function pendingBody(r: globalThis.Response): Promise<Record<string, unknown> | null> {
  const body = (await r.json().catch(() => null)) as Record<string, unknown> | null;
  return body !== null &&
    typeof body === "object" &&
    body.success === false &&
    body.errorReason === "settlement_pending"
    ? body
    : null;
}

export function cantonPaymentMiddleware(
  options: CantonPaymentMiddlewareOptions
): RequestHandler {
  const fetchImpl = options.fetch ?? globalThis.fetch;
  const facilitatorUrl = options.facilitatorUrl.replace(/\/$/, "");
  const redeemed =
    options.redeemed === null
      ? null
      : (options.redeemed ??
        (processRedeemed ??= createInMemoryRedeemedStore()));

  // Config-time guard (audit L1): on every advertised entry, asset must agree
  // with extra.instrumentId — otherwise the operator misconfig is silent at
  // runtime (the facilitator validates instrumentId and ignores asset on the
  // CIP-56 path). Fail fast at setup.
  for (const route of Object.values(options.routes)) {
    for (const r of route.accepts) assertAssetInstrumentConsistency(r);
  }

  // A ROUTE KEY IS A METHOD AND A LITERAL PATH.
  //
  // Keys are looked up as written. "GET /api/items/:id", "GET /premium/*",
  // "GET /docs/[slug]", a key with a query or a space, or one with no method
  // (the shapes @x402/express takes as patterns) matched no request here, so
  // the middleware called next() and the handler served the paid response
  // free, with no error at startup. Brackets inside a name ("report[1].pdf")
  // are a literal path; a whole [slug] / [...slug] / [[...slug]] segment is not.
  for (const key of Object.keys(options.routes)) {
    const sp = key.indexOf(" ");
    const method = sp < 0 ? "" : key.slice(0, sp);
    const path = sp < 0 ? key : key.slice(sp + 1);
    if (
      !/^[A-Za-z]+$/.test(method) ||
      !path.startsWith("/") ||
      /[?#*\s]|(^|\/):|(^|\/)\[{1,2}(\.\.\.)?[^/\]]+\]{1,2}(?=\/|$)/.test(path)
    ) {
      throw new Error(
        `cantonPaymentMiddleware: route ${JSON.stringify(key)} is not "METHOD /literal/path". ` +
          `Keys are literal: ":param", "*", "[slug]", a query or a space match no ` +
          `request, so the paid route would be served free. Give each paid path its own key.`
      );
    }
  }

  // THE GATE MUST COVER EVERYTHING THE ROUTER WILL SERVE.
  //
  // It used to look up `${req.method} ${req.path}` in `routes` and call next()
  // on a miss — an exact string match against a router that matches loosely.
  // Express defaults to `case sensitive routing` OFF and `strict routing` OFF,
  // so `app.get("/api/data")` also serves `/api/data/` and `/API/data`, and it
  // answers HEAD from the GET handler. None of those hit the configured key, so
  // the request fell straight through to the paid handler. Measured against a
  // real express app with the middleware configured for "GET /api/data":
  //
  //   GET  /api/data    402            <- the only form that was gated
  //   GET  /api/data/   200 PREMIUM    <- free
  //   GET  /API/data    200 PREMIUM    <- free
  //   GET  /Api/Data/   200 PREMIUM    <- free
  //   HEAD /api/data    200            <- handler ran, ungated
  //
  // A trailing slash is not an attack anyone needs to discover; a browser, a
  // proxy, or a link with a slash is enough to hand the resource away.
  //
  // The lookup now normalises exactly the dimensions the app's own router
  // ignores — read from the SAME two settings the router reads, so a merchant
  // who turns strict/case-sensitive routing ON keeps an exact gate and does not
  // start getting 402s for paths that would 404.
  const norm = (path: string, strict: boolean, caseSensitive: boolean): string => {
    let p = caseSensitive ? path : path.toLowerCase();
    if (!strict && p.length > 1 && p.endsWith("/")) p = p.slice(0, -1);
    return p;
  };
  const buildIndex = (
    strict: boolean,
    caseSensitive: boolean
  ): Map<string, RouteConfig> => {
    const idx = new Map<string, RouteConfig>();
    for (const [key, cfg] of Object.entries(options.routes)) {
      const sp = key.indexOf(" ");
      const method = (sp < 0 ? key : key.slice(0, sp)).toUpperCase();
      const path = sp < 0 ? "" : key.slice(sp + 1);
      idx.set(`${method} ${norm(path, strict, caseSensitive)}`, cfg);
    }
    return idx;
  };
  // Fail fast on a config whose entries collapse together under the loosest
  // normalisation — silently gating one of two routes is worse than refusing
  // to start.
  {
    const seen = new Map<string, string>();
    for (const key of Object.keys(options.routes)) {
      const sp = key.indexOf(" ");
      const method = (sp < 0 ? key : key.slice(0, sp)).toUpperCase();
      const path = sp < 0 ? "" : key.slice(sp + 1);
      const k = `${method} ${norm(path, false, false)}`;
      const prev = seen.get(k);
      // TWO SPELLINGS OF ONE ROUTE ARE ONLY A PROBLEM IF THEY DISAGREE.
      //
      // The guard refuses to start because the index can hold one config per
      // normalised route, so a collision would silently charge one spelling at
      // the other's price. That reasoning needs the two configs to DIFFER.
      // Where they are the same object — the defensive double-spelling a
      // merchant added as a workaround for the trailing-slash hole this
      // version closes — there is nothing to pick wrong, and refusing to boot
      // turns a patch bump into an outage for a config that was correct.
      //
      // Deliberately identity, not deep equality: two configs that merely look
      // alike today can drift apart tomorrow, and this check runs once at
      // startup where a false pass is expensive and a false refusal is loud.
      if (prev !== undefined && options.routes[prev] !== options.routes[key]) {
        throw new Error(
          `cantonPaymentMiddleware: routes ${JSON.stringify(prev)} and ` +
            `${JSON.stringify(key)} are the same route to express (it ignores ` +
            `case and a trailing slash by default) — only one could ever be ` +
            `charged. Give them distinct paths.`
        );
      }
      seen.set(k, key);
    }
  }
  let cachedIndex: Map<string, RouteConfig> | null = null;
  let cachedMode = "";
  // The loosest reading, always available. See the note in the handler.
  const looseIndex = buildIndex(false, false);
  // Every key by the path express.static reads for it (canonicalPath), read
  // loosely, and apart those whose written form is not canonical (written
  // with escapes). See "A PAID FILE IS NOT FREE" in the handler.
  const canonIndex = new Map<string, RouteConfig>();
  const escapedKeyIndex = new Map<string, RouteConfig>();
  {
    const firstKey = new Map<string, string>();
    for (const [key, cfg] of Object.entries(options.routes)) {
      const sp = key.indexOf(" ");
      const method = (sp < 0 ? key : key.slice(0, sp)).toUpperCase();
      const path = sp < 0 ? "" : key.slice(sp + 1);
      if (!path.startsWith("/")) continue;
      const canon = canonicalPath(path) ?? path;
      const k = `${method} ${norm(canon, false, false)}`;
      const prev = firstKey.get(k);
      if (prev !== undefined && options.routes[prev] !== cfg) {
        throw new Error(
          `cantonPaymentMiddleware: routes ${JSON.stringify(prev)} and ` +
            `${JSON.stringify(key)} are one file to express.static (it decodes ` +
            `%XX and resolves "." and "..") — only one could ever be charged. ` +
            `Give them distinct paths.`
        );
      }
      firstKey.set(k, key);
      canonIndex.set(k, cfg);
      if (norm(canon, false, false) !== norm(path, false, false)) escapedKeyIndex.set(k, cfg);
    }
  }

  const handle: RequestHandler = async function middleware(req, res, next) {
    // THE APP'S ROUTING FLAGS DO NOT GOVERN EVERY ROUTER, SO THE GATE CHARGES
    // ONLY WHAT THEY MATCH.
    //
    // `strict routing` and `case sensitive routing` configure the app's OWN
    // base router; an `express.Router()` is built loose and never consults the
    // app. A spelling the flags reject (GET /api/data/ for "GET /api/data" with
    // strict routing on) may be served by a Router or answered 404 by the app's
    // own router, and from inside the request the gate cannot tell which.
    // Passing it on served the paid route free in the first case (measured:
    // gate on the app, paid route on a Router, 200 premium); charging it took a
    // payment for a 404 in the second (measured: gate mounted at /api, paid
    // route on the app, settle and then 404). So such a spelling gets 404
    // wherever the gate sits, and the spelling the flags accept is charged and
    // served.
    // Truthy, as Express reads them (app.enabled): "true" or 1 turns them on.
    const strict = Boolean(req.app?.get?.("strict routing"));
    const caseSensitive = Boolean(req.app?.get?.("case sensitive routing"));
    const mode = `${strict}|${caseSensitive}`;
    if (cachedIndex === null || cachedMode !== mode) {
      cachedIndex = buildIndex(strict, caseSensitive);
      cachedMode = mode;
    }
    // Held for the close listener: by then routing has rewritten req.baseUrl.
    const appIndex = cachedIndex;
    const baseUrl = req.baseUrl;
    // ONE REQUEST HAS TWO HONEST SPELLINGS, SO BOTH MUST GATE.
    //
    // Express strips the mount prefix from `req.path`. Mounted at "/api", a
    // request for /api/data has `req.baseUrl="/api"`, `req.path="/data"`, and
    // `req.originalUrl="/api/data"` — so a merchant may reasonably have written
    // either "GET /api/data" (the full application path, what the README, the
    // docs and the examples all use, and what the 402 body reports as the
    // resource url) or "GET /data" (relative to the mount).
    //
    // Matching only ONE of them serves the paid resource for free under the
    // other, fail-OPEN and silent: no 402, no log line, invisible to the
    // setup-time collision check, and the merchant learns from a flat revenue
    // graph. Measured, with the middleware mounted at "/api":
    //
    //   key "GET /api/data", index on req.path            -> 200 premium
    //   key "GET /data",     index on baseUrl+path only   -> 200 premium
    //
    // Both were real; fixing the first alone just moved the hole. So look the
    // request up under both spellings and gate if EITHER is configured. The
    // failure direction of matching too much is a spurious 402 on a route the
    // merchant meant to be free — visible and recoverable — against silent
    // unpaid delivery, which is neither. The relative spelling can only ever
    // fire for requests already routed through this mount, so it cannot reach
    // across into an unrelated subtree.
    const inIndex = (
      index: Map<string, RouteConfig>,
      method: string,
      p: string
    ): RouteConfig | undefined =>
      index.get(`${method} ${p}`) ??
      // Express answers HEAD from the GET handler, so a HEAD must meet the same
      // gate; an explicitly configured HEAD route still wins.
      (method === "HEAD" ? index.get(`GET ${p}`) : undefined);

    /**
     * One raw path, looked up under the app's normalisation. This middleware
     * SETTLES before it calls next(), so a match on a path the router then
     * refuses is a payer who paid and got a 404; with the flags on, the
     * loosest index only refuses (looseHit below), it never charges.
     */
    const lookup = (method: string, raw: string): RouteConfig | undefined =>
      inIndex(appIndex, method, norm(raw, strict, caseSensitive));

    // `path` is relative to this middleware's mount, as `req.path` is, except
    // that "" is the mount's root without its slash (see relPath below).
    const gated = (method: string, path: string): RouteConfig | undefined =>
      lookup(method, baseUrl + path) ??
      // Only worth a second spelling when the middleware is actually mounted.
      (baseUrl ? lookup(method, path || "/") : undefined);
    const looseGated = (method: string, path: string): RouteConfig | undefined =>
      inIndex(looseIndex, method, norm(baseUrl + path, false, false)) ??
      (baseUrl ? inIndex(looseIndex, method, norm(path || "/", false, false)) : undefined);

    const method = req.method.toUpperCase();
    // Express 4 leaves req.path null for an absolute-form target such as
    // "GET foo://h/api" once a mount path is cut from it.
    if (typeof req.path !== "string") {
      res.status(400).json({ error: "malformed request target" });
      return;
    }

    // A PAID PATH SPELLED WITH REPEATED SLASHES IS REFUSED, NOT GUESSED.
    //
    // Express 4 drops one slash after a mount path. With this gate on the app
    // and the paid route on `app.use("/api", router)`, GET /api//data found no
    // key here and the router served it as /data: the paid route, free. With
    // this gate mounted at "/api" and the route on the app, the slash was
    // dropped before the gate saw it, so the gate charged and the app answered
    // 404. On Express 5 a Router's paid root answered GET /api// free. Whether
    // a router serves such a path depends on mounts this gate cannot see, so a
    // paid path with repeated slashes in the URL is neither charged nor passed
    // on.
    // Both spellings are checked: the URL as sent (a mount may already have
    // dropped a slash from req.path) and req.path (Express turns "\" into "/").
    const rawPath = String(req.originalUrl ?? req.url ?? "")
      .split(/[?#]/, 1)[0]!
      .replace(/^[a-z][a-z0-9+.-]*:\/\/[^/]*/i, "");
    // At a mount's root Express gives req.path "/" for both /api and /api/;
    // the URL as sent tells them apart. Read as "/", a paid "GET /api" was
    // looked up as /api/ and, with strict routing on, refused in every
    // spelling.
    const relPath = baseUrl && req.path === "/" && !rawPath.endsWith("/") ? "" : req.path;
    const config = gated(method, relPath);
    const collapsed = relPath.replace(/\/{2,}/g, "/");
    // THE APP'S FLAGS MISS, THE LOOSEST READING HITS: REFUSE, NOT PASS ON.
    //
    // With `strict routing` or `case sensitive routing` on the app, this gate on
    // the app and the paid route on an express.Router(), which ignores those
    // flags, GET /api/data/ or /API/data found no key here and the router
    // served the paid route free. The app's own router answers 404 to the same
    // request; anything else after this gate that answers such a spelling (a
    // free route, a parameter route, static files, a redirect to the canonical
    // path) gets 404 now too. The same holds inside a mount: see the note at
    // the top of the handler.
    const looseHit = (strict || caseSensitive) && looseGated(method, collapsed) !== undefined;
    if (
      (rawPath.includes("//") || collapsed !== relPath) &&
      (config ?? gated(method, collapsed) ?? looseHit)
    ) {
      res.status(400).json({ error: "repeated slashes in the path of a paid route" });
      return;
    }
    // A PAID FILE IS NOT FREE UNDER ANOTHER SPELLING OF ITS PATH.
    //
    // express.static decodes percent-escapes and resolves "." and ".." before
    // it reads the file; the router and this gate compare the path as sent.
    // With "GET /files/report.pdf" paid and the file under express.static at
    // /files, GET /files/./report.pdf, /files/x/../report.pdf and
    // /files/%72eport.pdf found no key here and were served free. A path whose
    // canonical form is that of a paid key it does not itself match is
    // refused. It is read loosely: no honest client sends such a path. A path
    // sent in canonical form is left to the checks above, unless the key it
    // matches was written with escapes: for "GET /files/a%5B1%5D.pdf",
    // /files/a[1].pdf is the same file.
    const canonicalHit = (p: string | null, asSent: string): RouteConfig | undefined => {
      if (p === null) return undefined;
      const index = p === asSent ? escapedKeyIndex : canonIndex;
      return inIndex(index, method, norm(p, false, false));
    };
    const canonRel = relPath === "" ? "" : canonicalPath(relPath);
    const canonConfig =
      canonicalHit(canonRel === null ? null : baseUrl + canonRel, baseUrl + collapsed) ??
      (baseUrl ? canonicalHit(canonRel, collapsed) : undefined) ??
      canonicalHit(canonicalPath(rawPath), rawPath.replace(/\/{2,}/g, "/"));
    if (canonConfig !== undefined && canonConfig !== config) {
      res.status(400).json({ error: "non-canonical path for a paid route" });
      return;
    }
    if (!config) {
      if (looseHit) {
        res.status(404).json({ error: "not found" });
        return;
      }
      next();
      return;
    }

    const sigHeader = req.header(HEADER_PAYMENT_SIGNATURE_V2);
    // Every answer this middleware gives on a paid route is per payer: keep it
    // out of shared caches. The paid response itself gets `private` below,
    // merged into whatever Cache-Control the app had set before this gate.
    const appCacheControl = res.getHeader("Cache-Control");
    res.setHeader("Cache-Control", "no-store");
    if (!sigHeader) {
      respond402WithRequired(req, res, config);
      return;
    }

    let decoded: unknown;
    try {
      decoded = decodeBase64Json<unknown>(sigHeader);
    } catch {
      decoded = undefined;
    }
    // Valid JSON may still be null, an array or a primitive; only an object
    // is an envelope. Reading `.accepted` of null threw.
    if (decoded === null || typeof decoded !== "object" || Array.isArray(decoded)) {
      res.status(400).json({ error: "malformed PAYMENT-SIGNATURE header" });
      return;
    }
    const envelope = decoded as PaymentPayloadEnvelope;

    // SECURITY (SEC-1): the `accepted` block lives inside the
    // client-controlled PAYMENT-SIGNATURE envelope and MUST NOT be trusted
    // as the payment requirements. The facilitator is a generic relay that
    // validates the on-ledger transfer against whatever requirements it is
    // handed, so forwarding the client's claim would let an attacker pay
    // `amount: "1"` (or redirect `payTo`) and still unlock the resource.
    // Pin to THIS route's configured accepts[]; on no match respond 402 and
    // never call the facilitator with the client's numbers.
    const requirements = selectServerRequirements(
      config.accepts,
      (envelope as { accepted?: unknown }).accepted
    );
    if (!requirements) {
      respond402WithRequired(
        req,
        res,
        config,
        "payment requirements do not match this resource's price"
      );
      return;
    }

    // THE PAYER MAY LEAVE WHILE WE SETTLE.
    //
    // A settle takes seconds, up to 45, and the payer's connection can close in
    // that time (a proxy timeout, a cancelled tool call). The release listener
    // below is attached after the claim, so a close that came earlier never
    // reached it: the ticket stayed spent, the handler wrote to a dead socket,
    // and a retry of the same payment was refused payment_already_redeemed.
    // Watched from here: gone before /settle, nothing is settled; gone before
    // the handler starts, nothing is claimed (or the claim is given back).
    let clientGone = false;
    res.once("close", () => {
      if (!res.writableEnded) clientGone = true;
    });

    // POST /verify
    let verifyJson: { isValid?: boolean; invalidReason?: string };
    try {
      const r = await fetchImpl(`${facilitatorUrl}/verify`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          x402Version: 2,
          paymentPayload: envelope,
          paymentRequirements: requirements,
        }),
      });
      // A NON-2xx /verify IS NOT A REFUSED PAYMENT. This call had no r.ok
      // check at all — unlike /settle below — so a facilitator 429
      // (`{error:"rate_limited"}`), a 400 on a shape it could not parse, or any
      // 5xx left `isValid` undefined, which is falsy, and the payer was told
      // its payment was invalid. The paying client answers a 402 by minting a
      // BRAND-NEW signed transfer, so the merchant's own overload became the
      // payer's problem: churn on the payer's holdings and a message saying
      // the payment was refused when the facilitator never judged it.
      if (!r.ok) {
        res.status(502).json({
          error:
            "facilitator could not verify the payment — retry the SAME " +
            "payment; nothing was settled",
        });
        return;
      }
      verifyJson = (await r.json()) as typeof verifyJson;
    } catch {
      res.status(502).json({ error: "facilitator unreachable on /verify" });
      return;
    }

    if (!verifyJson.isValid) {
      respond402WithRequired(req, res, config, verifyJson.invalidReason);
      return;
    }
    if (clientGone) return;

    // POST /settle BEFORE running the handler (pre-handler settle).
    let settleJson: {
      success?: boolean;
      errorReason?: string;
      /** Settle updateId — the redemption ticket, see RedeemedStore. */
      transaction?: string;
    } & Record<
      string,
      unknown
    > = {};
    // A PENDING SETTLE IS RETRIED HERE, WITH THE SAME PAYMENT. The facilitator
    // answers `settlement_pending` when the participant took the transfer and
    // its outcome is not known yet; asking again lets it look the submission
    // up. Answering the payer 402 would read as "rejected" and invite a second
    // payment while the first can still commit.
    for (let attempt = 0; ; attempt++) {
      try {
        const r = await fetchImpl(`${facilitatorUrl}/settle`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            x402Version: 2,
            paymentPayload: envelope,
            paymentRequirements: requirements,
          }),
        });
        // STATUS FIRST, BODY SECOND. This check used to sit AFTER `r.json()`,
        // inside the same try — so any non-2xx whose body is not JSON threw, and
        // control jumped to the catch below. The facilitator itself always
        // answers JSON, which is why that looked safe in-repo; the bodies that
        // are not JSON come from the reverse proxy this repo ships in front of it
        // (ops/*/Caddyfile), which returns a bodyless 502/504 when an upstream
        // read times out. A settle can spend 45s in the submit, past that
        // timeout, while the ExecuteSubmission goes on to commit on-ledger.
        //
        // So the one branch written to say "do NOT pay again" was exactly the
        // branch that could not run in the case it was written for.
        //
        // A non-2xx /settle is NOT a payment rejection — the facilitator answers
        // 503 when it cannot tell whether the submission committed. Turning that
        // into 402 tells the payer "rejected", and the paying client responds to
        // a 402 by minting a BRAND-NEW signed transfer over the payer's remaining
        // holdings: a second real payment for one purchase, which no dedup
        // catches because the commandId and hash are both fresh. Answer a
        // transport error instead, so the caller retries the SAME envelope.
        // The one exception is the scheme's pending answer, which travels as a
        // 503 with its body so that older middleware lands here; it is retried
        // below like a 200 one.
        if (!r.ok) {
          const pending = r.status === 503 ? await pendingBody(r) : null;
          if (!pending) {
            res
              .status(502)
              .json({
                error:
                  "facilitator could not confirm the payment — do NOT pay again; " +
                  "retry the same payment or check the merchant's receipt",
              });
            return;
          }
          settleJson = pending as typeof settleJson;
        } else {
          settleJson = (await r.json()) as typeof settleJson;
        }
      } catch (err) {
        // A THROW HERE COVERS TWO DIFFERENT FACTS, and they must not be merged.
        //
        // My first pass at this said "we know exactly as much: nothing" and gave
        // both the ambiguous wording. That was wrong in the safe-looking
        // direction: a connection that was never established PROVES the request
        // never left this process, so no /settle exists and nothing can have been
        // submitted. Reporting that as "may already have settled" turns an
        // ordinary outage — a facilitator redeploy, a wrong URL, a dead bridge —
        // into per-payer manual reconciliation of a payment that was never sent.
        //
        // Anything else — a reset, a read timeout, an abort — happened with a
        // socket open and the request possibly delivered. That one is genuinely
        // unknown and keeps the do-NOT-pay-again wording. `connectionNeverEstablished`
        // only ever makes the safe claim: unrecognised failures fall through here.
        if (connectionNeverEstablished(err)) {
          res.status(502).json({
            error:
              "facilitator unreachable on /settle — the request never left this " +
              "server, so nothing was settled; retry the same payment",
          });
          return;
        }
        res.status(502).json({
          error:
            "facilitator could not confirm the payment — do NOT pay again; " +
            "retry the same payment or check the merchant's receipt",
        });
        return;
      }
      if (settleJson.errorReason === "settlement_pending" && attempt < PENDING_RETRY_MS.length) {
        await new Promise((r) => setTimeout(r, PENDING_RETRY_MS[attempt]));
        if (clientGone) return;
        continue;
      }
      break;
    }
    if (settleJson.errorReason === "settlement_pending") {
      res.status(502).json({
        error:
          "the payment is pending on the ledger — do NOT pay again; retry the " +
          "same payment shortly",
      });
      return;
    }

    // A REPEAT OF A SETTLED PAYMENT. A facilitator that follows the scheme does
    // not answer it with success: it refuses with the updateId, so a server
    // that keeps no record of its own cannot serve one payment many times.
    // This middleware does keep one: the updateId is the payment's ticket, and
    // the claim below serves it once, or again only after a delivery that
    // failed gave the ticket back. Without a store there is nothing to decide
    // with, and it stays a refusal.
    const alreadySettled =
      settleJson.success !== true &&
      settleJson.errorReason === "invalid_exact_canton_payment_already_settled" &&
      typeof settleJson.transaction === "string" &&
      settleJson.transaction.length > 0 &&
      redeemed !== null;
    if (alreadySettled) {
      const { transaction, network, payer } = settleJson;
      settleJson = {
        success: true,
        transaction: transaction as string,
        ...(network !== undefined ? { network } : {}),
        ...(payer !== undefined ? { payer } : {}),
      };
    }

    // Positive confirmation only (SEC-1 / M1): a missing or non-`true`
    // `success` must NOT fall through to delivering the resource.
    if (settleJson.success !== true) {
      res.status(402).json({ error: settleJson.errorReason ?? "settle failed" });
      return;
    }
    // Settled, but nobody is there to serve: claim nothing, so a retry of the
    // same payment is still served.
    if (clientGone) return;

    // ONE PAYMENT, ONE DELIVERY. `settleJson.transaction` is the settle
    // updateId; the facilitator returns the SAME one for every replay of the
    // same signed bytes, so claiming it here is what separates "this payment
    // bought this response" from "this payment buys responses forever".
    // A SETTLE WE CANNOT IDENTIFY IS NOT ONE WE CAN DELIVER AGAINST.
    //
    // The ticket is the settle updateId, and `?? ""` used to turn a missing one
    // into the SHARED key "". The first such payment claimed "", and every
    // later payment from anyone was then refused `payment_already_redeemed`
    // after settling on-ledger: paid, undelivered, and told to pay again,
    // permanently.
    //
    // This is not a hypothetical about our own facilitator. `facilitatorUrl` is
    // merchant-configured and x402's whole model is that the SERVER picks the
    // facilitator, so a published middleware may not assume the remote fills a
    // field. It cannot dedup what it cannot name, and the honest answer is to
    // say the response was non-conforming rather than open a replay window or
    // burn a key everyone shares.
    if (redeemed !== null) {
      const ticket = settleJson.transaction;
      if (typeof ticket !== "string" || ticket.length === 0) {
        res.status(502).json({
          error:
            "facilitator reported success without a transaction id — the " +
            "payment cannot be identified, so this server will not deliver " +
            "against it; the payment may have settled, do NOT pay again",
        });
        return;
      }
      const first = await redeemed.claim(ticket);
      if (!first) {
        respond402WithRequired(req, res, config, "payment_already_redeemed");
        return;
      }
      if (clientGone) {
        void Promise.resolve(redeemed.release?.(ticket)).catch(() => {
          /* the payer left before the handler started; nothing to keep */
        });
        return;
      }

      // GIVE THE TICKET BACK WHEN NOTHING WAS DELIVERED.
      //
      // The claim above is what stops one payment buying a resource forever.
      // But it happens BEFORE next(), so if the merchant's handler throws, or
      // dies, or the process is killed mid-response, the payer has paid
      // on-ledger, received nothing, and the ticket that identifies their
      // payment is spent — a retry with the same signed bytes is answered
      // `payment_already_redeemed` forever. The money is gone and only an
      // operator can put it right.
      //
      // The sibling package already fixed exactly this: next/src/index.ts
      // wraps the handler in try/catch and releases before re-throwing. That
      // fix never reached here — express was the arm nobody checked, and it is
      // the arm most integrators actually run.
      //
      // Express cannot wrap `next()` the same way, because the handler runs
      // after this function returns. The observable equivalent is the response
      // itself: `close` fires exactly once, on every path including a socket
      // that died mid-write. "Not delivered" is a 5xx (what express's own error
      // handler sends when a handler throws, the case next's catch covers) or a
      // request the router did not serve (below). A 2xx, a 3xx, and a 4xx the
      // handler chose deliberately all keep the ticket claimed: the merchant
      // answered, and re-delivery is not owed.
      //
      // A connection that drops after the handler started, before any header
      // went out (the payer's client or a proxy timing out on a slow handler),
      // gives the ticket back ONCE per payment. It used to come back every
      // time, and node does not stop a handler when its client leaves: re-sending
      // the same header and closing early ran the handler again for one payment,
      // as often as the facilitator answered the repeat. Never giving it back
      // cost an honest payer behind a 60 s proxy timeout the whole payment. One
      // return bounds a payment to two runs of the handler and keeps that retry.
      //
      // A release that itself fails must not become the visible failure — the
      // payer is no worse off than before this existed, same rule as next.
      if (redeemed.release) {
        let settled = false; // this listener, not the payment: fire once
        res.on("close", () => {
          if (settled) return;
          settled = true;
          // `req.route` is the discriminator between the two very different
          // things that both arrive as 404. MEASURED, not assumed:
          //   handler answered 200        -> req.route set
          //   handler CHOSE 404           -> req.route set    (delivered: the
          //                                  merchant answered "no such record")
          //   router matched nothing      -> req.route UNSET  (not delivered:
          //                                  we charged for a spelling this app
          //                                  does not serve, and the payer got
          //                                  a 404 for their money)
          // Without it the second case reads as delivery and the payment is
          // spent on nothing — which is exactly the hole the loose route index
          // can open when it charges for a path the router then declines.
          //
          // So a 404 from middleware (`req.route` unset) returns the ticket, and
          // so does a middleware redirect that fetch follows back through this
          // gate, where the followed request claims it or, with the routing
          // flags on, is refused (express.static's redirect from /files/docs to
          // /files/docs/). Any other 3xx, and a route handler's, keeps it spent:
          // a redirect elsewhere may be the delivery itself (a signed download
          // URL).
          const followed = followedRequest(res, method, req.originalUrl, baseUrl);
          const notServed =
            req.route === undefined &&
            (res.statusCode === 404 ||
              (followed !== undefined &&
                (gated(followed.method, followed.path) !== undefined ||
                  ((strict || caseSensitive) && looseGated(followed.method, followed.path) !== undefined))));
          const notDelivered = res.headersSent
            ? res.statusCode >= 500 || notServed
            : firstDropBeforeHeaders(ticket);
          if (!notDelivered) return;
          void Promise.resolve(redeemed.release?.(ticket)).catch(() => {
            /* keep the merchant's failure, not ours */
          });
        });
      }
    }

    res.setHeader(HEADER_PAYMENT_RESPONSE_V2, encodeBase64Json(settleJson));
    // PAID, SO PRIVATE. The handler or express.static sets its own
    // Cache-Control after this point ("public, max-age=86400" for a file), and
    // a shared cache in front (Cloudflare, nginx) then served the paid body to
    // the next caller without payment. `private` is merged in as the headers go
    // out, whatever the handler set; the gate's own no-store goes.
    if (appCacheControl === undefined) res.removeHeader("Cache-Control");
    else res.setHeader("Cache-Control", appCacheControl);
    privateWhenSent(res);
    next();
  };

  // Express 4 does not await a middleware's promise, so a throw in the handler
  // above was an unhandled rejection that ended the merchant's process. Pass
  // it to next() as an Error: next(undefined) or next("route") would continue
  // to the paid route. The returned promise never rejects, so Express 5 cannot
  // call next a second time.
  return function middleware(req, res, next) {
    return Promise.resolve(handle(req, res, next)).catch((err: unknown) => {
      next(err instanceof Error ? err : new Error("payment middleware failed", { cause: err }));
    });
  };
}

/**
 * True the first time a payment's ticket is given back for a connection that
 * dropped before any header went out (see the release listener); false after.
 * Remembered per process for a day, long enough to outlive any payment's
 * deadline; bounded so a flood of payments cannot grow it without limit.
 */
const dropReleases = new Map<string, number>();
function firstDropBeforeHeaders(ticket: string): boolean {
  const now = Date.now();
  if (dropReleases.has(ticket)) return false;
  if (dropReleases.size >= 100_000) {
    for (const [t, at] of dropReleases) if (now - at > 86_400_000) dropReleases.delete(t);
    if (dropReleases.size >= 100_000) return false;
  }
  dropReleases.set(ticket, now);
  return true;
}

/**
 * Merge `private` into the response's Cache-Control as its headers are sent,
 * whatever the handler set before (as upstream's withPrivateCacheControl).
 */
function privateWhenSent(res: Response): void {
  const writeHead = res.writeHead;
  let merged = false;
  const merge = (value: unknown): string => {
    const v = value == null ? "" : String(value);
    if (!v) return "private";
    return v.split(",").some((d) => d.trim().toLowerCase() === "private") ? v : `${v}, private`;
  };
  res.writeHead = function (this: Response, ...args: unknown[]) {
    if (!merged) {
      // writeHead(status[, message][, headers]): headers passed here win over
      // setHeader, as an object or as a flat [name, value, ...] array (a proxy
      // passing upstream rawHeaders). Copied, never edited: the handler may
      // pass one shared object to free routes too.
      const i = args.length - 1;
      const last = args[i];
      if (Array.isArray(last)) {
        const flat = [...(last as unknown[])];
        let found = false;
        for (let k = 0; k + 1 < flat.length; k += 2) {
          if (String(flat[k]).toLowerCase() === "cache-control") {
            flat[k + 1] = merge(flat[k + 1]);
            found = true;
          }
        }
        if (!found) flat.push("Cache-Control", merge(res.getHeader("Cache-Control")));
        args[i] = flat;
      } else if (last && typeof last === "object") {
        const headers = { ...(last as Record<string, unknown>) };
        const key = Object.keys(headers).find((k) => k.toLowerCase() === "cache-control");
        if (key) headers[key] = merge(headers[key]);
        else res.setHeader("Cache-Control", merge(res.getHeader("Cache-Control")));
        args[i] = headers;
      } else {
        res.setHeader("Cache-Control", merge(res.getHeader("Cache-Control")));
      }
    }
    // Marked only once the headers really went: a writeHead that threw (an
    // odd-length array) leaves the next call to merge again.
    const sent = writeHead.apply(this, args as Parameters<Response["writeHead"]>);
    merged = true;
    return sent;
  } as Response["writeHead"];
}

/**
 * The path as express.static reads it: percent-escapes decoded, "\\" as "/",
 * "." and ".." resolved, repeated slashes collapsed, a trailing slash kept.
 * Null when an escape is malformed; express.static answers 400 to those.
 */
function canonicalPath(path: string): string | null {
  let decoded: string;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    return null;
  }
  const segments = decoded.replace(/\\/g, "/").split("/");
  const out: string[] = [];
  for (const seg of segments) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") out.pop();
    else out.push(seg);
  }
  const last = segments[segments.length - 1];
  const dir = segments.length > 1 && (last === "" || last === "." || last === "..");
  return "/" + out.join("/") + (dir && out.length > 0 ? "/" : "");
}

/**
 * The method and mount-relative path of the request fetch makes to follow
 * this redirect, or undefined when fetch does not follow it or the target
 * leaves this origin or `baseUrl`.
 */
function followedRequest(
  res: Response,
  method: string,
  originalUrl: string,
  baseUrl: string
): { method: string; path: string } | undefined {
  const status = res.statusCode;
  let follow: string;
  if (status === 301 || status === 302) follow = method === "POST" ? "GET" : method;
  else if (status === 303) follow = method === "HEAD" ? "HEAD" : "GET";
  else if (status === 307 || status === 308) follow = method;
  else return undefined;
  const location = res.getHeader("location");
  if (typeof location !== "string") return undefined;
  let self: URL;
  let target: URL;
  try {
    // The payer sets Host, so this origin is a placeholder only a relative
    // Location can resolve to.
    self = new URL(`http://x402.invalid${originalUrl}`);
    target = new URL(location, self);
  } catch {
    return undefined;
  }
  if (target.origin !== self.origin || !target.pathname.startsWith(baseUrl)) return undefined;
  const rest = target.pathname.slice(baseUrl.length);
  if (rest !== "" && !rest.startsWith("/")) return undefined;
  // "" is the mount's root without its slash, as the handler reads it.
  return { method: follow, path: rest };
}

function respond402WithRequired(
  req: Request,
  res: Response,
  config: RouteConfig,
  errorReason?: string
): void {
  const host = req.get("host") ?? "unknown";
  const proto = req.protocol || "http";
  const required = {
    x402Version: 2,
    error: errorReason ?? "PAYMENT-SIGNATURE header required",
    resource: {
      url: `${proto}://${host}${req.originalUrl}`,
      ...(config.description !== undefined
        ? { description: config.description }
        : {}),
      ...(config.mimeType !== undefined ? { mimeType: config.mimeType } : {}),
    },
    accepts: config.accepts,
    extensions: {},
  };
  res.setHeader(HEADER_PAYMENT_REQUIRED_V2, encodeBase64Json(required));
  res.status(402).json({});
}
