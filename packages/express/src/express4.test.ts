/**
 * The middleware supports Express 4 and 5 (peerDependencies). These run the
 * layouts where the two differ, each request carrying a valid payment, so
 * "not charged" means the facilitator was never called.
 */
import { describe, it, expect, vi, beforeAll, afterAll } from "vitest";
import { createRequire } from "node:module";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connect } from "node:net";
import type { AddressInfo } from "node:net";
import request from "supertest";
import express5 from "express";
import {
  HEADER_PAYMENT_SIGNATURE_V2,
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

/** The default redeemed store is one per process: every settle needs its own id. */
let settled = 0;

/** A facilitator that verifies and settles every payment. */
function facilitator() {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.endsWith("/verify")) return Response.json({ isValid: true, payer: PAYER });
    return Response.json({ success: true, transaction: `upd-${++settled}`, payer: PAYER, network: "canton:devnet" });
  });
}

function gate(fetch: ReturnType<typeof facilitator>, key: string) {
  return cantonPaymentMiddleware({
    routes: { [key]: { accepts: [requirements()] } },
    facilitatorUrl: "http://fac.test",
    fetch: fetch as unknown as typeof globalThis.fetch,
  });
}

/** Gate on the app, paid and free routes on a Router mounted at /api. */
function gateOnApp(express: ExpressFn, settings: Record<string, boolean> = {}) {
  const fetch = facilitator();
  const served = { paid: 0 };
  const app = express();
  for (const [k, v] of Object.entries(settings)) app.set(k, v);
  app.use(gate(fetch, "GET /api/data"));
  const api = express.Router();
  api.get("/data", (_req, res) => {
    served.paid++;
    res.json({ data: "premium-payload" });
  });
  api.get("/free", (_req, res) => {
    res.json({ free: true });
  });
  app.use("/api", api);
  return { app, fetch, served };
}

/** Gate mounted at /api, paid route on the app. */
function gateOnPrefix(express: ExpressFn) {
  const fetch = facilitator();
  const served = { paid: 0 };
  const app = express();
  app.use("/api", gate(fetch, "GET /api/data"));
  app.get("/api/data", (_req, res) => {
    served.paid++;
    res.json({ data: "premium-payload" });
  });
  return { app, fetch, served };
}

const SLASHED = ["/api//data", "/API//data/", "//api/data", "/api///data?x=1"];

/** A paid file and a free one, served by express.static. */
let filesDir = "";
beforeAll(() => {
  filesDir = mkdtempSync(join(tmpdir(), "x402-static-"));
  writeFileSync(join(filesDir, "report.pdf"), "premium-payload");
  writeFileSync(join(filesDir, "free.txt"), "free-file");
  writeFileSync(join(filesDir, "my report.pdf"), "premium-payload");
  writeFileSync(join(filesDir, "report[1].pdf"), "premium-payload");
  mkdirSync(join(filesDir, "sub"));
  mkdirSync(join(filesDir, "docs"));
  writeFileSync(join(filesDir, "docs", "index.html"), "premium-payload");
});
afterAll(() => rmSync(filesDir, { recursive: true, force: true }));

/** Spellings of /files/report.pdf that express.static resolves to that file. */
const RESPELLED = [
  "/files/./report.pdf",
  "/files/sub/../report.pdf",
  "/files/%72eport.pdf",
  "/files/%2E/report.pdf",
  "/files/.%2Freport.pdf",
  "/files/sub/..%2Freport.pdf",
  "/FILES/./report.pdf",
  "/files/report.pdf/.",
];

describe.each([
  ["Express 4", express4],
  ["Express 5", express5],
])("%s", (_name, express) => {
  it.each([
    ["gate on the app, route on a mounted Router", gateOnApp],
    ["gate mounted at /api, route on the app", gateOnPrefix],
  ])("%s: serves the paid route for a valid payment", async (_layout, build) => {
    const { app, fetch, served } = build(express);
    const r = await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
    expect(r.status).toBe(200);
    expect(served.paid).toBe(1);
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["gate on the app, route on a mounted Router", gateOnApp],
    ["gate mounted at /api, route on the app", gateOnPrefix],
  ])("%s: refuses a paid path with repeated slashes before any charge", async (_layout, build) => {
    const { app, fetch, served } = build(express);
    for (const path of SLASHED) {
      for (const method of ["get", "head"] as const) {
        const r = await request(app)[method](path).set(HEADER_PAYMENT_SIGNATURE_V2, signature);
        // 400 from the gate; 404 when the path never reaches the gate's mount.
        expect([400, 404], `${method} ${path}: ${r.status}`).toContain(r.status);
        if (path !== "//api/data") expect(r.status, `${method} ${path}`).toBe(400);
      }
    }
    expect(fetch).not.toHaveBeenCalled();
    expect(served.paid).toBe(0);
  });

  it("passes a free path with repeated slashes on", async () => {
    const { app, fetch } = gateOnApp(express);
    const r = await request(app).get("/api//free");
    expect(r.status).not.toBe(400);
    if (express === express4) expect(r.body).toEqual({ free: true });
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    ["strict routing", "/api/data/"],
    ["case sensitive routing", "/api/DATA"],
  ])("%s on the app, paid route on a Router: %s is refused, not served free", async (flag, path) => {
    // An express.Router() ignores the app's routing flags and serves this
    // spelling; the gate reads it through the app's flags and finds no key.
    const { app, fetch, served } = gateOnApp(express, { [flag]: true });
    for (const method of ["get", "head"] as const) {
      const r = await request(app)[method](path).set(HEADER_PAYMENT_SIGNATURE_V2, signature);
      expect(r.status, `${method} ${path}`).toBe(404);
    }
    expect(served.paid).toBe(0);
    expect((await request(app).get("/api/data")).status).toBe(402);
    expect((await request(app).get("/api/free")).body).toEqual({ free: true });
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    ["strict routing", "/api/data/", "true"],
    ["case sensitive routing", "/api/DATA", 1],
  ])("%s set to a truthy non-boolean: %s on the app's own route is refused, not charged", async (flag, path, value) => {
    // Express turns a flag on for any truthy value; reading only `true` left
    // the gate loose while the app's router was strict: settle, then 404.
    const fetch = facilitator();
    let served = 0;
    const app = express();
    app.set(flag, value);
    app.use(gate(fetch, "GET /api/data"));
    app.get("/api/data", (_req, res) => {
      served++;
      res.json({ data: "premium-payload" });
    });
    const r = await request(app).get(path).set(HEADER_PAYMENT_SIGNATURE_V2, signature);
    expect(r.status).toBe(404);
    expect(fetch).not.toHaveBeenCalled();
    expect(served).toBe(0);
    const ok = await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
    expect(ok.status).toBe(200);
    expect(served).toBe(1);
  });

  it.each([
    ["strict routing", "/api//data/"],
    ["case sensitive routing", "/api//DATA"],
  ])("%s on the app, repeated slashes too: %s is refused, not served free", async (flag, path) => {
    // Express 4 drops the extra slash at the Router's mount, and the Router
    // ignores the app's flags: both spelling tricks at once.
    const { app, fetch, served } = gateOnApp(express, { [flag]: true });
    for (const method of ["get", "head"] as const) {
      const r = await request(app)[method](path).set(HEADER_PAYMENT_SIGNATURE_V2, signature);
      expect(r.status, `${method} ${path}`).toBe(400);
    }
    expect(served.paid).toBe(0);
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    ["gate on the app, route on a mounted Router", gateOnApp],
    ["gate mounted at /api, route on the app", gateOnPrefix],
  ])("%s: never serves a paid path spelled with a backslash for free", async (_layout, build) => {
    // Express turns "\" into "/" for req.path; the raw URL keeps it.
    const { app, fetch, served } = build(express);
    await withServer(app, async (raw) => {
      for (const target of ["/api\\/data#f", "/api\\data#f", "/API\\/data/#f"]) {
        const answer = await raw(target);
        expect(answer, target).not.toContain("premium-payload");
      }
    });
    expect(served.paid).toBe(0);
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    ["gate on the app", (app: ReturnType<ExpressFn>, g: ReturnType<typeof gate>) => app.use(g)],
    ["gate mounted at /files", (app: ReturnType<ExpressFn>, g: ReturnType<typeof gate>) => app.use("/files", g)],
  ])("%s: a paid static file is not free under another spelling of its path", async (_layout, mount) => {
    const fetch = facilitator();
    const app = express();
    mount(app, gate(fetch, "GET /files/report.pdf"));
    app.use("/files", express.static(filesDir));
    await withServer(app, async (raw) => {
      for (const target of RESPELLED) {
        const answer = await raw(target);
        expect(answer, target).not.toContain("premium-payload");
        expect(answer.split("\r\n")[0], target).toBe("HTTP/1.1 400 Bad Request");
      }
      for (const target of ["/files/free.txt", "/files/./free.txt", "/files/%66ree.txt"]) {
        const answer = await raw(target);
        expect(answer.split("\r\n")[0], target).toBe("HTTP/1.1 200 OK");
        expect(answer, target).toContain("free-file");
      }
      expect((await raw("/files/report.pdf")).split("\r\n")[0]).toBe("HTTP/1.1 402 Payment Required");
    });
    expect(fetch).not.toHaveBeenCalled();
    const paid = await request(app)
      .get("/files/report.pdf")
      .set(HEADER_PAYMENT_SIGNATURE_V2, signature)
      .responseType("blob");
    expect(paid.status).toBe(200);
    expect(String(paid.body)).toBe("premium-payload");
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it("charges a file whose key is written as the URL is sent, and refuses one written decoded at startup", async () => {
    const fetch = facilitator();
    const encoded = express();
    encoded.use(gate(fetch, "GET /files/my%20report.pdf"));
    encoded.use("/files", express.static(filesDir));
    // A raw space never arrives in a request path, so this key matched nothing.
    expect(() => gate(fetch, "GET /files/my report.pdf")).toThrow(/is not "METHOD \/literal\/path"/);
    expect((await request(encoded).get("/files/my%20report.pdf")).status).toBe(402);
    expect(fetch).not.toHaveBeenCalled();
    const paid = await request(encoded)
      .get("/files/my%20report.pdf")
      .set(HEADER_PAYMENT_SIGNATURE_V2, signature)
      .responseType("blob");
    expect(paid.status).toBe(200);
    expect(String(paid.body)).toBe("premium-payload");
  });

  it("a key written with escapes is not free under another spelling of its file either", async () => {
    const fetch = facilitator();
    const app = express();
    app.use(gate(fetch, "GET /files/my%20report.pdf"));
    app.use(gate(fetch, "GET /files/report%5B1%5D.pdf"));
    app.use("/files", express.static(filesDir));
    await withServer(app, async (raw) => {
      for (const target of [
        "/files/./my%20report.pdf",
        "/files/sub/../my%20report.pdf",
        "/files/my%20%72eport.pdf",
        "/files/%6Dy%20report.pdf",
        "/files/my%20report%2Epdf",
        "/files/report[1].pdf",
        "/files/./report[1].pdf",
      ]) {
        const answer = await raw(target);
        expect(answer, target).not.toContain("premium-payload");
        expect(answer.split("\r\n")[0], target).toBe("HTTP/1.1 400 Bad Request");
      }
      for (const target of ["/files/my%20report.pdf", "/files/report%5B1%5D.pdf"]) {
        expect((await raw(target)).split("\r\n")[0], target).toBe("HTTP/1.1 402 Payment Required");
      }
    });
    expect(fetch).not.toHaveBeenCalled();
  });

  it("with both routing flags on, a paid file is charged only under its own spelling", async () => {
    // The file system tells case and a trailing slash apart; with the flags
    // off the middleware folds them as the router does, charges, and
    // express.static answers 404.
    const fetch = facilitator();
    const app = express();
    app.set("strict routing", true);
    app.set("case sensitive routing", true);
    app.use(gate(fetch, "GET /files/report.pdf"));
    app.use("/files", express.static(filesDir));
    for (const path of ["/files/REPORT.pdf", "/files/report.pdf/"]) {
      const r = await request(app).get(path).set(HEADER_PAYMENT_SIGNATURE_V2, signature);
      expect(r.status, path).toBe(404);
    }
    expect(fetch).not.toHaveBeenCalled();
    const paid = await request(app)
      .get("/files/report.pdf")
      .set(HEADER_PAYMENT_SIGNATURE_V2, signature)
      .responseType("blob");
    expect(paid.status).toBe(200);
    expect(String(paid.body)).toBe("premium-payload");
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["the route on the app", (app: ReturnType<ExpressFn>, g: ReturnType<typeof gate>) => {
      app.use("/api", g);
      app.get("/api", (_req, res) => res.json({ data: "premium-payload" }));
    }],
    ["the route on a Router", (app: ReturnType<ExpressFn>, g: ReturnType<typeof gate>) => {
      const r = express.Router();
      r.use(g);
      r.get("/", (_req, res) => res.json({ data: "premium-payload" }));
      app.use("/api", r);
    }],
  ])("strict routing, a paid mount root, %s: /api is charged and served, /api/ is refused", async (_l, mount) => {
    const fetch = facilitator();
    const app = express();
    app.set("strict routing", true);
    mount(app, gate(fetch, "GET /api"));
    for (const method of ["get", "head"] as const) {
      const r = await request(app)[method]("/api/").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
      expect(r.status, `${method} /api/`).toBe(404);
    }
    expect(fetch).not.toHaveBeenCalled();
    const paid = await request(app).get("/api").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
    expect(paid.status).toBe(200);
    expect(paid.body).toEqual({ data: "premium-payload" });
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it("default flags, a paid mount root: /api and /api/ are both charged and served", async () => {
    const fetch = facilitator();
    const app = express();
    app.use("/api", gate(fetch, "GET /api"));
    app.get("/api", (_req, res) => res.json({ data: "premium-payload" }));
    for (const path of ["/api", "/api/"]) {
      const r = await request(app).get(path).set(HEADER_PAYMENT_SIGNATURE_V2, signature);
      expect(r.status, path).toBe(200);
    }
    expect(fetch).toHaveBeenCalledTimes(4);
  });

  it("strict routing, a mount root keyed with its slash: /api is refused, not charged for a 404", async () => {
    const fetch = facilitator();
    const app = express();
    app.set("strict routing", true);
    app.use("/api", gate(fetch, "GET /api/"));
    app.get("/api/", (_req, res) => res.json({ data: "premium-payload" }));
    const r = await request(app).get("/api").set(HEADER_PAYMENT_SIGNATURE_V2, signature);
    expect(r.status).toBe(404);
    expect(fetch).not.toHaveBeenCalled();
    expect((await request(app).get("/api/").set(HEADER_PAYMENT_SIGNATURE_V2, signature)).status).toBe(200);
  });

  it("strict routing, a paid directory: the redirect to a refused spelling gives the payment back", async () => {
    // express.static answers /files/docs with 301 to /files/docs/, which the
    // flags refuse. The payment must stay usable, not be spent on a redirect.
    let calls = 0;
    const id = `upd-docs-fixed-${++settled}`;
    const fixed = vi.fn(async (input: RequestInfo | URL) => {
      calls++;
      if (String(input).endsWith("/verify")) return Response.json({ isValid: true, payer: PAYER });
      return Response.json({ success: true, transaction: id, payer: PAYER, network: "canton:devnet" });
    });
    const app = express();
    app.set("strict routing", true);
    app.set("case sensitive routing", true);
    app.use(gate(fixed, "GET /files/docs"));
    app.use("/files", express.static(filesDir));
    for (let i = 0; i < 2; i++) {
      const r = await request(app).get("/files/docs").set(HEADER_PAYMENT_SIGNATURE_V2, signature).redirects(0);
      expect(r.status, `attempt ${i + 1}`).toBe(301);
    }
    expect(calls).toBe(4);
  });

  it("strict routing, a directory keyed both ways with one config: pay, follow, read", async () => {
    let calls = 0;
    const id = `upd-docs-both-${++settled}`;
    const fixed = vi.fn(async (input: RequestInfo | URL) => {
      calls++;
      if (String(input).endsWith("/verify")) return Response.json({ isValid: true, payer: PAYER });
      return Response.json({ success: true, transaction: id, payer: PAYER, network: "canton:devnet" });
    });
    const config = { accepts: [requirements()] };
    const app = express();
    app.set("strict routing", true);
    app.set("case sensitive routing", true);
    app.use(
      cantonPaymentMiddleware({
        routes: { "GET /files/docs": config, "GET /files/docs/": config },
        facilitatorUrl: "http://fac.test",
        fetch: fixed as unknown as typeof globalThis.fetch,
      })
    );
    app.use("/files", express.static(filesDir));
    const first = await request(app).get("/files/docs").set(HEADER_PAYMENT_SIGNATURE_V2, signature).redirects(0);
    expect(first.status).toBe(301);
    const second = await request(app)
      .get("/files/docs/")
      .set(HEADER_PAYMENT_SIGNATURE_V2, signature)
      .responseType("blob");
    expect(second.status).toBe(200);
    expect(String(second.body)).toBe("premium-payload");
    expect(calls).toBe(4);
  });

  it("refuses to start when two keys with different terms are one file", () => {
    const routes = {
      "GET /files/my%20report.pdf": { accepts: [requirements()] },
      "GET /files/my%20%72eport.pdf": { accepts: [requirements()] },
    };
    expect(() => cantonPaymentMiddleware({ routes, facilitatorUrl: "http://fac.test" })).toThrow(/one file to express.static/);
    const same = { accepts: [requirements()] };
    expect(() =>
      cantonPaymentMiddleware({
        routes: { "GET /files/my%20report.pdf": same, "GET /files/my%20%72eport.pdf": same },
        facilitatorUrl: "http://fac.test",
      })
    ).not.toThrow();
  });

  it("answers 400 to an absolute-form target that leaves no path, and keeps serving", async () => {
    const fetch = facilitator();
    const app = express();
    app.use("/api", gate(fetch, "GET /api"));
    app.get("/api", (_req, res) => {
      res.json({ data: "premium-payload" });
    });
    await withServer(app, async (raw) => {
      for (const target of ["foo://h/api", "foo://h/api/"]) {
        const answer = await raw(target);
        expect(answer.split("\r\n")[0], target).toBe("HTTP/1.1 400 Bad Request");
        expect(answer).not.toContain("premium-payload");
      }
      expect((await raw("/api")).split("\r\n")[0]).toBe("HTTP/1.1 402 Payment Required");
    });
    expect(fetch).not.toHaveBeenCalled();
  });
});

/** Run `body` against `app` on a real port, sending request targets as written. */
async function withServer(
  app: ReturnType<ExpressFn>,
  body: (raw: (target: string) => Promise<string>) => Promise<void>
): Promise<void> {
  const server = app.listen(0, "127.0.0.1");
  await new Promise<void>((r) => server.once("listening", () => r()));
  const { port } = server.address() as AddressInfo;
  const raw = (target: string) =>
    new Promise<string>((resolve, reject) => {
      // write, not end: a half-closed client makes Node drop a response that
      // is not written in the same tick, and an empty answer proves nothing.
      const s = connect(port, "127.0.0.1", () =>
        s.write(`GET ${target} HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n`)
      );
      let data = "";
      s.on("data", (c) => (data += String(c)));
      s.on("close", () =>
        data.startsWith("HTTP/1.1 ") ? resolve(data) : reject(new Error(`no answer to ${target}`))
      );
      s.on("error", reject);
    });
  try {
    await body(raw);
  } finally {
    server.close();
  }
}
