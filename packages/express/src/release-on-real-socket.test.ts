import { describe, it, expect } from "vitest";
import express from "express";
import { createRequire } from "node:module";
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

  // A ticket of its own per app: the gate remembers once-only give-backs per
  // ticket for the life of the process.
  let seq = 0;
  const facilitatorFor = (transaction: string) =>
    (async (url: string) => {
      if (String(url).endsWith("/verify"))
        return new Response(JSON.stringify({ isValid: true, payer: PAYER }), { status: 200 });
      return new Response(JSON.stringify({ success: true, transaction, payer: PAYER }), {
        status: 200,
      });
    }) as unknown as typeof fetch;

  // Express 4 too: the peer range allows it, and its routing sets req.route
  // the same way only if we check.
  const express4 = createRequire(import.meta.url)("express4") as typeof express;

  /** One app behind the gate; `get` sends one paid request, every time with
   *  the same payment. */
  async function serve(
    handler: express.RequestHandler,
    path: string,
    o: { after?: express.RequestHandler; lib?: typeof express } = {}
  ) {
    const tx = `u-live-${++seq}`;
    const redeemed = createInMemoryRedeemedStore();
    const app = (o.lib ?? express)();
    app.use(
      cantonPaymentMiddleware({
        routes: {
          [`GET ${path}`]: { accepts: [REQ] },
        },
        facilitatorUrl: "http://fac.test",
        fetch: facilitatorFor(tx),
        redeemed,
      })
    );
    if (o.after) app.use(o.after);
    if (handler) app.get(path, handler);
    const server = http.createServer(app);
    await new Promise<void>((r) => server.listen(0, r));
    const port = (server.address() as { port: number }).port;
    const agent = new http.Agent({ keepAlive: true, maxSockets: 1 });
    const get = async (): Promise<number> => {
      const status = await send(port, path, agent);
      await new Promise((r) => setTimeout(r, 60));
      return status;
    };
    const close = async () => {
      agent.destroy();
      await new Promise<void>((r) => server.close(() => r()));
    };
    return { get, close, released: () => redeemed.claim(tx) };
  }

  async function run(
    handler: express.RequestHandler,
    path: string,
    o: { after?: express.RequestHandler; lib?: typeof express } = {}
  ) {
    const s = await serve(handler, path, o);
    const status = await s.get();
    const stillFree = await s.released();
    await s.close();
    return { status, released: stillFree };
  }

  function send(port: number, path: string, agent: http.Agent): Promise<number> {
    return new Promise<number>((resolve, reject) => {
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

  const served: express.RequestHandler = (_q, res) => {
    res.json({ ok: true });
  };
  const denyAuth: express.RequestHandler = (_q, res) => {
    res.status(401).json({ error: "unauthorized" });
  };
  const badBody: express.RequestHandler = (_q, res) => {
    res.status(400).json({ error: "bad body" });
  };

  it("a 401 from auth middleware after the gate gives the ticket back", async () => {
    const r = await run(served, "/paid", { after: denyAuth });
    expect(r.status).toBe(401);
    expect(r.released).toBe(true);
  });

  it("a 400 from middleware after the gate gives it back", async () => {
    const r = await run(served, "/paid", { after: badBody });
    expect(r.status).toBe(400);
    expect(r.released).toBe(true);
  });

  it("a second middleware 4xx on the same payment keeps it spent", async () => {
    // A handler mounted with app.use may answer a 4xx as its paid answer; the
    // payment is not bound to the query, so it is given back only once.
    const s = await serve(served, "/paid", { after: denyAuth });
    expect(await s.get()).toBe(401); // given back: the retry gets through
    expect(await s.get()).toBe(401); // not again
    expect(await s.released()).toBe(false);
    await s.close();
  });

  it("a 404 from the router is given back once too", async () => {
    // An app.use handler's own 404 ("not registered") looks the same to the
    // gate, and it may be the paid answer.
    const s = await serve(undefined as unknown as express.RequestHandler, "/paid");
    expect(await s.get()).toBe(404); // given back
    expect(await s.get()).toBe(404); // not again
    expect(await s.released()).toBe(false);
    await s.close();
  });

  it("a 4xx the route handler chose keeps it spent: the handler answered", async () => {
    const r = await run((_q, res) => { res.status(403).json({ error: "no such record" }); }, "/paid");
    expect(r.status).toBe(403);
    expect(r.released).toBe(false);
  });

  it("Express 4: middleware 401 gives the ticket back, a served route keeps it", async () => {
    const denied = await run(served, "/paid", { after: denyAuth, lib: express4 });
    expect(denied.status).toBe(401);
    expect(denied.released).toBe(true);
    const ok = await run(served, "/paid", { lib: express4 });
    expect(ok.status).toBe(200);
    expect(ok.released).toBe(false);
    const missing = await run(undefined as unknown as express.RequestHandler, "/paid", { lib: express4 });
    expect(missing.status).toBe(404);
    expect(missing.released).toBe(true);
  });
});
