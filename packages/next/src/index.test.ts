import { describe, it, expect, vi } from "vitest";
import {
  HEADER_PAYMENT_REQUIRED_V2,
  HEADER_PAYMENT_SIGNATURE_V2,
  HEADER_PAYMENT_RESPONSE_V2,
  encodeBase64Json,
  decodeBase64Json,
  type PaymentRequirements,
} from "@ftptech/x402-canton-core";
import { withCantonPayment } from "./index.js";

const FACILITATOR = "http://fac.test";
const MERCHANT = "merchant::1220m";
const FACILITATOR_PARTY = "ftp_facilitator::1220fff";
const SYNC = "global-domain::1220xyz";
const PAYER = "agent::1220abc";

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
    payload: {
      assetTransferMethod: "transfer-factory",
      submissionRef: "sub-1",
    },
  });
}

function mockFacilitator(
  verifyResult: { isValid: boolean; invalidReason?: string; payer?: string },
  settleResult:
    | { success: boolean; transaction?: string; payer?: string; errorReason?: string }
    | { throw: true }
): typeof globalThis.fetch {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.endsWith("/verify")) {
      return new Response(JSON.stringify(verifyResult), { status: 200 });
    }
    if (url.endsWith("/settle")) {
      if ("throw" in settleResult) throw new Error("settle net error");
      return new Response(JSON.stringify(settleResult), { status: 200 });
    }
    return new Response("not found", { status: 404 });
  }) as typeof globalThis.fetch;
}

function makeHandler(opts: { fetch?: typeof globalThis.fetch } = {}) {
  return withCantonPayment(
    async () => Response.json({ data: "premium-payload" }),
    {
      accepts: [requirements()],
      facilitatorUrl: FACILITATOR,
      description: "Premium data",
      mimeType: "application/json",
      ...(opts.fetch ? { fetch: opts.fetch } : {}),
    }
  );
}

describe("withCantonPayment", () => {
  it("missing PAYMENT-SIGNATURE → 402 with PAYMENT-REQUIRED header", async () => {
    const handler = makeHandler();
    const res = await handler(new Request("https://app.example.com/api/data"));

    expect(res.status).toBe(402);
    const required = decodeBase64Json<{
      x402Version: number;
      accepts: PaymentRequirements[];
      resource: { url: string; description?: string; mimeType?: string };
    }>(res.headers.get(HEADER_PAYMENT_REQUIRED_V2) ?? "");
    expect(required.x402Version).toBe(2);
    expect(required.accepts[0]?.scheme).toBe("exact");
    expect(required.resource.url).toBe("https://app.example.com/api/data");
    expect(required.resource.description).toBe("Premium data");
    expect(required.resource.mimeType).toBe("application/json");
  });

  it("malformed PAYMENT-SIGNATURE → 400", async () => {
    const handler = makeHandler();
    const res = await handler(
      new Request("https://app.example.com/api/data", {
        headers: { [HEADER_PAYMENT_SIGNATURE_V2]: "not-base64-json" },
      })
    );
    expect(res.status).toBe(400);
  });

  it("facilitator unreachable on /verify → 502", async () => {
    const fetch = vi.fn(async () => {
      throw new Error("net down");
    }) as typeof globalThis.fetch;
    const handler = makeHandler({ fetch });
    const res = await handler(
      new Request("https://app.example.com/api/data", {
        headers: { [HEADER_PAYMENT_SIGNATURE_V2]: paymentSignature() },
      })
    );
    expect(res.status).toBe(502);
  });

  it("/verify isValid: false → 402 with invalidReason in PAYMENT-REQUIRED.error", async () => {
    const fetch = mockFacilitator(
      { isValid: false, invalidReason: "invalid_exact_canton_amount_mismatch" },
      { success: false }
    );
    const handler = makeHandler({ fetch });
    const res = await handler(
      new Request("https://app.example.com/api/data", {
        headers: { [HEADER_PAYMENT_SIGNATURE_V2]: paymentSignature() },
      })
    );
    expect(res.status).toBe(402);
    const required = decodeBase64Json<{ error: string }>(
      res.headers.get(HEADER_PAYMENT_REQUIRED_V2) ?? ""
    );
    expect(required.error).toBe("invalid_exact_canton_amount_mismatch");
  });

  it("/verify ok → /settle success → handler runs → response carries PAYMENT-RESPONSE", async () => {
    const fetch = mockFacilitator(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-xyz", payer: PAYER }
    );
    const handler = makeHandler({ fetch });
    const res = await handler(
      new Request("https://app.example.com/api/data", {
        headers: { [HEADER_PAYMENT_SIGNATURE_V2]: paymentSignature() },
      })
    );

    expect(res.status).toBe(200);
    const body = (await res.json()) as { data: string };
    expect(body).toEqual({ data: "premium-payload" });

    const settle = decodeBase64Json<{
      success: boolean;
      transaction: string;
      payer: string;
    }>(res.headers.get(HEADER_PAYMENT_RESPONSE_V2) ?? "");
    expect(settle.success).toBe(true);
    expect(settle.transaction).toBe("u-xyz");
    expect(settle.payer).toBe(PAYER);
  });

  it("/settle returns success: false → 402 with errorReason", async () => {
    const fetch = mockFacilitator(
      { isValid: true, payer: PAYER },
      {
        success: false,
        errorReason: "unexpected_canton_ledger_error",
      }
    );
    const handler = makeHandler({ fetch });
    const res = await handler(
      new Request("https://app.example.com/api/data", {
        headers: { [HEADER_PAYMENT_SIGNATURE_V2]: paymentSignature() },
      })
    );
    expect(res.status).toBe(402);
    const body = (await res.json()) as { error: string };
    expect(body.error).toBe("unexpected_canton_ledger_error");
  });

  it("/settle network error → 502", async () => {
    const fetch = mockFacilitator(
      { isValid: true, payer: PAYER },
      { throw: true }
    );
    const handler = makeHandler({ fetch });
    const res = await handler(
      new Request("https://app.example.com/api/data", {
        headers: { [HEADER_PAYMENT_SIGNATURE_V2]: paymentSignature() },
      })
    );
    expect(res.status).toBe(502);
  });

  it("passes the optional second `context` argument to the wrapped handler", async () => {
    const inner = vi.fn(async () => Response.json({ ok: true }));
    const handler = withCantonPayment(inner, {
      accepts: [requirements()],
      facilitatorUrl: FACILITATOR,
      fetch: mockFacilitator(
        { isValid: true, payer: PAYER },
        { success: true, transaction: "u-1", payer: PAYER }
      ),
    });

    const ctx = { params: { id: "42" } };
    await handler(
      new Request("https://app.example.com/api/data/42", {
        headers: { [HEADER_PAYMENT_SIGNATURE_V2]: paymentSignature() },
      }),
      ctx
    );

    expect(inner).toHaveBeenCalledTimes(1);
    expect(inner.mock.calls[0][1]).toBe(ctx);
  });
});

// ---------------------------------------------------------------------------
// SEC-1 regression: pin requirements to server config; reject client tamper.
// ---------------------------------------------------------------------------
describe("withCantonPayment — SEC-1 requirements pinning", () => {
  function sigWith(accepted: unknown): string {
    return encodeBase64Json({
      x402Version: 2,
      scheme: "exact",
      network: "canton:devnet",
      resource: { url: "https://app.example.com/api/data" },
      accepted,
      payload: {
        assetTransferMethod: "transfer-factory",
        submissionRef: "sub-1",
      },
    });
  }
  function recording(
    verifyResult: Record<string, unknown>,
    settleResult: Record<string, unknown>
  ): { fetch: typeof globalThis.fetch; calls: Array<{ url: string; body: any }> } {
    const calls: Array<{ url: string; body: any }> = [];
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      calls.push({ url, body: init?.body ? JSON.parse(String(init.body)) : null });
      if (url.endsWith("/verify"))
        return new Response(JSON.stringify(verifyResult), { status: 200 });
      if (url.endsWith("/settle"))
        return new Response(JSON.stringify(settleResult), { status: 200 });
      throw new Error(`unexpected fetch: ${url}`);
    }) as unknown as typeof globalThis.fetch;
    return { fetch, calls };
  }
  function run(sig: string, fetchImpl: typeof globalThis.fetch) {
    const inner = vi.fn(async () => Response.json({ data: "premium-payload" }));
    const wrapped = withCantonPayment(inner, {
      accepts: [requirements()],
      facilitatorUrl: FACILITATOR,
      fetch: fetchImpl,
    });
    const headers = new Headers();
    headers.set(HEADER_PAYMENT_SIGNATURE_V2, sig);
    const req = new Request("https://app.example.com/api/data", { headers });
    return { res: wrapped(req) as Promise<Response>, inner };
  }

  it("attacker lowers amount to '1' → 402, handler NOT run, facilitator NOT called", async () => {
    const rec = recording(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "0xabc", payer: PAYER }
    );
    const { res, inner } = run(sigWith({ ...requirements(), amount: "1" }), rec.fetch);
    const r = await res;
    expect(r.status).toBe(402);
    expect(inner).not.toHaveBeenCalled();
    expect(rec.calls).toHaveLength(0);
  });

  it("attacker redirects payTo → 402, facilitator NOT called", async () => {
    const rec = recording({ isValid: true }, { success: true });
    const { res, inner } = run(
      sigWith({ ...requirements(), payTo: "attacker::1220evil" }),
      rec.fetch
    );
    const r = await res;
    expect(r.status).toBe(402);
    expect(inner).not.toHaveBeenCalled();
    expect(rec.calls).toHaveLength(0);
  });

  it("honest payment → 200 AND facilitator receives the SERVER requirements", async () => {
    const rec = recording(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "0xabc", payer: PAYER }
    );
    const { res } = run(sigWith(requirements()), rec.fetch);
    const r = await res;
    expect(r.status).toBe(200);
    const verifyCall = rec.calls.find((c) => c.url.endsWith("/verify"));
    const settleCall = rec.calls.find((c) => c.url.endsWith("/settle"));
    expect(verifyCall?.body.paymentRequirements).toEqual(requirements());
    expect(settleCall?.body.paymentRequirements).toEqual(requirements());
  });

  it("settle body without `success` → 402, handler NOT run (M1)", async () => {
    const rec = recording({ isValid: true, payer: PAYER }, {});
    const { res, inner } = run(sigWith(requirements()), rec.fetch);
    const r = await res;
    expect(r.status).toBe(402);
    expect(inner).not.toHaveBeenCalled();
  });
});

describe("withCantonPayment — config-time asset/instrumentId guard (audit L1)", () => {
  it("throws at setup when an accepts entry's asset disagrees with extra.instrumentId", () => {
    const bad: PaymentRequirements = {
      ...requirements(),
      asset: "dso::Amulet",
      extra: {
        assetTransferMethod: "transfer-factory",
        feePayer: FACILITATOR_PARTY,
        synchronizerId: SYNC,
        instrumentId: { admin: "OTHER", id: "Amulet" },
        executeBeforeSeconds: 120,
      },
    };
    expect(() =>
      withCantonPayment(async () => Response.json({ ok: true }), {
        accepts: [bad],
        facilitatorUrl: FACILITATOR,
      })
    ).toThrow(/disagrees/);
  });
});

describe("one payment, one delivery — across routes, not per route", () => {
  // App Router applies withCantonPayment once per route file. The default
  // store used to be built inside each call, so every route remembered its own
  // redemptions and one settled payment unlocked every equally-priced route.
  const settled = { isValid: true, payer: PAYER };
  const okFetch = () =>
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/verify"))
        return new Response(JSON.stringify(settled), { status: 200 });
      if (url.endsWith("/settle"))
        return new Response(
          JSON.stringify({ success: true, transaction: "1220-one-and-only" }),
          { status: 200 }
        );
      throw new Error(`unexpected fetch: ${url}`);
    }) as unknown as typeof globalThis.fetch;

  const sig = encodeBase64Json({
    x402Version: 2,
    scheme: "exact",
    network: "canton:devnet",
    resource: { url: "https://app.example.com/api/data" },
    accepted: requirements(),
    payload: { assetTransferMethod: "transfer-factory", submissionRef: "sub-1" },
  });
  const route = (fetchImpl: typeof globalThis.fetch) =>
    withCantonPayment(async () => Response.json({ data: "premium-payload" }), {
      accepts: [requirements()],
      facilitatorUrl: FACILITATOR,
      description: "Premium data",
      mimeType: "application/json",
      fetch: fetchImpl,
    });
  const call = (h: ReturnType<typeof route>) =>
    h(
      new Request("https://app.example.com/api/data", {
        headers: { [HEADER_PAYMENT_SIGNATURE_V2]: sig },
      })
    );

  it("the SECOND route refuses the same settled payment", async () => {
    const a = route(okFetch());
    const b = route(okFetch());
    expect((await call(a)).status).toBe(200); // paid for
    expect((await call(b)).status).toBe(402); // used to be 200 — free
  });

  it("an explicitly passed store still wins, and null still disables", async () => {
    // The discriminator. A merchant on several instances passes a SHARED store;
    // the process-wide default must not quietly override it, and `null` must
    // still restore the old unlimited-redemption behaviour on purpose.
    const claims: string[] = [];
    const mine = { claim: (id: string) => (claims.push(id), true) };
    const a = withCantonPayment(async () => Response.json({ ok: true }), {
      accepts: [requirements()],
      facilitatorUrl: FACILITATOR,
      description: "d",
      mimeType: "application/json",
      fetch: okFetch(),
      redeemed: mine,
    });
    expect((await call(a)).status).toBe(200);
    expect(claims).toEqual(["1220-one-and-only"]);

    const open = withCantonPayment(async () => Response.json({ ok: true }), {
      accepts: [requirements()],
      facilitatorUrl: FACILITATOR,
      description: "d",
      mimeType: "application/json",
      fetch: okFetch(),
      redeemed: null,
    });
    expect((await call(open)).status).toBe(200);
    expect((await call(open)).status).toBe(200);
  });
});

/**
 * Twin of the express case. The dedup ticket is the settle updateId; `?? ""`
 * turned a missing one into a key EVERY payment shares, so the first claimed it
 * and every later payment was refused after settling on-ledger. A published
 * wrapper cannot assume a merchant-configured facilitator fills the field.
 */
describe("a settle with no transaction id is not deliverable", () => {
  const handlerWith = (settle: { success: boolean; transaction?: string }) =>
    withCantonPayment(async () => Response.json({ data: "premium-payload" }), {
      accepts: [requirements()],
      facilitatorUrl: FACILITATOR,
      fetch: mockFacilitator({ isValid: true, payer: PAYER }, settle),
      redeemed: (() => {
        const seen = new Set<string>();
        return { claim: async (id: string) => (seen.has(id) ? false : (seen.add(id), true)) };
      })(),
    });

  const call = (h: ReturnType<typeof withCantonPayment>) =>
    h(
      new Request("https://app.example.com/api/data", {
        headers: { "PAYMENT-SIGNATURE": paymentSignature() },
      })
    );

  it("refuses to deliver, and says the payment may have settled", async () => {
    const res = await call(handlerWith({ success: true }));
    expect(res.status).toBe(502);
    expect(JSON.stringify(await res.json())).toMatch(/do NOT pay again/);
  });

  it("treats an empty-string transaction the same way", async () => {
    expect((await call(handlerWith({ success: true, transaction: "" }))).status).toBe(502);
  });

  it("still delivers on a conforming settle", async () => {
    const res = await call(handlerWith({ success: true, transaction: "update-OK" }));
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ data: "premium-payload" });
  });
})
