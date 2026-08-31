/**
 * withCantonPayment — Next.js App Router Route-Handler wrapper that
 * gates a route behind Canton x402 payment via a facilitator.
 *
 * Usage:
 *
 *   // app/api/data/route.ts
 *   import { withCantonPayment } from "@ftptech/x402-canton-next";
 *
 *   export const GET = withCantonPayment(
 *     async (_req) => Response.json({ data: "premium" }),
 *     {
 *       accepts: [paymentRequirements],
 *       facilitatorUrl: process.env.FACILITATOR_URL!,
 *       description: "Premium market data",
 *       mimeType: "application/json",
 *     }
 *   );
 *
 * Per-request flow (same semantics as @ftptech/x402-canton-express, just
 * adapted to Next.js's Web-Fetch handler signature):
 *
 *   1. Missing PAYMENT-SIGNATURE → 402 + PAYMENT-REQUIRED header.
 *   2. PAYMENT-SIGNATURE present → POST {facilitatorUrl}/verify.
 *      isValid: false → 402 + invalidReason in PAYMENT-REQUIRED.
 *   3. isValid: true → POST {facilitatorUrl}/settle.
 *      success: false → 402 + errorReason.
 *   4. success: true → call the wrapped handler. Clone its response
 *      and attach PAYMENT-RESPONSE with the settlement metadata.
 *
 * Settled pre-handler — same trade-off as the Express middleware:
 * payment lands before the resource is delivered. That is
 * merchant-friendly, and the cost falls on the PAYER, not the
 * merchant: if the handler crashes after a successful settle, the
 * money is already on-ledger and the resource never arrived. (An
 * earlier version of this sentence said the losses were the
 * merchant's, which is backwards — the merchant has the money.)
 * This wrapper therefore gives the redemption ticket back on a
 * handler throw, so the payer can retry the SAME payment; see the
 * handler call below.
 *
 * No dependency on `next`. Uses plain `Request`/`Response`/`Headers`
 * from the Web Fetch API — Next.js's `NextRequest` extends `Request`
 * so this wrapper composes cleanly without a framework version pin.
 */

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

export interface WithCantonPaymentConfig {
  /** Entries advertised in 402.accepts[]. */
  accepts: PaymentRequirements[];
  /** Base URL of the Canton x402 facilitator (no trailing slash). */
  facilitatorUrl: string;
  /** Description echoed in PAYMENT-REQUIRED.resource.description. */
  description?: string;
  /** Content-Type the protected handler returns. */
  mimeType?: string;
  /** Injectable fetch — defaults to globalThis.fetch. */
  fetch?: typeof globalThis.fetch;
  /**
   * One payment, one delivery. A settle updateId may be redeemed ONCE; a replay
   * is answered 402 rather than served again. Defaults to an in-process store —
   * pass a SHARED implementation when running more than one instance, or each
   * instance will serve the same replay once. Explicit `null` restores the old
   * unlimited-redemption behaviour.
   */
  redeemed?: RedeemedStore | null;
}

export type CantonPaymentHandler<TCtx = unknown> = (
  request: Request,
  context?: TCtx
) => Promise<Response> | Response;

interface PaymentPayloadEnvelope {
  x402Version: number;
  scheme: string;
  accepted: PaymentRequirements;
  resource: X402ResourceInfo;
  payload: unknown;
  extensions?: Record<string, unknown>;
}

/**
 * ONE default store for the whole process, not one per wrapper.
 *
 * App Router applies withCantonPayment once per route file, and the default
 * store used to be built inside the call — so each route remembered its own
 * redemptions. A settled payment was therefore redeemable once PER ROUTE: pay
 * for /api/a, then replay the identical PAYMENT-SIGNATURE header at /api/b and
 * /api/c, and every equally-priced route unlocked for that one payment. The
 * ticket has to be global, because the thing it identifies — a settle
 * updateId — is global.
 *
 * A merchant on several instances still needs a SHARED store (Redis, a table)
 * and can pass one; this only fixes the single-process default, which used to
 * be wrong even there.
 */
let processRedeemed: RedeemedStore | undefined;

export function withCantonPayment<TCtx = unknown>(
  handler: CantonPaymentHandler<TCtx>,
  config: WithCantonPaymentConfig
): CantonPaymentHandler<TCtx> {
  const fetchImpl = config.fetch ?? globalThis.fetch;
  const facilitatorUrl = config.facilitatorUrl.replace(/\/$/, "");
  const redeemed =
    config.redeemed === null
      ? null
      : (config.redeemed ??
        (processRedeemed ??= createInMemoryRedeemedStore()));

  // Config-time guard (audit L1): asset must agree with extra.instrumentId on
  // every advertised entry (the facilitator validates instrumentId and ignores
  // asset on the CIP-56 path). Fail fast at setup.
  for (const r of config.accepts) assertAssetInstrumentConsistency(r);

  return async function withCantonPaymentHandler(request, context) {
    const sigHeader = request.headers.get(HEADER_PAYMENT_SIGNATURE_V2);
    if (!sigHeader) {
      return paymentRequired(request, config);
    }

    let envelope: PaymentPayloadEnvelope;
    try {
      envelope = decodeBase64Json<PaymentPayloadEnvelope>(sigHeader);
    } catch {
      return jsonError(400, "malformed PAYMENT-SIGNATURE header");
    }

    // SECURITY (SEC-1): never trust the client-supplied `accepted` block as
    // the payment requirements — pin to this route's configured accepts[].
    // Forwarding the client's claim would let an attacker pay `amount: "1"`
    // (or redirect `payTo`) and still unlock the resource. No match → 402,
    // and never call the facilitator with the client's numbers.
    const requirements = selectServerRequirements(
      config.accepts,
      (envelope as { accepted?: unknown }).accepted
    );
    if (!requirements) {
      return paymentRequired(
        request,
        config,
        "payment requirements do not match this resource's price"
      );
    }

    // /verify
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
      // See the express middleware: a non-2xx /verify is the facilitator
      // failing to judge, not a refused payment, and answering 402 makes the
      // paying client mint a brand-new signed transfer.
      if (!r.ok) {
        return jsonError(
          502,
          "facilitator could not verify the payment — retry the SAME " +
            "payment; nothing was settled"
        );
      }
      verifyJson = (await r.json()) as typeof verifyJson;
    } catch {
      return jsonError(502, "facilitator unreachable on /verify");
    }

    if (!verifyJson.isValid) {
      return paymentRequired(request, config, verifyJson.invalidReason);
    }

    // /settle (pre-handler)
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
      // STATUS FIRST, BODY SECOND — see the express middleware for why: this
      // check used to sit after `r.json()`, so a non-2xx with a non-JSON body
      // (the bodyless 502/504 the shipped reverse proxy returns on an upstream
      // read timeout) threw past the guard into the catch.
      //
      // A non-2xx /settle means the facilitator could not determine whether the
      // submission committed. Answering 402 makes the paying client mint a
      // SECOND real payment, so answer a transport error and let the caller
      // retry the SAME envelope.
      if (!r.ok) {
        return jsonError(
          502,
          "facilitator could not confirm the payment — do NOT pay again; " +
            "retry the same payment or check the merchant's receipt"
        );
      }
      settleJson = (await r.json()) as typeof settleJson;
    } catch (err) {
      // See the express middleware: a connection that was never established
      // PROVES the request never left this server, so nothing was settled and a
      // retry is free. Everything else happened with a socket open and the
      // request possibly delivered — that one stays ambiguous.
      if (connectionNeverEstablished(err)) {
        return jsonError(
          502,
          "facilitator unreachable on /settle — the request never left this " +
            "server, so nothing was settled; retry the same payment"
        );
      }
      return jsonError(
        502,
        "facilitator could not confirm the payment — do NOT pay again; " +
          "retry the same payment or check the merchant's receipt"
      );
    }

    // Positive confirmation only (SEC-1 / M1): a missing or non-`true`
    // `success` must NOT fall through to running the handler.
    if (settleJson.success !== true) {
      return jsonError(402, settleJson.errorReason ?? "settle failed");
    }

    // ONE PAYMENT, ONE DELIVERY. The facilitator returns the SAME updateId for
    // every replay of the same signed bytes, so claiming it here is what
    // separates "this payment bought this response" from "this payment buys
    // responses until executeBefore passes".
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
        return jsonError(
          502,
          "facilitator reported success without a transaction id — the " +
            "payment cannot be identified, so this route will not deliver " +
            "against it; the payment may have settled, do NOT pay again"
        );
      }
      const first = await redeemed.claim(ticket);
      if (!first) {
        return paymentRequired(request, config, "payment_already_redeemed");
      }
    }

    // Run the handler and clone its response to attach the
    // PAYMENT-RESPONSE header without consuming the body stream.
    //
    // A THROW HERE MEANS NOTHING WAS DELIVERED, so the ticket claimed a moment
    // ago must go back. Otherwise the payer — who has already paid on-ledger —
    // gets a 500 and is then refused `payment_already_redeemed` on every retry
    // of that same payment: paid, unserved, and permanently unservable.
    //
    // Safe precisely because this wrapper AWAITS a Response object: a throw
    // means the handler returned nothing and the client has been sent nothing.
    // The Express sibling cannot do this — it hands off via `next()` and never
    // awaits, so a route there may throw after writing bytes to `res`.
    //
    // The error is re-thrown unchanged. Swallowing it would turn a merchant
    // bug into a silent success, and Next's own error handling is the right
    // place for it.
    let original: Response;
    try {
      original = await handler(request, context);
    } catch (err) {
      if (redeemed?.release) {
        // A store that fails to release must not replace the merchant's real
        // error with its own; the payer is no worse off than before this fix.
        try {
          await redeemed.release(settleJson.transaction as string);
        } catch {
          /* keep the original failure */
        }
      }
      throw err;
    }
    const headers = new Headers(original.headers);
    headers.set(HEADER_PAYMENT_RESPONSE_V2, encodeBase64Json(settleJson));
    return new Response(original.body, {
      status: original.status,
      statusText: original.statusText,
      headers,
    });
  };
}

function paymentRequired(
  request: Request,
  config: WithCantonPaymentConfig,
  errorReason?: string
): Response {
  const url = request.url;
  const required = {
    x402Version: 2,
    error: errorReason ?? "PAYMENT-SIGNATURE header required",
    resource: {
      url,
      ...(config.description !== undefined
        ? { description: config.description }
        : {}),
      ...(config.mimeType !== undefined ? { mimeType: config.mimeType } : {}),
    },
    accepts: config.accepts,
    extensions: {},
  };
  const headers = new Headers({ "Content-Type": "application/json" });
  headers.set(HEADER_PAYMENT_REQUIRED_V2, encodeBase64Json(required));
  return new Response("{}", { status: 402, headers });
}

function jsonError(status: number, error: string): Response {
  return new Response(JSON.stringify({ error }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}
