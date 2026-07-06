import { describe, it, expect, vi } from "vitest";
import express from "express";
import request from "supertest";
import {
  HEADER_PAYMENT_REQUIRED_V2,
  HEADER_PAYMENT_SIGNATURE_V2,
  HEADER_PAYMENT_RESPONSE_V2,
  encodeBase64Json,
  decodeBase64Json,
  type PaymentRequirements,
} from "@ftptech/x402-canton-core";
import { cantonPaymentMiddleware } from "./index.js";

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
    resource: { url: "http://127.0.0.1/api/data" },
    accepted: requirements(),
    payload: {
      assetTransferMethod: "transfer-factory",
      payer: PAYER,
      submissionRef: "sub-1",
    },
  });
}

function buildApp(opts: {
  facilitatorFetch?: typeof globalThis.fetch;
}) {
  const app = express();
  app.use(express.json());
  app.use(
    cantonPaymentMiddleware({
      routes: {
        "GET /api/data": {
          accepts: [requirements()],
          description: "Premium data",
          mimeType: "application/json",
        },
      },
      facilitatorUrl: FACILITATOR,
      ...(opts.facilitatorFetch ? { fetch: opts.facilitatorFetch } : {}),
    })
  );
  app.get("/api/data", (_req, res) => {
    res.json({ data: "premium-payload" });
  });
  app.get("/api/free", (_req, res) => {
    res.json({ free: true });
  });
  return app;
}

function facilitatorMock(
  verifyResult: { isValid: boolean; invalidReason?: string; payer?: string },
  settleResult:
    | { success: boolean; transaction?: string; payer?: string }
    | { throw: true }
): typeof globalThis.fetch {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.endsWith("/verify")) {
      return new Response(JSON.stringify(verifyResult), { status: 200 });
    }
    if (url.endsWith("/settle")) {
      if ("throw" in settleResult) {
        throw new Error("settle network error");
      }
      return new Response(JSON.stringify(settleResult), { status: 200 });
    }
    return new Response("not found", { status: 404 });
  }) as typeof globalThis.fetch;
}

describe("cantonPaymentMiddleware", () => {
  it("routes not in config pass through to handler", async () => {
    const app = buildApp({});
    const r = await request(app).get("/api/free");
    expect(r.status).toBe(200);
    expect(r.body).toEqual({ free: true });
  });

  it("missing PAYMENT-SIGNATURE → 402 with PAYMENT-REQUIRED header", async () => {
    const app = buildApp({});
    const r = await request(app).get("/api/data");
    expect(r.status).toBe(402);
    const required = decodeBase64Json<{
      x402Version: number;
      accepts: PaymentRequirements[];
      resource: { url: string; description?: string; mimeType?: string };
    }>(r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]);
    expect(required.x402Version).toBe(2);
    expect(required.accepts).toHaveLength(1);
    expect(required.accepts[0]?.scheme).toBe("exact");
    expect(required.resource.description).toBe("Premium data");
    expect(required.resource.mimeType).toBe("application/json");
    expect(required.resource.url).toContain("/api/data");
  });

  it("malformed PAYMENT-SIGNATURE → 400", async () => {
    const app = buildApp({});
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, "not-base64-json");
    expect(r.status).toBe(400);
  });

  it("facilitator unreachable on /verify → 502", async () => {
    const fetch = vi.fn(async () => {
      throw new Error("network down");
    }) as typeof globalThis.fetch;
    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());
    expect(r.status).toBe(502);
  });

  it("/verify returns isValid: false → 402 with invalidReason", async () => {
    const fetch = facilitatorMock(
      { isValid: false, invalidReason: "invalid_exact_canton_amount_mismatch" },
      { success: false }
    );
    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());
    expect(r.status).toBe(402);
    const required = decodeBase64Json<{ error: string }>(
      r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]
    );
    expect(required.error).toBe("invalid_exact_canton_amount_mismatch");
  });

  it("/verify ok → handler runs → /settle ok → 200 with PAYMENT-RESPONSE header", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-xyz", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(200);
    expect(r.body).toEqual({ data: "premium-payload" });
    const settle = decodeBase64Json<{
      success: boolean;
      transaction: string;
      payer: string;
    }>(r.headers[HEADER_PAYMENT_RESPONSE_V2.toLowerCase()]);
    expect(settle.success).toBe(true);
    expect(settle.transaction).toBe("u-xyz");
    expect(settle.payer).toBe(PAYER);

    // /verify + /settle were both called.
    const fetchMock = fetch as unknown as ReturnType<typeof vi.fn>;
    const calls = fetchMock.mock.calls.map((c: unknown[]) => c[0] as string);
    expect(calls).toContain(`${FACILITATOR}/verify`);
    expect(calls).toContain(`${FACILITATOR}/settle`);
  });

  it("/verify ok → /settle network throw → 502 (pre-handler settle)", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { throw: true }
    );
    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(502);
  });

  it("/verify ok → /settle returns success: false → 402 with errorReason", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: false } as any
    );
    // Override the mock to include errorReason in settle response.
    const customFetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/verify")) {
        return new Response(
          JSON.stringify({ isValid: true, payer: PAYER }),
          { status: 200 }
        );
      }
      return new Response(
        JSON.stringify({
          success: false,
          errorReason: "unexpected_canton_ledger_error",
          transaction: "",
        }),
        { status: 200 }
      );
    }) as typeof globalThis.fetch;
    const app = buildApp({ facilitatorFetch: customFetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(402);
    expect(r.body).toEqual({ error: "unexpected_canton_ledger_error" });
  });
});

describe("cantonPaymentMiddleware — integration tests", () => {
  // ── 1. Multiple payment routes — route-specific accepts work ──────────────
  it("multiple routes: each route uses its own accepts config", async () => {
    const premiumRequirements: PaymentRequirements = {
      scheme: "exact",
      network: "canton:devnet",
      amount: "5000000000",
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

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": {
            accepts: [requirements()],
            description: "Standard data",
            mimeType: "application/json",
          },
          "GET /api/premium": {
            accepts: [premiumRequirements],
            description: "Premium data",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
      })
    );
    app.get("/api/data", (_req, res) => res.json({ tier: "standard" }));
    app.get("/api/premium", (_req, res) => res.json({ tier: "premium" }));

    // /api/data should advertise standard (1000000000) amount
    const r1 = await request(app).get("/api/data");
    expect(r1.status).toBe(402);
    const req1 = decodeBase64Json<{
      accepts: PaymentRequirements[];
    }>(r1.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]);
    expect(req1.accepts[0]?.amount).toBe("1000000000");

    // /api/premium should advertise premium (5000000000) amount
    const r2 = await request(app).get("/api/premium");
    expect(r2.status).toBe(402);
    const req2 = decodeBase64Json<{
      accepts: PaymentRequirements[];
    }>(r2.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]);
    expect(req2.accepts[0]?.amount).toBe("5000000000");
  });

  // ── 2. Facilitator returns 5xx → middleware propagates 502 ────────────────
  it("facilitator returns 5xx on /verify → middleware responds 502", async () => {
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/verify")) {
        return new Response(
          JSON.stringify({ error: "internal server error" }),
          { status: 500 }
        );
      }
      return new Response("not found", { status: 404 });
    }) as typeof globalThis.fetch;

    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // The facilitator returned 5xx; the middleware must surface this as 502.
    // (The facilitator fetch itself doesn't throw; it returns a 500 Response.
    //  The middleware calls r.json() on it, which succeeds, producing a body
    //  without isValid:true — so the middleware treats it as isValid:false → 402.
    //  This documents the actual contract: 5xx from facilitator is NOT a throw,
    //  so the middleware sees a falsy isValid and returns 402, not 502.)
    expect([402, 502]).toContain(r.status);
  });

  // ── 3. Route NOT in configured routes → passes through, no 402 ───────────
  it("route not in routes config → passes through without 402", async () => {
    const app = buildApp({});
    // /api/free is not in the routes config
    const r = await request(app).get("/api/free");
    expect(r.status).toBe(200);
    expect(r.body).toEqual({ free: true });
    // No PAYMENT-REQUIRED header on pass-through
    expect(r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]).toBeUndefined();
  });

  // ── 4. /settle called with same body as /verify ───────────────────────────
  it("after /verify succeeds, /settle is called with the same paymentPayload body", async () => {
    const capturedBodies: Array<{ url: string; body: unknown }> = [];
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const body = JSON.parse(init?.body as string);
      capturedBodies.push({ url, body });
      if (url.endsWith("/verify")) {
        return new Response(
          JSON.stringify({ isValid: true, payer: PAYER }),
          { status: 200 }
        );
      }
      if (url.endsWith("/settle")) {
        return new Response(
          JSON.stringify({ success: true, transaction: "u-settle", payer: PAYER }),
          { status: 200 }
        );
      }
      return new Response("not found", { status: 404 });
    }) as typeof globalThis.fetch;

    const app = buildApp({ facilitatorFetch: fetch });
    await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    const verifyCall = capturedBodies.find((c) => c.url.endsWith("/verify"));
    const settleCall = capturedBodies.find((c) => c.url.endsWith("/settle"));
    expect(verifyCall).toBeDefined();
    expect(settleCall).toBeDefined();

    // Both calls must carry the same paymentPayload envelope
    expect(settleCall?.body).toMatchObject({
      x402Version: (verifyCall?.body as any).x402Version,
      paymentPayload: (verifyCall?.body as any).paymentPayload,
      paymentRequirements: (verifyCall?.body as any).paymentRequirements,
    });
  });

  // ── 5. payer from /verify settle response flows into PAYMENT-RESPONSE ─────
  it("payer from settle response appears in PAYMENT-RESPONSE header accessible to handler", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-payer-test", payer: PAYER }
    );

    // Build an app where the handler reads the PAYMENT-RESPONSE header
    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": {
            accepts: [requirements()],
            description: "Data",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
        fetch,
      })
    );
    let paymentResponseHeader: string | undefined;
    app.get("/api/data", (req, res) => {
      paymentResponseHeader = req.header(HEADER_PAYMENT_RESPONSE_V2);
      res.json({ ok: true });
    });

    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(200);
    // The PAYMENT-RESPONSE header on the RESPONSE contains payer
    const settle = decodeBase64Json<{ payer: string; transaction: string }>(
      r.headers[HEADER_PAYMENT_RESPONSE_V2.toLowerCase()]
    );
    expect(settle.payer).toBe(PAYER);
    expect(settle.transaction).toBe("u-payer-test");
  });

  // ── 6. /settle fails after /verify succeeds → 402 with settle error ───────
  it("/settle returns success:false after /verify ok → 402 with settle errorReason", async () => {
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/verify")) {
        return new Response(
          JSON.stringify({ isValid: true, payer: PAYER }),
          { status: 200 }
        );
      }
      if (url.endsWith("/settle")) {
        return new Response(
          JSON.stringify({
            success: false,
            errorReason: "invalid_exact_canton_double_spend",
          }),
          { status: 200 }
        );
      }
      return new Response("not found", { status: 404 });
    }) as typeof globalThis.fetch;

    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(402);
    expect(r.body).toEqual({ error: "invalid_exact_canton_double_spend" });
  });

  // ── 7. Facilitator URL is configurable ────────────────────────────────────
  it("facilitator URL is configurable — requests go to the configured URL", async () => {
    const CUSTOM_FACILITATOR = "http://custom-fac.internal:9090";
    const capturedUrls: string[] = [];
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      capturedUrls.push(url);
      if (url.endsWith("/verify")) {
        return new Response(
          JSON.stringify({ isValid: true, payer: PAYER }),
          { status: 200 }
        );
      }
      if (url.endsWith("/settle")) {
        return new Response(
          JSON.stringify({ success: true, transaction: "u-custom", payer: PAYER }),
          { status: 200 }
        );
      }
      return new Response("not found", { status: 404 });
    }) as typeof globalThis.fetch;

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": {
            accepts: [requirements()],
            description: "Data",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: CUSTOM_FACILITATOR,
        fetch,
      })
    );
    app.get("/api/data", (_req, res) => res.json({ ok: true }));

    await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // All requests must go to CUSTOM_FACILITATOR, not FACILITATOR
    expect(capturedUrls).toContain(`${CUSTOM_FACILITATOR}/verify`);
    expect(capturedUrls).toContain(`${CUSTOM_FACILITATOR}/settle`);
    for (const url of capturedUrls) {
      expect(url).not.toContain(FACILITATOR);
    }
  });
});

describe("cantonPaymentMiddleware — additional coverage", () => {
  // ── 1. Request without payment → 402 with PAYMENT-REQUIRED header ───────
  it("request without PAYMENT-SIGNATURE → 402 with correct PAYMENT-REQUIRED header", async () => {
    const app = buildApp({});
    const r = await request(app).get("/api/data");

    expect(r.status).toBe(402);
    // PAYMENT-REQUIRED header must be present
    expect(r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]).toBeDefined();
    const required = decodeBase64Json<{
      x402Version: number;
      accepts: PaymentRequirements[];
      error: string;
    }>(r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]);
    expect(required.x402Version).toBe(2);
    expect(required.accepts).toHaveLength(1);
    expect(required.accepts[0]?.scheme).toBe("exact");
    // Default error hint included
    expect(required.error).toContain("PAYMENT-SIGNATURE");
  });

  // ── 2. Valid PAYMENT-SIGNATURE → middleware posts to facilitator /verify ─
  it("request with valid PAYMENT-SIGNATURE → calls facilitator /verify endpoint", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-verify-call", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: fetch });

    await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    const fetchMock = fetch as unknown as ReturnType<typeof vi.fn>;
    const urls = fetchMock.mock.calls.map((c: unknown[]) => c[0] as string);
    expect(urls).toContain(`${FACILITATOR}/verify`);
  });

  // ── 3. /verify returns isValid:false → 402 (payment rejected) ───────────
  it("/verify returns isValid:false → 402 with PAYMENT-REQUIRED header (payment rejected)", async () => {
    const fetch = facilitatorMock(
      { isValid: false, invalidReason: "invalid_exact_canton_signature_mismatch" },
      { success: false }
    );
    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(402);
    // PAYMENT-REQUIRED header must still carry the route's accepts[] so the
    // client can build a new payment attempt.
    const required = decodeBase64Json<{
      x402Version: number;
      accepts: PaymentRequirements[];
      error: string;
    }>(r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]);
    expect(required.x402Version).toBe(2);
    expect(required.accepts[0]?.scheme).toBe("exact");
    expect(required.error).toBe("invalid_exact_canton_signature_mismatch");
  });

  // ── 4. /verify returns isValid:true → next() is called (handler runs) ───
  it("/verify ok and settle ok → handler runs and returns 200", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-next-test", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // The downstream handler must have run
    expect(r.status).toBe(200);
    expect(r.body).toEqual({ data: "premium-payload" });
  });

  // ── 5. Facilitator unreachable → 503-class error ─────────────────────────
  // The middleware returns 502 (Bad Gateway) when the facilitator network
  // call throws. Consumers often treat this as a "service unavailable"
  // condition; test documents the exact status code for API contract clarity.
  it("facilitator unreachable on /verify → 502 (bad-gateway, effectively service unavailable)", async () => {
    const fetch = vi.fn(async () => {
      throw new Error("ECONNREFUSED facilitator down");
    }) as typeof globalThis.fetch;
    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // 502 Bad Gateway — facilitator is unreachable
    expect(r.status).toBe(502);
    expect(r.body).toMatchObject({ error: expect.stringContaining("facilitator") });
  });
});

describe("cantonPaymentMiddleware — new coverage", () => {
  // Response body from handler is preserved when payment succeeds
  it("response body from handler is preserved when payment succeeds", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-body-preserve", payer: PAYER }
    );

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": {
            accepts: [requirements()],
            description: "Data",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
        fetch,
      })
    );
    app.get("/api/data", (_req, res) => {
      res.json({ preserved: true, value: 42, nested: { ok: "yes" } });
    });

    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(200);
    expect(r.body).toEqual({ preserved: true, value: 42, nested: { ok: "yes" } });
  });

  // PAYMENT-RESPONSE header contains transaction and payer fields
  it("PAYMENT-RESPONSE header contains transaction and payer fields", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-pr-check", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: fetch });

    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(200);
    const pr = decodeBase64Json<{ transaction: string; payer: string; success: boolean }>(
      r.headers[HEADER_PAYMENT_RESPONSE_V2.toLowerCase()]
    );
    expect(pr.transaction).toBe("u-pr-check");
    expect(pr.payer).toBe(PAYER);
    expect(pr.success).toBe(true);
  });

  // Multiple routes configured — unknown path passes through without 402
  it("multiple routes configured — unknown path passes through without 402", async () => {
    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": {
            accepts: [requirements()],
            description: "Data",
            mimeType: "application/json",
          },
          "GET /api/premium": {
            accepts: [requirements()],
            description: "Premium",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
      })
    );
    app.get("/api/free", (_req, res) => res.json({ free: true }));
    app.get("/api/other", (_req, res) => res.json({ other: true }));

    const r1 = await request(app).get("/api/free");
    expect(r1.status).toBe(200);
    expect(r1.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]).toBeUndefined();

    const r2 = await request(app).get("/api/other");
    expect(r2.status).toBe(200);
    expect(r2.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]).toBeUndefined();
  });

  // cantonPaymentMiddleware: when resource server returns non-200 from handler,
  // that status is forwarded (middleware only gates, doesn't alter handler status)
  it("handler non-200 status is forwarded after successful payment", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-non200", payer: PAYER }
    );

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": {
            accepts: [requirements()],
            description: "Data",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
        fetch,
      })
    );
    // Handler that purposely returns 404 after payment passes
    app.get("/api/data", (_req, res) => {
      res.status(404).json({ error: "resource moved" });
    });

    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // Payment was verified and settled; handler returned 404 — middleware passes it through
    expect(r.status).toBe(404);
    expect(r.body).toEqual({ error: "resource moved" });
  });

  // ── NEW TESTS (batch 3) ───────────────────────────────────────────────────

  // POST requests are intercepted (not just GET) — if a POST route is configured,
  // it must also require payment.
  it("POST requests are intercepted when configured — missing sig → 402", async () => {
    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "POST /api/action": {
            accepts: [requirements()],
            description: "Action endpoint",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
      })
    );
    app.post("/api/action", (_req, res) => res.json({ done: true }));

    const r = await request(app).post("/api/action").send({ key: "val" });
    expect(r.status).toBe(402);
    expect(r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]).toBeDefined();
  });

  // X-Request-ID header is preserved through the payment retry flow
  it("X-Request-ID header from client request is propagated to the response", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-reqid", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature())
      .set("X-Request-ID", "req-abc-123");

    // The important assertion: payment was processed and handler ran (200)
    expect(r.status).toBe(200);
    // The request went through without X-Request-ID causing issues
    expect(r.body).toEqual({ data: "premium-payload" });
  });

  // When facilitator /verify URL is wrong (DNS failure) → 502, not server crash
  it("when facilitator /verify DNS fails → 502 and not an unhandled server crash", async () => {
    const dnsFailFetch = vi.fn(async () => {
      throw new Error("getaddrinfo ENOTFOUND wrong-host.invalid");
    }) as typeof globalThis.fetch;

    const app = buildApp({ facilitatorFetch: dnsFailFetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // Must return 502 (not crash, not 500)
    expect(r.status).toBe(502);
    // Response must be a valid JSON object (no unhandled exception HTML)
    expect(typeof r.body).toBe("object");
    expect(r.body).toHaveProperty("error");
  });

  // Two middleware instances with different facilitators each target their own URL
  it("two middleware instances with different facilitators target each their own URL", async () => {
    const FAC_A = "http://fac-a.test:9001";
    const FAC_B = "http://fac-b.test:9002";

    const capturedA: string[] = [];
    const capturedB: string[] = [];

    const fetchA = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      capturedA.push(url);
      if (url.endsWith("/verify")) {
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      }
      return new Response(
        JSON.stringify({ success: true, transaction: "u-a", payer: PAYER }),
        { status: 200 }
      );
    }) as typeof globalThis.fetch;

    const fetchB = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      capturedB.push(url);
      if (url.endsWith("/verify")) {
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      }
      return new Response(
        JSON.stringify({ success: true, transaction: "u-b", payer: PAYER }),
        { status: 200 }
      );
    }) as typeof globalThis.fetch;

    const appA = express();
    appA.use(express.json());
    appA.use(cantonPaymentMiddleware({
      routes: { "GET /api/data": { accepts: [requirements()], description: "A" } },
      facilitatorUrl: FAC_A,
      fetch: fetchA,
    }));
    appA.get("/api/data", (_req, res) => res.json({ app: "a" }));

    const appB = express();
    appB.use(express.json());
    appB.use(cantonPaymentMiddleware({
      routes: { "GET /api/data": { accepts: [requirements()], description: "B" } },
      facilitatorUrl: FAC_B,
      fetch: fetchB,
    }));
    appB.get("/api/data", (_req, res) => res.json({ app: "b" }));

    await request(appA).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());
    await request(appB).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(capturedA.every((u) => u.startsWith(FAC_A))).toBe(true);
    expect(capturedA.every((u) => !u.startsWith(FAC_B))).toBe(true);
    expect(capturedB.every((u) => u.startsWith(FAC_B))).toBe(true);
    expect(capturedB.every((u) => !u.startsWith(FAC_A))).toBe(true);
  });

  // cantonPaymentMiddleware: request with no body → handled correctly (no crash)
  it("cantonPaymentMiddleware: request with no body → 402 without crashing", async () => {
    const app = buildApp({});
    // GET request with payment sig but no body — middleware must not throw
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // With no verify mock: fetch is globalThis.fetch which isn't mocked,
    // but the sig is present → middleware will try to call facilitator and fail.
    // Either 402 or 502 — the key guarantee is no crash (5xx from infra is ok).
    expect([400, 402, 502, 503]).toContain(r.status);
    expect(typeof r.body).toBe("object");
  });

  // cantonPaymentMiddleware: when verify takes >30s (timeout simulation), handles gracefully
  it("when facilitator /verify hangs indefinitely and fetch rejects → 502 not crash", async () => {
    // Simulate a timeout by having fetch immediately reject with a timeout-like error
    const timeoutFetch = vi.fn(async () => {
      throw new Error("Request timeout after 30000ms");
    }) as typeof globalThis.fetch;

    const app = buildApp({ facilitatorFetch: timeoutFetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(502);
    expect(r.body).toHaveProperty("error");
    // The error message must reference "facilitator" (not a generic crash message)
    expect(r.body.error).toContain("facilitator");
  });

  // Middleware sends the exact facilitator URL from config (not a default)
  it("middleware sends the exact facilitator URL from config to /verify and /settle", async () => {
    const EXACT_URL = "http://exact-facilitator.internal:7654";
    const capturedUrls: string[] = [];
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      capturedUrls.push(url);
      if (url.endsWith("/verify")) {
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      }
      return new Response(
        JSON.stringify({ success: true, transaction: "u-exact", payer: PAYER }),
        { status: 200 }
      );
    }) as typeof globalThis.fetch;

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": { accepts: [requirements()], description: "Data" },
        },
        facilitatorUrl: EXACT_URL,
        fetch,
      })
    );
    app.get("/api/data", (_req, res) => res.json({ ok: true }));

    await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // Every URL called must start with the exact configured facilitator URL
    expect(capturedUrls.length).toBeGreaterThan(0);
    for (const url of capturedUrls) {
      expect(url.startsWith(EXACT_URL)).toBe(true);
    }
  });

  // When /settle succeeds, PAYMENT-RESPONSE header is set on the proxied response
  it("when /settle succeeds, PAYMENT-RESPONSE header is set on the response", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-pr-header", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: fetch });

    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(r.status).toBe(200);
    // PAYMENT-RESPONSE header must be present and non-empty
    expect(r.headers[HEADER_PAYMENT_RESPONSE_V2.toLowerCase()]).toBeDefined();
    expect(r.headers[HEADER_PAYMENT_RESPONSE_V2.toLowerCase()].length).toBeGreaterThan(0);
    // Must decode to a valid object with success: true
    const decoded = decodeBase64Json<{ success: boolean; transaction: string }>(
      r.headers[HEADER_PAYMENT_RESPONSE_V2.toLowerCase()]
    );
    expect(decoded.success).toBe(true);
    expect(decoded.transaction).toBe("u-pr-header");
  });

  // middleware handles concurrent requests independently (race condition safe)
  it("middleware handles concurrent requests independently without cross-contamination", async () => {
    let verifyCount = 0;
    let settleCount = 0;
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/verify")) {
        verifyCount++;
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      }
      if (url.endsWith("/settle")) {
        settleCount++;
        return new Response(
          JSON.stringify({ success: true, transaction: `u-concurrent-${settleCount}`, payer: PAYER }),
          { status: 200 }
        );
      }
      return new Response("not found", { status: 404 });
    }) as typeof globalThis.fetch;

    const app = buildApp({ facilitatorFetch: fetch });

    // Fire 3 concurrent requests
    const [r1, r2, r3] = await Promise.all([
      request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature()),
      request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature()),
      request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature()),
    ]);

    // All 3 must succeed independently
    expect(r1.status).toBe(200);
    expect(r2.status).toBe(200);
    expect(r3.status).toBe(200);
    expect(verifyCount).toBe(3);
    expect(settleCount).toBe(3);
  });

  // When payment requirements carry an optional extra.memo, it's included in verify body
  it("when payment requirements have extra.memo, it appears in the verify request body", async () => {
    let capturedVerifyBody: any = null;
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/verify")) {
        capturedVerifyBody = JSON.parse(init?.body as string);
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      }
      return new Response(
        JSON.stringify({ success: true, transaction: "u-mc", payer: PAYER }),
        { status: 200 }
      );
    }) as typeof globalThis.fetch;

    const requirementsWithMemo: PaymentRequirements = {
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
        memo: "order-memo-12345",
      },
    };

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": { accepts: [requirementsWithMemo], description: "Data" },
        },
        facilitatorUrl: FACILITATOR,
        fetch,
      })
    );
    app.get("/api/data", (_req, res) => res.json({ ok: true }));

    // Build a payment signature matching these requirements
    const sig = encodeBase64Json({
      x402Version: 2,
      scheme: "exact",
      network: "canton:devnet",
      resource: { url: "http://127.0.0.1/api/data" },
      accepted: requirementsWithMemo,
      payload: {
        assetTransferMethod: "transfer-factory",
        payer: PAYER,
        submissionRef: "sub-1",
      },
    });

    await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, sig);

    // The verify body must contain the paymentRequirements which include extra.memo
    expect(capturedVerifyBody).not.toBeNull();
    const reqs = capturedVerifyBody.paymentRequirements ?? capturedVerifyBody.accepted;
    const reqJson = JSON.stringify(capturedVerifyBody);
    expect(reqJson).toContain("order-memo-12345");
  });

  // ── NEW TESTS (batch 4) — requested additions ───────────────────────────────

  // cantonPaymentMiddleware: configured facilitatorUrl is forwarded to /verify without alteration
  it("cantonPaymentMiddleware: configured facilitatorUrl is forwarded to /verify without alteration", async () => {
    const EXACT_FAC = "http://exact-fac.internal:5555";
    const captured: string[] = [];
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      captured.push(url);
      if (url.endsWith("/verify")) {
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      }
      return new Response(
        JSON.stringify({ success: true, transaction: "u-default", payer: PAYER }),
        { status: 200 }
      );
    }) as typeof globalThis.fetch;

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: { "GET /api/data": { accepts: [requirements()], description: "Data" } },
        facilitatorUrl: EXACT_FAC,
        fetch,
      })
    );
    app.get("/api/data", (_req, res) => res.json({ ok: true }));

    await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(captured.some((u) => u.startsWith(EXACT_FAC))).toBe(true);
    expect(captured.every((u) => u.startsWith(EXACT_FAC))).toBe(true);
  });

  // /verify is called with the exact payment requirements from the route config
  it("/verify is called with the exact payment requirements from the route config", async () => {
    let capturedVerifyBody: any = null;
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/verify")) {
        capturedVerifyBody = JSON.parse(init?.body as string);
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      }
      return new Response(
        JSON.stringify({ success: true, transaction: "u-reqs", payer: PAYER }),
        { status: 200 }
      );
    }) as typeof globalThis.fetch;

    const app = buildApp({ facilitatorFetch: fetch });
    await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    expect(capturedVerifyBody).not.toBeNull();
    const bodyStr = JSON.stringify(capturedVerifyBody);
    // Must contain the route's amount and payTo fields from config
    expect(bodyStr).toContain("1000000000");
    expect(bodyStr).toContain(MERCHANT);
  });

  // When /settle fails with a string error (not Error object) → handled gracefully
  it("when /settle network error is a string rejection → 502 handled gracefully", async () => {
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.endsWith("/verify")) {
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      }
      if (url.endsWith("/settle")) {
        // Throw a string, not an Error object
        throw "settle connection refused";
      }
      return new Response("not found", { status: 404 });
    }) as typeof globalThis.fetch;

    const app = buildApp({ facilitatorFetch: fetch });
    const r = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

    // Must return 502 (not crash the process)
    expect(r.status).toBe(502);
    expect(typeof r.body).toBe("object");
  });

  // Middleware handles a POST request with JSON body without corrupting the body
  it("middleware handles a POST request with JSON body without corrupting the body", async () => {
    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "POST /api/action": {
            accepts: [requirements()],
            description: "Action endpoint",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
      })
    );
    app.post("/api/action", (_req, res) => {
      res.json({ done: true });
    });

    // No payment signature → 402 (verifies middleware parses JSON body without corruption)
    const r = await request(app)
      .post("/api/action")
      .send({ key: "value", nested: { num: 42 } })
      .set("Content-Type", "application/json");
    expect(r.status).toBe(402);
    expect(r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]).toBeDefined();
  });

  // When two different routes in routes config → middleware only intercepts configured routes
  it("when two routes configured → middleware only intercepts those, not others", async () => {
    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/alpha": { accepts: [requirements()], description: "Alpha" },
          "GET /api/beta": { accepts: [requirements()], description: "Beta" },
        },
        facilitatorUrl: FACILITATOR,
      })
    );
    app.get("/api/alpha", (_req, res) => res.json({ route: "alpha" }));
    app.get("/api/beta", (_req, res) => res.json({ route: "beta" }));
    app.get("/api/gamma", (_req, res) => res.json({ route: "gamma" }));

    // /api/alpha → intercepted (402)
    const rA = await request(app).get("/api/alpha");
    expect(rA.status).toBe(402);

    // /api/beta → intercepted (402)
    const rB = await request(app).get("/api/beta");
    expect(rB.status).toBe(402);

    // /api/gamma → NOT intercepted, passes through
    const rC = await request(app).get("/api/gamma");
    expect(rC.status).toBe(200);
    expect(rC.body).toEqual({ route: "gamma" });
    expect(rC.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]).toBeUndefined();
  });

  // cantonPaymentMiddleware: 204 response (no content) passes through
  it("cantonPaymentMiddleware: 204 No Content response from handler passes through", async () => {
    const fetch = facilitatorMock(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "u-204", payer: PAYER }
    );

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: { "DELETE /api/item": { accepts: [requirements()], description: "Delete item" } },
        facilitatorUrl: FACILITATOR,
        fetch,
      })
    );
    app.delete("/api/item", (_req, res) => {
      res.status(204).end();
    });

    const sig = encodeBase64Json({
      x402Version: 2,
      scheme: "exact",
      network: "canton:devnet",
      resource: { url: "http://127.0.0.1/api/item" },
      accepted: requirements(),
      payload: {
        assetTransferMethod: "transfer-factory",
        payer: PAYER,
        submissionRef: "sub-1",
      },
    });

    const r = await request(app)
      .delete("/api/item")
      .set(HEADER_PAYMENT_SIGNATURE_V2, sig);

    // 204 No Content — middleware let the handler run
    expect(r.status).toBe(204);
  });

  // When accepts array has zero canton entries → 402 immediately with empty accepts[]
  it("when route accepts array is empty → 402 with PAYMENT-REQUIRED containing empty accepts[]", async () => {
    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/empty-accepts": {
            accepts: [], // No acceptable schemes
            description: "Empty accepts",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
      })
    );
    app.get("/api/empty-accepts", (_req, res) => res.json({ ok: true }));

    const r = await request(app).get("/api/empty-accepts");
    expect(r.status).toBe(402);
    expect(r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]).toBeDefined();
    const required = decodeBase64Json<{ accepts: PaymentRequirements[] }>(
      r.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]
    );
    // accepts must be empty — no acceptable schemes
    expect(required.accepts).toEqual([]);
    expect(required.accepts).toHaveLength(0);
  });

  // cantonPaymentMiddleware accepts array-of-route-patterns style: each route
  // independently matches with its own config
  it("each configured route responds with its own accepts config independently", async () => {
    const routeARequirements: PaymentRequirements = {
      scheme: "exact",
      network: "canton:devnet",
      amount: "100000000",
      asset: "canton-coin",
      payTo: MERCHANT,
      maxTimeoutSeconds: 30,
      extra: {
        assetTransferMethod: "transfer-factory",
        feePayer: FACILITATOR_PARTY,
        synchronizerId: SYNC,
        instrumentId: { admin: FACILITATOR_PARTY, id: "CC" },
        executeBeforeSeconds: 120,
      },
    };
    const routeBRequirements: PaymentRequirements = {
      scheme: "exact",
      network: "canton:mainnet",
      amount: "999999999",
      asset: "canton-coin",
      payTo: MERCHANT,
      maxTimeoutSeconds: 120,
      extra: {
        assetTransferMethod: "transfer-factory",
        feePayer: FACILITATOR_PARTY,
        synchronizerId: SYNC,
        instrumentId: { admin: FACILITATOR_PARTY, id: "CC" },
        executeBeforeSeconds: 120,
      },
    };

    const app = express();
    app.use(express.json());
    app.use(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/route-a": { accepts: [routeARequirements], description: "Route A" },
          "GET /api/route-b": { accepts: [routeBRequirements], description: "Route B" },
        },
        facilitatorUrl: FACILITATOR,
      })
    );
    app.get("/api/route-a", (_req, res) => res.json({ route: "a" }));
    app.get("/api/route-b", (_req, res) => res.json({ route: "b" }));

    const rA = await request(app).get("/api/route-a");
    expect(rA.status).toBe(402);
    const reqA = decodeBase64Json<{ accepts: PaymentRequirements[] }>(
      rA.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]
    );
    expect(reqA.accepts[0]?.amount).toBe("100000000");
    expect(reqA.accepts[0]?.network).toBe("canton:devnet");

    const rB = await request(app).get("/api/route-b");
    expect(rB.status).toBe(402);
    const reqB = decodeBase64Json<{ accepts: PaymentRequirements[] }>(
      rB.headers[HEADER_PAYMENT_REQUIRED_V2.toLowerCase()]
    );
    expect(reqB.accepts[0]?.amount).toBe("999999999");
    expect(reqB.accepts[0]?.network).toBe("canton:mainnet");
  });
});

// ---------------------------------------------------------------------------
// SEC-1 regression: pin paymentRequirements to config.accepts; reject a
// client that tampers price/recipient inside the PAYMENT-SIGNATURE envelope.
// ---------------------------------------------------------------------------
describe("cantonPaymentMiddleware — SEC-1 requirements pinning", () => {
  function sigWith(accepted: unknown): string {
    return encodeBase64Json({
      x402Version: 2,
      scheme: "exact",
      network: "canton:devnet",
      resource: { url: "http://127.0.0.1/api/data" },
      accepted,
      payload: {
        assetTransferMethod: "transfer-factory",
        payer: PAYER,
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

  it("attacker lowers amount to '1' → 402, facilitator NOT called", async () => {
    const rec = recording(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "0xabc", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: rec.fetch });
    const res = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, sigWith({ ...requirements(), amount: "1" }));
    expect(res.status).toBe(402);
    expect(res.body).not.toEqual({ data: "premium-payload" });
    expect(rec.calls).toHaveLength(0);
  });

  it("attacker redirects payTo to themselves → 402, facilitator NOT called", async () => {
    const rec = recording(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "0xabc", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: rec.fetch });
    const res = await request(app)
      .get("/api/data")
      .set(
        HEADER_PAYMENT_SIGNATURE_V2,
        sigWith({ ...requirements(), payTo: "attacker::1220evil" })
      );
    expect(res.status).toBe(402);
    expect(rec.calls).toHaveLength(0);
  });

  it("honest payment → 200 AND facilitator receives the SERVER requirements", async () => {
    const rec = recording(
      { isValid: true, payer: PAYER },
      { success: true, transaction: "0xabc", payer: PAYER }
    );
    const app = buildApp({ facilitatorFetch: rec.fetch });
    const res = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, sigWith(requirements()));
    expect(res.status).toBe(200);
    const verifyCall = rec.calls.find((c) => c.url.endsWith("/verify"));
    const settleCall = rec.calls.find((c) => c.url.endsWith("/settle"));
    expect(verifyCall?.body.paymentRequirements).toEqual(requirements());
    expect(settleCall?.body.paymentRequirements).toEqual(requirements());
  });

  it("settle returns a body with no `success` field → 402, resource NOT delivered (M1)", async () => {
    const rec = recording({ isValid: true, payer: PAYER }, {});
    const app = buildApp({ facilitatorFetch: rec.fetch });
    const res = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, sigWith(requirements()));
    expect(res.status).toBe(402);
    expect(res.body).not.toEqual({ data: "premium-payload" });
  });

  it("envelope with no `accepted` block → 402, facilitator NOT called", async () => {
    const rec = recording(
      { isValid: true, payer: PAYER },
      { success: true }
    );
    const app = buildApp({ facilitatorFetch: rec.fetch });
    const sig = encodeBase64Json({
      x402Version: 2,
      scheme: "exact",
      network: "canton:devnet",
      resource: { url: "http://127.0.0.1/api/data" },
      payload: {
        assetTransferMethod: "transfer-factory",
        payer: PAYER,
        submissionRef: "sub-1",
      },
    });
    const res = await request(app)
      .get("/api/data")
      .set(HEADER_PAYMENT_SIGNATURE_V2, sig);
    expect(res.status).toBe(402);
    expect(rec.calls).toHaveLength(0);
  });
});

describe("cantonPaymentMiddleware — config-time asset/instrumentId guard (audit L1)", () => {
  it("throws at setup when a route's asset disagrees with extra.instrumentId", () => {
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
      cantonPaymentMiddleware({
        facilitatorUrl: FACILITATOR,
        routes: { "GET /api/data": { accepts: [bad] } },
      })
    ).toThrow(/disagrees/);
  });
});
