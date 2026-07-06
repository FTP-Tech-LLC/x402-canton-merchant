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
  type PaymentRequirements,
  type X402ResourceInfo,
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
}

interface PaymentPayloadEnvelope {
  x402Version: number;
  scheme: string;
  accepted: PaymentRequirements;
  resource: X402ResourceInfo;
  payload: unknown;
  extensions?: Record<string, unknown>;
}

export function cantonPaymentMiddleware(
  options: CantonPaymentMiddlewareOptions
): RequestHandler {
  const fetchImpl = options.fetch ?? globalThis.fetch;
  const facilitatorUrl = options.facilitatorUrl.replace(/\/$/, "");

  // Config-time guard (audit L1): on every advertised entry, asset must agree
  // with extra.instrumentId — otherwise the operator misconfig is silent at
  // runtime (the facilitator validates instrumentId and ignores asset on the
  // CIP-56 path). Fail fast at setup.
  for (const route of Object.values(options.routes)) {
    for (const r of route.accepts) assertAssetInstrumentConsistency(r);
  }

  return async function middleware(req, res, next) {
    const routeKey = `${req.method} ${req.path}`;
    const config = options.routes[routeKey];
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
    let settleJson: { success?: boolean; errorReason?: string } & Record<
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
      settleJson = (await r.json()) as typeof settleJson;
    } catch {
      res.status(502).json({ error: "facilitator unreachable on /settle" });
      return;
    }

    // Positive confirmation only (SEC-1 / M1): a missing or non-`true`
    // `success` must NOT fall through to delivering the resource.
    if (settleJson.success !== true) {
      res.status(402).json({ error: settleJson.errorReason ?? "settle failed" });
      return;
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
