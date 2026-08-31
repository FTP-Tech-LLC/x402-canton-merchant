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
 * ORIGINAL updateId and `success:true`, so instance #2 sees a fresh, valid
 * ticket its own Map has never held.
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

  return async function middleware(req, res, next) {
    // THE APP'S ROUTING FLAGS DO NOT GOVERN A SUB-ROUTER.
    //
    // `strict routing` and `case sensitive routing` configure the app's OWN
    // base router; an `express.Router()` instance is built by the raw `router`
    // package with strict:false/caseSensitive:false and never consults the app.
    // So a merchant who turns strict routing on and puts the paid route on a
    // Router — the standard way to organise an express app, and the shape the
    // mount fix above exists to serve — got a sub-router still matching loosely
    // while this gate had stopped normalising. Measured, paid route on a Router
    // mounted at /api with `strict routing` ON:
    //
    //   GET /api/data/   ->  200 {"data":"premium-payload"}
    //
    // The gate cannot know which router will serve the request, so it must not
    // be stricter than the loosest one that could. Every spelling is therefore
    // looked up under the app's own normalisation AND under the loosest one.
    // Matching too much costs a 402 on a path a strict app would have 404'd —
    // visible, and not a resource given away.
    const strict = req.app?.get?.("strict routing") === true;
    const caseSensitive = req.app?.get?.("case sensitive routing") === true;
    const mode = `${strict}|${caseSensitive}`;
    if (cachedIndex === null || cachedMode !== mode) {
      cachedIndex = buildIndex(strict, caseSensitive);
      cachedMode = mode;
    }
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
    const method = req.method.toUpperCase();
    const inIndex = (
      index: Map<string, RouteConfig>,
      p: string
    ): RouteConfig | undefined =>
      index.get(`${method} ${p}`) ??
      // Express answers HEAD from the GET handler, so a HEAD must meet the same
      // gate; an explicitly configured HEAD route still wins.
      (method === "HEAD" ? index.get(`GET ${p}`) : undefined);

    /**
     * One raw path, looked up under the app's normalisation and — only when we
     * are inside a mount — under the loosest one too.
     *
     * The mount test is what keeps the loose reading from costing money. This
     * middleware SETTLES before it calls next(), so a match on a path the
     * router then refuses is not a spurious 402: it is a payer who paid and got
     * a 404. Measured, `strict routing` ON and the paid route on the app's own
     * base router:
     *
     *   GET /api/data/  ->  404, and the settle had already happened
     *
     * On the app's base router the app's flags DO describe the router that
     * serves, so matching more than they allow can only ever match something
     * that will 404. Inside a mount they do not — an express.Router() is built
     * loose and ignores them — which is the hole the loose reading exists to
     * close. So it is applied exactly there and nowhere else.
     */
    const lookup = (raw: string): RouteConfig | undefined =>
      inIndex(cachedIndex!, norm(raw, strict, caseSensitive)) ??
      (req.baseUrl ? inIndex(looseIndex, norm(raw, false, false)) : undefined);

    const config =
      lookup(req.baseUrl + req.path) ??
      // Only worth a second spelling when the middleware is actually mounted.
      (req.baseUrl ? lookup(req.path) : undefined);
    if (!config) {
      next();
      return;
    }

    const sigHeader = req.header(HEADER_PAYMENT_SIGNATURE_V2);
    if (!sigHeader) {
      respond402WithRequired(req, res, config);
      return;
    }

    let envelope: PaymentPayloadEnvelope;
    try {
      envelope = decodeBase64Json<PaymentPayloadEnvelope>(sigHeader);
    } catch {
      res.status(400).json({ error: "malformed PAYMENT-SIGNATURE header" });
      return;
    }

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

    // POST /settle BEFORE running the handler (pre-handler settle).
    let settleJson: {
      success?: boolean;
      errorReason?: string;
      /** Settle updateId — the redemption ticket, see RedeemedStore. */
      transaction?: string;
    } & Record<
      string,
      unknown
    >;
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
      if (!r.ok) {
        res
          .status(502)
          .json({
            error:
              "facilitator could not confirm the payment — do NOT pay again; " +
              "retry the same payment or check the merchant's receipt",
          });
        return;
      }
      settleJson = (await r.json()) as typeof settleJson;
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

    // Positive confirmation only (SEC-1 / M1): a missing or non-`true`
    // `success` must NOT fall through to delivering the resource.
    if (settleJson.success !== true) {
      res.status(402).json({ error: settleJson.errorReason ?? "settle failed" });
      return;
    }

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
      // that died mid-write. Two conditions mean "not delivered":
      //   * nothing was ever sent (`!res.headersSent`) — the handler threw
      //     before writing, or the connection dropped;
      //   * a 5xx went out — which is what express's own error handler sends
      //     when a handler throws, i.e. the precise case next's catch covers.
      // A 2xx, a 3xx, and a 4xx the handler chose deliberately all keep the
      // ticket claimed: the merchant answered, and re-delivery is not owed.
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
          const answered = req.route !== undefined;
          const delivered = res.headersSent && res.statusCode < 500 && answered;
          if (delivered) return;
          void Promise.resolve(redeemed.release?.(ticket)).catch(() => {
            /* keep the merchant's failure, not ours */
          });
        });
      }
    }

    res.setHeader(HEADER_PAYMENT_RESPONSE_V2, encodeBase64Json(settleJson));
    next();
  };
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
