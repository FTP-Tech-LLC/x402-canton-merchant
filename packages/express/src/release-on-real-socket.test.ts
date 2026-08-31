import { describe, it, expect } from "vitest";
import express from "express";
import http from "node:http";
import { createInMemoryRedeemedStore, encodeBase64Json } from "@ftptech/x402-canton-core";
import { cantonPaymentMiddleware } from "./index.js";

/**
 * The release listener, on a REAL http server over a REAL socket with
 * keep-alive.
 *
 * The unit tests next to this file drive the middleware through supertest,
 * which opens a fresh socket per request and tears it down immediately. That
 * hides the only thing genuinely uncertain about hanging money logic off
 * `res.on("close")`: when the event fires relative to the response completing,
 * under an agent that REUSES the connection — which is how every production
 * client talks to a merchant.
 *
 * Fire too early and a served response looks undelivered and the ticket is
 * handed back, so one payment buys the resource forever. Fire too late, or not
 * at all on a reused socket, and the fix does nothing. Neither shows up in
 * supertest.
 */
describe("the release listener on a real socket with keep-alive", () => {
  const PAYER = "agent::1220aa";
  const MERCHANT = "merchant::1220bb";
  const FP = "fac::1220dd";
  const SYNC = "global-domain::1220ee";
  const REQ = {
    scheme: "exact" as const,
    network: "canton:devnet" as const,
    amount: "1000000000",
    asset: "canton-coin",
    payTo: MERCHANT,
    maxTimeoutSeconds: 60,
    extra: {
      assetTransferMethod: "transfer-factory",
      feePayer: FP,
      synchronizerId: SYNC,
      instrumentId: { admin: FP, id: "CC" },
      executeBeforeSeconds: 120,
    },
  };

  const facilitatorFetch = (async (url: string) => {
    if (String(url).endsWith("/verify"))
      return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
    return new Response(
      JSON.stringify({ success: true, transaction: "u-live", payer: PAYER }),
      { status: 200 }
    );
  }) as unknown as typeof fetch;

  async function run(handler: express.RequestHandler, path: string) {
    const redeemed = createInMemoryRedeemedStore();
    const app = express();
    app.use(
      cantonPaymentMiddleware({
        routes: {
          [`GET ${path}`]: { accepts: [REQ] },
        },
        facilitatorUrl: "http://fac.test",
        fetch: facilitatorFetch,
        redeemed,
      })
    );
    if (handler) app.get(path, handler);
    const server = http.createServer(app);
    await new Promise<void>((r) => server.listen(0, r));
    const port = (server.address() as { port: number }).port;
    const agent = new http.Agent({ keepAlive: true, maxSockets: 1 });
    const status = await new Promise<number>((resolve, reject) => {
      const req = http.request(
        {
          port, path, method: "GET", agent,
          headers: {
            "PAYMENT-SIGNATURE": encodeBase64Json({
              x402Version: 2,
              scheme: "exact",
              network: "canton:devnet",
              resource: { url: `http://127.0.0.1${path}` },
              accepted: REQ,
              payload: { assetTransferMethod: "transfer-factory", submissionRef: "sub-1" },
            }),
          },
        },
        (res) => { res.resume(); res.on("end", () => resolve(res.statusCode ?? 0)); }
      );
      req.on("error", reject);
      req.end();
    });
    await new Promise((r) => setTimeout(r, 60));
    const stillFree = await redeemed.claim("u-live");
    agent.destroy();
    await new Promise<void>((r) => server.close(() => r()));
    return { status, released: stillFree };
  }

  it("a served request keeps the ticket spent", async () => {
    const r = await run((_q, res) => { res.json({ ok: true }); }, "/paid");
    expect(r.status).toBe(200);
    expect(r.released).toBe(false);
  });

  it("a throwing handler gives the ticket back", async () => {
    const r = await run(() => { throw new Error("boom"); }, "/paid");
    expect(r.status).toBeGreaterThanOrEqual(500);
    expect(r.released).toBe(true);
  });

  it("a path the router does not serve gives it back too", async () => {
    const r = await run(undefined as unknown as express.RequestHandler, "/paid");
    expect(r.status).toBe(404);
    expect(r.released).toBe(true);
  });
});
