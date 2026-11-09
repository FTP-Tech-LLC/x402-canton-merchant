/**
 * A handler's 5xx, shared caches and a payer who leaves, for the Next adapter.
 */
import { describe, it, expect, vi } from "vitest";
import {
  HEADER_PAYMENT_SIGNATURE_V2,
  createInMemoryRedeemedStore,
  encodeBase64Json,
  type PaymentRequirements,
} from "@ftptech/x402-canton-core";
import { withCantonPayment } from "./index.js";

const URL_ = "https://app.example.com/api/data";
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

function facilitator(id: string, onSettle: () => void = () => {}) {
  return vi.fn(async (input: RequestInfo | URL) => {
    if (String(input).endsWith("/verify")) return Response.json({ isValid: true, payer: PAYER });
    onSettle();
    return Response.json({ success: true, transaction: id, payer: PAYER, network: "canton:devnet" });
  });
}

const paid = (init: RequestInit = {}) =>
  new Request(URL_, { ...init, headers: { [HEADER_PAYMENT_SIGNATURE_V2]: signature } });

describe("withCantonPayment, money-safety", () => {
  it("a 5xx the handler returns gives the payment back, so the same payment is served on retry", async () => {
    let calls = 0;
    const fetch = facilitator(`upd-next-5xx-${++seq}`);
    const handler = withCantonPayment(
      async () => {
        calls++;
        return calls === 1 ? Response.json({ error: "upstream down" }, { status: 502 }) : Response.json({ data: "premium-payload" });
      },
      { accepts: [requirements()], facilitatorUrl: "http://fac.test", fetch: fetch as never, redeemed: createInMemoryRedeemedStore() }
    );
    expect((await handler(paid())).status).toBe(502);
    expect((await handler(paid())).status).toBe(200);
    expect(calls).toBe(2);
  });

  it("the 402 is no-store and the paid response is private, merged with the handler's own", async () => {
    const fetch = facilitator(`upd-next-cache-${++seq}`);
    const handler = withCantonPayment(
      async () => new Response("premium-payload", { headers: { "Cache-Control": "public, max-age=60" } }),
      { accepts: [requirements()], facilitatorUrl: "http://fac.test", fetch: fetch as never, redeemed: createInMemoryRedeemedStore() }
    );
    const required = await handler(new Request(URL_));
    expect(required.status).toBe(402);
    expect(required.headers.get("cache-control")).toBe("no-store");
    const res = await handler(paid());
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe("public, max-age=60, private");
  });

  it("a payer who left before /settle is not charged, and one who left during it is served on retry", async () => {
    const before = new AbortController();
    before.abort();
    const fetch1 = facilitator(`upd-next-gone1-${++seq}`);
    let ran = 0;
    const handler1 = withCantonPayment(
      async () => {
        ran++;
        return Response.json({ data: "premium-payload" });
      },
      { accepts: [requirements()], facilitatorUrl: "http://fac.test", fetch: fetch1 as never, redeemed: createInMemoryRedeemedStore() }
    );
    await handler1(paid({ signal: before.signal }));
    expect(fetch1.mock.calls.map((c) => String(c[0]))).toEqual(["http://fac.test/verify"]);
    expect(ran).toBe(0);

    const during = new AbortController();
    const fetch2 = facilitator(`upd-next-gone2-${++seq}`, () => during.abort());
    const handler2 = withCantonPayment(
      async () => {
        ran++;
        return Response.json({ data: "premium-payload" });
      },
      { accepts: [requirements()], facilitatorUrl: "http://fac.test", fetch: fetch2 as never, redeemed: createInMemoryRedeemedStore() }
    );
    await handler2(paid({ signal: during.signal }));
    expect(ran).toBe(0);
    expect((await handler2(paid())).status).toBe(200);
    expect(ran).toBe(1);
  });
});
