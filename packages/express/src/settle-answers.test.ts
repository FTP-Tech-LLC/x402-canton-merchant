/**
 * The facilitator's scheme answers (specs/scheme_exact_canton.md): a repeat of
 * a settled payment is refused with its updateId, and an unknown outcome is
 * `settlement_pending`. Express 4 and 5, every paid request with a payment.
 */
import { describe, it, expect, vi } from "vitest";
import { createRequire } from "node:module";
import request from "supertest";
import express5 from "express";
import {
  HEADER_PAYMENT_RESPONSE_V2,
  HEADER_PAYMENT_SIGNATURE_V2,
  createInMemoryRedeemedStore,
  decodeBase64Json,
  encodeBase64Json,
  type PaymentRequirements,
} from "@ftptech/x402-canton-core";
import { cantonPaymentMiddleware } from "./index.js";

const express4 = createRequire(import.meta.url)("express4") as typeof express5;
const PAYER = "agent::1220abc";

function requirements(): PaymentRequirements {
  return {
    scheme: "exact",
    network: "canton:devnet",
    amount: "1000000000",
    asset: "canton-coin",
    payTo: "merchant::1220m",
    maxTimeoutSeconds: 60,
    extra: {
      assetTransferMethod: "transfer-factory",
      feePayer: "ftp_facilitator::1220fff",
      synchronizerId: "global-domain::1220xyz",
      instrumentId: { admin: "ftp_facilitator::1220fff", id: "CC" },
      executeBeforeSeconds: 120,
    },
  };
}

const signature = encodeBase64Json({
  x402Version: 2,
  accepted: requirements(),
  payload: { assetTransferMethod: "transfer-factory" },
});

let seq = 0;

/** /settle answers from `answers` in turn, the last one forever: a body, or
 *  [status, body]. */
type Answer = Record<string, unknown> | [number, Record<string, unknown>];
function facilitator(answers: Answer[]) {
  let i = 0;
  return vi.fn(async (input: RequestInfo | URL) => {
    if (String(input).endsWith("/verify")) return Response.json({ isValid: true, payer: PAYER });
    const a = answers[Math.min(i, answers.length - 1)]!;
    i += 1;
    return Array.isArray(a) ? Response.json(a[1], { status: a[0] }) : Response.json(a);
  });
}
const settles = (f: ReturnType<typeof facilitator>) => f.mock.calls.filter((c) => String(c[0]).endsWith("/settle")).length;

const ok = (id: string) => ({ success: true, transaction: id, payer: PAYER, network: "canton:devnet" });
const already = (id: string) => ({
  success: false,
  errorReason: "invalid_exact_canton_payment_already_settled",
  transaction: id,
  network: "canton:devnet",
  payer: PAYER,
});
const pendingBody = { success: false, errorReason: "settlement_pending", transaction: "ab".repeat(32), network: "canton:devnet" };
/** As our facilitator sends it: 503 with the scheme's body. */
const pending: Answer = [503, pendingBody];

describe.each([
  ["Express 4", express4],
  ["Express 5", express5],
])("%s", (_name, express) => {
  const app = (fetch: ReturnType<typeof facilitator>, redeemed: ReturnType<typeof createInMemoryRedeemedStore> | null, handler?: express5.RequestHandler) => {
    const a = express();
    a.use(
      cantonPaymentMiddleware({
        routes: { "GET /api/data": { accepts: [requirements()] } },
        facilitatorUrl: "http://fac.test",
        fetch: fetch as unknown as typeof globalThis.fetch,
        redeemed,
      })
    );
    a.get("/api/data", handler ?? ((_req, res) => void res.json({ data: "premium-payload" })));
    return a;
  };
  const paid = (a: ReturnType<typeof app>) => request(a).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature);

  it("a refusal that names the settled updateId is that payment's ticket: served once, then refused", async () => {
    const id = `upd-already-${++seq}`;
    const a = app(facilitator([already(id)]), createInMemoryRedeemedStore());
    const first = await paid(a);
    expect(first.status).toBe(200);
    expect(decodeBase64Json<Record<string, unknown>>(first.headers[HEADER_PAYMENT_RESPONSE_V2.toLowerCase()] as string)).toMatchObject({
      success: true,
      transaction: id,
    });
    const second = await paid(a);
    expect(second.status).toBe(402);
  });

  it("after a failed delivery, the same payment is served again under the scheme's answer", async () => {
    const id = `upd-retry-${++seq}`;
    let calls = 0;
    const a = app(facilitator([ok(id), already(id)]), createInMemoryRedeemedStore(), (_req, res) => {
      calls++;
      if (calls === 1) res.status(502).json({ error: "upstream down" });
      else res.json({ data: "premium-payload" });
    });
    expect((await paid(a)).status).toBe(502);
    expect((await paid(a)).status).toBe(200);
    expect((await paid(a)).status).toBe(402);
  });

  it("without a redeemed store, the refusal stays a refusal", async () => {
    const a = app(facilitator([already(`upd-nostore-${++seq}`)]), null);
    expect((await paid(a)).status).toBe(402);
  });

  it("a pending settle is retried with the same payment until it settles", { timeout: 15_000 }, async () => {
    const id = `upd-pending-${++seq}`;
    const fetch = facilitator([pending, pending, ok(id)]);
    const r = await paid(app(fetch, createInMemoryRedeemedStore()));
    expect(r.status).toBe(200);
    expect(settles(fetch)).toBe(3);
  });

  it("a 200 pending answer is retried the same way", { timeout: 15_000 }, async () => {
    const id = `upd-pending200-${++seq}`;
    const fetch = facilitator([pendingBody, ok(id)]);
    expect((await paid(app(fetch, createInMemoryRedeemedStore()))).status).toBe(200);
    expect(settles(fetch)).toBe(2);
  });

  it("a 503 that is not the pending answer is 502 at once, not retried", async () => {
    const fetch = facilitator([[503, { error: "settle_outcome_unknown" }]]);
    const r = await paid(app(fetch, createInMemoryRedeemedStore()));
    expect(r.status).toBe(502);
    expect(settles(fetch)).toBe(1);
  });

  it("a settle still pending after the retries is 502 do NOT pay again, never 402", { timeout: 15_000 }, async () => {
    const fetch = facilitator([pending]);
    const r = await paid(app(fetch, createInMemoryRedeemedStore()));
    expect(r.status).toBe(502);
    expect(r.body.error).toMatch(/do NOT pay again/);
    expect(settles(fetch)).toBe(4);
  });
});
