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
 * payment lands before the resource is delivered (merchant-friendly;
 * losses are on the merchant only if the handler crashes after a
 * successful settle).
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
  type PaymentRequirements,
  type X402ResourceInfo,
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

export function withCantonPayment<TCtx = unknown>(
  handler: CantonPaymentHandler<TCtx>,
  config: WithCantonPaymentConfig
): CantonPaymentHandler<TCtx> {
  const fetchImpl = config.fetch ?? globalThis.fetch;
  const facilitatorUrl = config.facilitatorUrl.replace(/\/$/, "");

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
      verifyJson = (await r.json()) as typeof verifyJson;
    } catch {
      return jsonError(502, "facilitator unreachable on /verify");
    }

    if (!verifyJson.isValid) {
      return paymentRequired(request, config, verifyJson.invalidReason);
    }

    // /settle (pre-handler)
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
      return jsonError(502, "facilitator unreachable on /settle");
    }

    // Positive confirmation only (SEC-1 / M1): a missing or non-`true`
    // `success` must NOT fall through to running the handler.
    if (settleJson.success !== true) {
      return jsonError(402, settleJson.errorReason ?? "settle failed");
    }

    // Run the handler and clone its response to attach the
    // PAYMENT-RESPONSE header without consuming the body stream.
    const original = await handler(request, context);
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
