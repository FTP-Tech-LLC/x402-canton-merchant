/**
 * Route keys, a payer who leaves mid-payment, and shared caches, on Express 4
 * and 5. Every paid request carries a valid payment.
 */
import { describe, it, expect, vi } from "vitest";
import { createRequire } from "node:module";
import { connect } from "node:net";
import type { AddressInfo } from "node:net";
import request from "supertest";
import express5 from "express";
import {
  HEADER_PAYMENT_SIGNATURE_V2,
  createInMemoryRedeemedStore,
  encodeBase64Json,
  type PaymentRequirements,
} from "@ftptech/x402-canton-core";
import { cantonPaymentMiddleware } from "./index.js";

const express4 = createRequire(import.meta.url)("express4") as typeof express5;
type ExpressFn = typeof express5;
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
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Verifies everything; /settle answers `id` once `gate` resolves. */
function facilitator(id: string, gate: Promise<void> = Promise.resolve(), onSettle: () => void = () => {}) {
  return vi.fn(async (input: RequestInfo | URL) => {
    if (String(input).endsWith("/verify")) return Response.json({ isValid: true, payer: PAYER });
    onSettle();
    await gate;
    return Response.json({ success: true, transaction: id, payer: PAYER, network: "canton:devnet" });
  });
}

function gate(fetch: ReturnType<typeof facilitator>, redeemed = createInMemoryRedeemedStore()) {
  return cantonPaymentMiddleware({
    routes: { "GET /api/data": { accepts: [requirements()] } },
    facilitatorUrl: "http://fac.test",
    fetch: fetch as unknown as typeof globalThis.fetch,
    redeemed,
  });
}

/** Start `app`, send one paid GET on a raw socket, and hand the socket back. */
async function rawPaidGet(app: ReturnType<ExpressFn>) {
  const server = app.listen(0, "127.0.0.1");
  await new Promise<void>((r) => server.once("listening", () => r()));
  const { port } = server.address() as AddressInfo;
  const socket = connect(port, "127.0.0.1", () =>
    socket.write(`GET /api/data HTTP/1.1\r\nHost: h\r\n${HEADER_PAYMENT_SIGNATURE_V2}: ${signature}\r\n\r\n`)
  );
  socket.on("error", () => {});
  return { server, socket };
}

describe.each([
  ["Express 4", express4],
  ["Express 5", express5],
])("%s", (_name, express) => {
  it("refuses at startup a route key that is a pattern or has no method", () => {
    for (const key of [
      "GET /api/items/:id",
      "GET /premium/*",
      "GET /docs/[slug]",
      "GET /docs/[...slug]",
      "GET /docs/[[...slug]]/x",
      "GET /api/data?x=1",
      "GET /api/data#top",
      "GET /files/my report.pdf",
      "/api/data",
      "GET  /api/data",
    ]) {
      expect(
        () =>
          cantonPaymentMiddleware({ routes: { [key]: { accepts: [requirements()] } }, facilitatorUrl: "http://fac.test" }),
        key
      ).toThrow(/is not "METHOD \/literal\/path"/);
    }
    for (const key of [
      "GET /api/data",
      "post /api/data",
      "GET /v1/party/agent::1220abc/quote",
      "GET /files/report[1].pdf",
      "GET /img/logo[2x].png",
      "GET /files/my%20report.pdf",
    ]) {
      expect(
        () =>
          cantonPaymentMiddleware({ routes: { [key]: { accepts: [requirements()] } }, facilitatorUrl: "http://fac.test" }),
        key
      ).not.toThrow();
    }
  });

  it("a payer who leaves during /settle is not charged a delivery: the same payment is served on retry", async () => {
    const id = `upd-gone-settle-${++seq}`;
    let open!: () => void;
    const settled = new Promise<void>((r) => (open = r));
    let entered!: () => void;
    const inSettle = new Promise<void>((r) => (entered = r));
    const fetch = facilitator(id, settled, entered);
    const redeemed = createInMemoryRedeemedStore();
    let served = 0;
    const app = express();
    app.use(gate(fetch, redeemed));
    app.get("/api/data", (_req, res) => {
      served++;
      res.json({ data: "premium-payload" });
    });
    const { server, socket } = await rawPaidGet(app);
    try {
      await inSettle;
      socket.destroy();
      await sleep(50);
      open();
      await sleep(50);
      expect(served).toBe(0);
      const retry = await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
      expect(retry.status).toBe(200);
      expect(served).toBe(1);
    } finally {
      server.close();
    }
  });

  it("a connection that drops after the handler started gets the payment back once: at most two runs", async () => {
    const id = `upd-gone-handler-${++seq}`;
    const fetch = facilitator(id);
    const redeemed = createInMemoryRedeemedStore();
    let served = 0;
    let started!: () => void;
    const inHandler = new Promise<void>((r) => (started = r));
    const app = express();
    app.use(gate(fetch, redeemed));
    app.get("/api/data", async (_req, res) => {
      served++;
      started();
      await sleep(100);
      res.json({ data: "premium-payload" });
    });
    const first = await rawPaidGet(app);
    try {
      await inHandler;
      first.socket.destroy();
      await sleep(150);
      expect(served).toBe(1);
      // The retry a proxy timeout calls for runs again, once.
      started = () => {};
      const second = await rawPaidGet(app);
      await sleep(30);
      second.socket.destroy();
      await sleep(150);
      second.server.close();
      expect(served).toBe(2);
      const replay = await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
      expect(replay.status).toBe(402);
      expect(served).toBe(2);
    } finally {
      first.server.close();
    }
  });

  it("a handler 5xx still gives the payment back", async () => {
    const id = `upd-5xx-${++seq}`;
    const fetch = facilitator(id);
    const redeemed = createInMemoryRedeemedStore();
    let calls = 0;
    const app = express();
    app.use(gate(fetch, redeemed));
    app.get("/api/data", (_req, res) => {
      calls++;
      if (calls === 1) res.status(502).json({ error: "upstream down" });
      else res.json({ data: "premium-payload" });
    });
    expect((await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature)).status).toBe(502);
    expect((await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature)).status).toBe(200);
  });

  it.each([
    ["no Cache-Control", (_req: unknown, res: express5.Response) => res.json({ data: "premium-payload" }), "private"],
    [
      "a public one",
      (_req: unknown, res: express5.Response) => {
        res.set("Cache-Control", "public, max-age=86400");
        res.json({ data: "premium-payload" });
      },
      "public, max-age=86400, private",
    ],
    [
      "one passed to writeHead",
      (_req: unknown, res: express5.Response) => {
        res.writeHead(200, { "Cache-Control": "public, max-age=60", "Content-Type": "text/plain" });
        res.end("premium-payload");
      },
      "public, max-age=60, private",
    ],
    [
      "raw headers passed to writeHead, as a proxy does",
      (_req: unknown, res: express5.Response) => {
        res.writeHead(200, ["cache-control", "public, max-age=300", "content-type", "text/plain"]);
        res.end("premium-payload");
      },
      "public, max-age=300, private",
    ],
    [
      "raw headers without Cache-Control",
      (_req: unknown, res: express5.Response) => {
        res.writeHead(200, ["content-type", "text/plain"]);
        res.end("premium-payload");
      },
      "private",
    ],
  ])("the paid response is private (handler with %s); the 402 is no-store", async (_l, handler, expected) => {
    const fetch = facilitator(`upd-cache-${++seq}`);
    const app = express();
    app.use(gate(fetch));
    app.get("/api/data", handler as never);
    const required = await request(app).get("/api/data");
    expect(required.status).toBe(402);
    expect(required.headers["cache-control"]).toBe("no-store");
    const paid = await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
    expect(paid.status).toBe(200);
    expect(paid.headers["cache-control"]).toBe(expected);
  });

  it("a Cache-Control the app set before the gate is kept and made private", async () => {
    const fetch = facilitator(`upd-cache-app-${++seq}`);
    const app = express();
    app.use((_req, res, next) => {
      res.set("Cache-Control", "no-cache");
      next();
    });
    app.use(gate(fetch));
    app.get("/api/data", (_req, res) => res.json({ data: "premium-payload" }));
    const paid = await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
    expect(paid.headers["cache-control"]).toBe("no-cache, private");
  });

  it("a headers object the paid handler shares with a free route is not edited", async () => {
    const fetch = facilitator(`upd-cache-shared-${++seq}`);
    const shared = { "Cache-Control": "public, max-age=60", "Content-Type": "text/plain" };
    const app = express();
    app.use(gate(fetch));
    app.get("/api/data", (_req, res) => {
      res.writeHead(200, shared);
      res.end("premium-payload");
    });
    app.get("/api/free", (_req, res) => {
      res.writeHead(200, shared);
      res.end("free");
    });
    const paid = await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
    expect(paid.headers["cache-control"]).toBe("public, max-age=60, private");
    expect(shared["Cache-Control"]).toBe("public, max-age=60");
    expect((await request(app).get("/api/free")).headers["cache-control"]).toBe("public, max-age=60");
  });
});
