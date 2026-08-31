/**
 * ONE INVARIANT, SWEPT: the payer is charged if and only if they are served.
 *
 * This middleware settles BEFORE `next()`, so a path it matches but Express then
 * does not route is a payment for nothing — and a path it fails to match on a
 * route that IS served is free access. Both directions have gone wrong here in
 * one day: gating only `baseUrl + path` broke mount-relative route keys, and the
 * loose reading that fixed that charged for URLs the app answered 404 to
 * (measured then: `404 settles=1`).
 *
 * Those were caught one case at a time. This sweeps the configuration space
 * instead — mounted vs not, strict routing on/off, case sensitivity on/off,
 * trailing slash, HEAD, encoded segments, params, query strings — and asserts
 * the same property everywhere, from the OUTSIDE: whatever the middleware
 * decided, `settles > 0` must agree with "the paid handler ran".
 *
 * Deliberately black-box. It does not read the route index or the normaliser; a
 * future rewrite of either keeps this test meaningful, which is the point.
 *
 * Every request carries a DISTINCT payment. The first version reused one
 * envelope across all of them, and the process-wide redeemed store correctly
 * refused the replays — producing a settle followed by a 402 that looked exactly
 * like "charged and refused" until it was measured. A sweep that replays one
 * payment tests the replay guard, not routing.
 */
import { describe, it, expect, vi } from "vitest";
import express from "express";
import request from "supertest";
import {
  HEADER_PAYMENT_SIGNATURE_V2,
  encodeBase64Json,
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

/** Copied from index.test.ts rather than invented: an envelope that does not
 *  pass `selectServerRequirements` yields a 402 with settles=0, which makes the
 *  whole sweep vacuously green. That is exactly what the first version did. */
let refCounter = 0;
function paymentSignature(): string {
  refCounter += 1;
  return encodeBase64Json({
    x402Version: 2,
    scheme: "exact",
    network: "canton:devnet",
    resource: { url: "http://127.0.0.1/api/data" },
    accepted: requirements(),
    payload: {
      assetTransferMethod: "transfer-factory",
      submissionRef: `sub-${refCounter}`,
    },
  });
}

/**
 * Counts what the middleware actually asked the facilitator to do.
 *
 * Each settle answers a DISTINCT `transaction`. The redeemed ticket is the
 * settle updateId, and it is claimed once per PROCESS — a fake that returns one
 * fixed id makes every request after the first a replay, which the middleware
 * correctly refuses with a post-settle 402. That reads identically to "charged
 * and refused" from the outside, and it is what the first two versions of this
 * sweep were actually measuring.
 */
let settleCounter = 0;
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
      settleCounter += 1;
      return new Response(
        JSON.stringify({ success: true, transaction: `1220u-${settleCounter}`, payer: PAYER }),
        { status: 200 }
      );
    }
    return new Response("not found", { status: 404 });
  }) as unknown as typeof globalThis.fetch;
  return { calls, fetchFn };
}

interface Shape {
  name: string;
  /** The route key handed to the middleware. */
  routeKey: string;
  /** Builds the app; the paid handler sets `res.locals.served`. */
  build: (mw: express.RequestHandler, served: () => void) => express.Express;
}

const SHAPES: Shape[] = [
  {
    name: "app-level route",
    routeKey: "GET /api/data",
    build: (mw, served) => {
      const app = express();
      app.use(mw);
      app.get("/api/data", (_q, r) => {
        served();
        r.json({ ok: true });
      });
      return app;
    },
  },
  {
    name: "sub-Router mounted at /api, key carries the mount",
    routeKey: "GET /api/data",
    build: (mw, served) => {
      const app = express();
      const r = express.Router();
      r.use(mw);
      r.get("/data", (_q, s) => {
        served();
        s.json({ ok: true });
      });
      app.use("/api", r);
      return app;
    },
  },
  {
    name: "sub-Router mounted at /api, key is mount-RELATIVE",
    routeKey: "GET /data",
    build: (mw, served) => {
      const app = express();
      const r = express.Router();
      r.use(mw);
      r.get("/data", (_q, s) => {
        served();
        s.json({ ok: true });
      });
      app.use("/api", r);
      return app;
    },
  },
  {
    name: "app with strict routing ON",
    routeKey: "GET /api/data",
    build: (mw, served) => {
      const app = express();
      app.set("strict routing", true);
      app.use(mw);
      app.get("/api/data", (_q, r) => {
        served();
        r.json({ ok: true });
      });
      return app;
    },
  },
  {
    name: "app with case-sensitive routing ON",
    routeKey: "GET /api/data",
    build: (mw, served) => {
      const app = express();
      app.set("case sensitive routing", true);
      app.use(mw);
      app.get("/api/data", (_q, r) => {
        served();
        r.json({ ok: true });
      });
      return app;
    },
  },
  {
    name: "nested mounts /v1 -> /api",
    routeKey: "GET /v1/api/data",
    build: (mw, served) => {
      const app = express();
      const outer = express.Router();
      const inner = express.Router();
      inner.use(mw);
      inner.get("/data", (_q, s) => {
        served();
        s.json({ ok: true });
      });
      outer.use("/api", inner);
      app.use("/v1", outer);
      return app;
    },
  },
];

/** URLs to try against every shape. The set deliberately mixes hits, misses and
 *  the near-misses that a normaliser gets wrong. */
const URLS = [
  "/api/data",
  "/api/data/",
  "/API/DATA",
  "/api/data?x=1",
  "/api/data//",
  "/api//data",
  "/api/data/extra",
  "/api/dat",
  "/api/data%2Fx",
  "/api/DATA",
  "/data",
  "/data/",
  "/v1/api/data",
  "/apidata",
  "/api/../api/data",
];

const METHODS = ["get", "head"] as const;

describe("settle happens IF AND ONLY IF the paid route is served", () => {
  for (const shape of SHAPES) {
    for (const method of METHODS) {
      it(`${shape.name} — ${method.toUpperCase()} across ${URLS.length} URLs`, async () => {
        const violations: string[] = [];
        for (const url of URLS) {
          const { calls, fetchFn } = countingFacilitator();
          let servedCount = 0;
          const app = shape.build(
            cantonPaymentMiddleware({
              routes: {
                [shape.routeKey]: {
                  accepts: [requirements()],
                  description: "Premium",
                  mimeType: "application/json",
                },
              },
              facilitatorUrl: FACILITATOR,
              fetch: fetchFn,
            }),
            () => {
              servedCount++;
            }
          );

          const res = await request(app)
            [method](url)
            .set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());

          // THE INVARIANT. Charging without serving is theft; serving the paid
          // route without charging is free access. Both are failures.
          if (servedCount > 0 && calls.settle === 0) {
            violations.push(`${method} ${url}: SERVED but settles=0 (free access)`);
          }
          if (servedCount === 0 && calls.settle > 0) {
            violations.push(
              `${method} ${url}: settles=${calls.settle} but the paid handler NEVER RAN ` +
                `(status ${res.status}) — the payer was charged for nothing`
            );
          }
        }
        expect(violations).toEqual([]);
      });
    }
  }

  it("the sweep is not vacuous: some URL in the set really is served AND charged", async () => {
    // A sweep whose every case is a 404 would pass while proving nothing.
    const { calls, fetchFn } = countingFacilitator();
    let served = 0;
    const app = SHAPES[0]!.build(
      cantonPaymentMiddleware({
        routes: {
          "GET /api/data": {
            accepts: [requirements()],
            description: "Premium",
            mimeType: "application/json",
          },
        },
        facilitatorUrl: FACILITATOR,
        fetch: fetchFn,
      }),
      () => {
        served++;
      }
    );
    await request(app).get("/api/data").set(HEADER_PAYMENT_SIGNATURE_V2, paymentSignature());
    expect(served).toBe(1);
    expect(calls.settle).toBe(1);
  });
});
