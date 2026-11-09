/**
 * The facilitator's scheme answers for the Next adapter: a repeat refused with
 * its updateId is the payment's ticket, and `settlement_pending` is retried.
 */
import { describe, it, expect, vi } from "vitest";
import {
  HEADER_PAYMENT_SIGNATURE_V2,
  createInMemoryRedeemedStore,
  encodeBase64Json,
  type PaymentRequirements,
} from "@ftptech/x402-canton-core";
import { withCantonPayment } from "./index.js";

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
const signature = encodeBase64Json({ x402Version: 2, accepted: requirements(), payload: { assetTransferMethod: "transfer-factory" } });
const paid = () => new Request("https://app.example.com/api/data", { headers: { [HEADER_PAYMENT_SIGNATURE_V2]: signature } });

/** A /settle body, or [status, body]; the last one repeats. */
type Answer = Record<string, unknown> | [number, Record<string, unknown>];
function facilitator(answers: Answer[]) {
  let i = 0;
  return vi.fn(async (input: RequestInfo | URL) => {
    if (String(input).endsWith("/verify")) return Response.json({ isValid: true, payer: PAYER });
    const a = answers[Math.min(i++, answers.length - 1)]!;
    return Array.isArray(a) ? Response.json(a[1], { status: a[0] }) : Response.json(a);
  });
}
const PENDING = { success: false, errorReason: "settlement_pending", transaction: "ab".repeat(32), network: "canton:devnet" };
const settles = (f: ReturnType<typeof facilitator>) => f.mock.calls.filter((c) => String(c[0]).endsWith("/settle")).length;
const handler = (fetch: ReturnType<typeof facilitator>) =>
  withCantonPayment(async () => Response.json({ data: "premium-payload" }), {
    accepts: [requirements()],
    facilitatorUrl: "http://fac.test",
    fetch: fetch as never,
    redeemed: createInMemoryRedeemedStore(),
  });

describe("withCantonPayment, settle answers", () => {
  it("a refusal naming the settled updateId is served once, then refused", async () => {
    const h = handler(facilitator([{ success: false, errorReason: "invalid_exact_canton_payment_already_settled", transaction: "upd-next-already", network: "canton:devnet" }]));
    expect((await h(paid())).status).toBe(200);
    expect((await h(paid())).status).toBe(402);
  });

  it.each([
    ["a 503 with the pending body", [503, PENDING] as Answer],
    ["a 200 pending answer", PENDING as Answer],
  ])("%s is retried until it settles", { timeout: 15_000 }, async (_l, pending) => {
    const fetch = facilitator([pending, { success: true, transaction: `upd-next-pending-${String(_l).length}`, payer: PAYER, network: "canton:devnet" }]);
    expect((await handler(fetch)(paid())).status).toBe(200);
    expect(settles(fetch)).toBe(2);
  });

  it("still pending after the retries is 502 do NOT pay again", { timeout: 15_000 }, async () => {
    const fetch = facilitator([[503, PENDING]]);
    const res = await handler(fetch)(paid());
    expect(res.status).toBe(502);
    expect(((await res.json()) as { error: string }).error).toMatch(/do NOT pay again/);
    expect(settles(fetch)).toBe(4);
  });

  it("a 503 that is not the pending answer is 502 at once, not retried", async () => {
    const fetch = facilitator([[503, { error: "settle_outcome_unknown" }]]);
    const res = await handler(fetch)(paid());
    expect(res.status).toBe(502);
    expect(settles(fetch)).toBe(1);
  });
});
