# @ftptech/x402-canton-express

Express middleware that gates routes behind Canton x402 payment via
a facilitator.

## Install

```bash
npm i @ftptech/x402-canton-core @ftptech/x402-canton-express
```

> Note: the `@ftp` npm scope is not final and may change before the
> first public release (see [`docs/PUBLISHING.md`](https://github.com/sunstrike228/canton-x402/blob/main/docs/PUBLISHING.md)).
> Pin the version you install and check the README for the current
> package name.

## Quick example

```ts
import express from "express";
import { cantonPaymentMiddleware } from "@ftptech/x402-canton-express";

const app = express();
app.use(express.json());
app.use(
  cantonPaymentMiddleware({
    routes: {
      "GET /api/data": {
        accepts: [paymentRequirements],
        description: "Premium data feed",
      },
    },
    facilitatorUrl: process.env.FACILITATOR_URL!,
  })
);

app.get("/api/data", (_req, res) => res.json({ data: "premium" }));
```

Per-request flow:
1. No `PAYMENT-SIGNATURE` → 402 with `PAYMENT-REQUIRED` carrying
   `accepts[]` and the resource URL.
2. With signature → calls facilitator `/verify`. If invalid → 402
   with the `invalidReason`.
3. If valid → calls facilitator `/settle` (pre-handler). On
   success, attaches `PAYMENT-RESPONSE` and runs the handler.

## Peer dependency

Express 4 or 5.

## Project

[github.com/sunstrike228/canton-x402](https://github.com/sunstrike228/canton-x402).
