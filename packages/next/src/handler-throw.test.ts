/**
 * A HANDLER CRASH MUST NOT BURN THE PAYMENT.
 *
 * This wrapper settles before the handler runs and claims the settle updateId
 * as a one-delivery ticket immediately after. The claim is right — it is what
 * stops one payment buying two responses. What is wrong is what happens when
 * the handler then THROWS: the ticket stays claimed, so the payer, who has
 * already paid on-ledger and received a 500, is refused
 * `payment_already_redeemed` on every retry of that same payment. Paid, not
 * served, and permanently unable to be served.
 *
 * The Express sibling states this trade-off honestly ("the payment landed
 * on-ledger but the resource wasn't delivered"); the Next copy of the same
 * sentence claimed the "losses are on the merchant", which is backwards — the
 * merchant has the money. Two copies of one rule, and the one that got the
 * accounting wrong is the one that could actually fix it.
 *
 * The fix is only safe HERE, not in Express. This wrapper AWAITS a `Response`
 * object: a throw means the handler produced nothing and the client has been
 * sent nothing, so releasing the ticket cannot double-deliver. Express hands
 * control to the route via `next()` and never awaits it — a route there can
 * throw after it has already written bytes to `res`.
 */
import { describe, it, expect, vi } from "vitest";
import {
  HEADER_PAYMENT_SIGNATURE_V2,
  encodeBase64Json,
  createInMemoryRedeemedStore,
  type PaymentRequirements,
} from "@ftptech/x402-canton-core";
import { withCantonPayment } from "./index.js";

const FACILITATOR = "http://fac.test";
const MERCHANT = "merchant::1220m";
const FACILITATOR_PARTY = "ftp_facilitator::1220fff";
const SYNC = "global-domain::1220xyz";
const PAYER = "agent::1220abc";
/** The settle updateId. The facilitator answers a replay of the SAME signed
 *  bytes with the SAME updateId, which is exactly why one id is one delivery. */
const TICKET = "1220update-abc";

function requirements(): PaymentRequirements {
  return {
    scheme: "exact",
    network: "canton:devnet",
    amount: "1000000000",
    asset: "canton-coin",
    payTo: MERCHANT,
    maxTimeoutSeconds: 60,
    extra: {
      assetTransferMethod: "transfer-factory",
      feePayer: FACILITATOR_PARTY,
      synchronizerId: SYNC,
      instrumentId: { admin: FACILITATOR_PARTY, id: "CC" },
      executeBeforeSeconds: 120,
    },
  };
}

function paymentSignature(): string {
  return encodeBase64Json({
    x402Version: 2,
    scheme: "exact",
    network: "canton:devnet",
    resource: { url: "https://app.example.com/api/data" },
    accepted: requirements(),
    payload: { assetTransferMethod: "transfer-factory", submissionRef: "sub-1" },
  });
}

/** Counts settles so "the payer paid" is measured, not assumed. Always answers
 *  the same updateId, which is what the real facilitator does for a replay. */
function countingFacilitator() {
  const calls = { verify: 0, settle: 0 };
  const fetchFn = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.endsWith("/verify")) {
      calls.verify++;
      return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
    }
    if (url.endsWith("/settle")) {
      calls.settle++;
      return new Response(
        JSON.stringify({ success: true, transaction: TICKET, payer: PAYER }),
        { status: 200 }
      );
    }
    return new Response("not found", { status: 404 });
  }) as unknown as typeof globalThis.fetch;
  return { calls, fetchFn };
}

function paidRequest(): Request {
  return new Request("https://app.example.com/api/data", {
    headers: { [HEADER_PAYMENT_SIGNATURE_V2]: paymentSignature() },
  });
}

describe("a handler that throws must leave the payment redeemable", () => {
  it("releases the ticket, so the SAME payment can be retried and served", async () => {
    const { calls, fetchFn } = countingFacilitator();
    // A store shared across both attempts, as a real single-process merchant has.
    const redeemed = createInMemoryRedeemedStore();
    let attempts = 0;
    const handler = withCantonPayment(
      async () => {
        attempts++;
        if (attempts === 1) throw new Error("database down");
        return Response.json({ data: "premium-payload" });
      },
      {
        accepts: [requirements()],
        facilitatorUrl: FACILITATOR,
        description: "Premium data",
        mimeType: "application/json",
        fetch: fetchFn,
        redeemed,
      }
    );

    // Attempt 1: settles, handler crashes. The crash must still propagate —
    // swallowing it would turn a merchant bug into a silent 200.
    await expect(handler(paidRequest())).rejects.toThrow("database down");
    expect(calls.settle).toBe(1);

    // Attempt 2: the SAME payment, the same updateId. The payer must be served.
    const res = await handler(paidRequest());
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ data: "premium-payload" });
    expect(attempts).toBe(2);
  });

  it("still refuses a replay when the handler SUCCEEDED (release is throw-only)", async () => {
    // The release must not become a general amnesty: one payment, one delivery
    // is the whole point of the ticket. This is the discriminator — a fix that
    // released unconditionally would pass the test above and break this one.
    const { fetchFn } = countingFacilitator();
    const redeemed = createInMemoryRedeemedStore();
    let served = 0;
    const handler = withCantonPayment(
      async () => {
        served++;
        return Response.json({ data: "premium-payload" });
      },
      {
        accepts: [requirements()],
        facilitatorUrl: FACILITATOR,
        fetch: fetchFn,
        redeemed,
      }
    );

    expect((await handler(paidRequest())).status).toBe(200);
    expect((await handler(paidRequest())).status).toBe(402);
    expect(served).toBe(1);
  });

  it("a store without `release` is not broken by the crash path", async () => {
    // `release` is optional on RedeemedStore: merchants ship their own (Redis, a
    // table) against the published interface, and adding a REQUIRED method would
    // break every one of them at the type level. A store that cannot release
    // keeps today's behaviour — the crash propagates, nothing throws inside the
    // wrapper's own error handling.
    const { fetchFn } = countingFacilitator();
    const claimed = new Set<string>();
    const handler = withCantonPayment(
      async () => {
        throw new Error("database down");
      },
      {
        accepts: [requirements()],
        facilitatorUrl: FACILITATOR,
        fetch: fetchFn,
        redeemed: {
          claim(id: string) {
            if (claimed.has(id)) return false;
            claimed.add(id);
            return true;
          },
        },
      }
    );

    await expect(handler(paidRequest())).rejects.toThrow("database down");
    expect(claimed.has(TICKET)).toBe(true);
  });
});
