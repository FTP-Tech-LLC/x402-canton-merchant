# @ftptech/x402-canton-next

Next.js App Router wrapper that gates a Route Handler behind Canton
x402 payment via a facilitator.

## Install

```bash
npm i @ftptech/x402-canton-core @ftptech/x402-canton-next
```

> Note: the `@ftp` npm scope is not final and may change before the
> first public release (see [`docs/PUBLISHING.md`](https://github.com/sunstrike228/canton-x402/blob/main/docs/PUBLISHING.md)).
> Pin the version you install and check the README for the current
> package name.

## Quick example

```ts
// app/api/data/route.ts
import { withCantonPayment } from "@ftptech/x402-canton-next";

export const GET = withCantonPayment(
  async () => Response.json({ data: "premium" }),
  {
    accepts: [paymentRequirements],
    facilitatorUrl: process.env.FACILITATOR_URL!,
    description: "Premium data feed",
    mimeType: "application/json",
  }
);
```

Per-request flow matches `@ftptech/x402-canton-express`: facilitator
`/verify` → pre-handler `/settle` → handler → response with
`PAYMENT-RESPONSE` header attached.

## No `next` dependency

This package uses plain Web Fetch `Request`/`Response`/`Headers`.
Next.js's `NextRequest` extends `Request`, so the wrapper composes
without pinning a `next` version.

## Project

[github.com/sunstrike228/canton-x402](https://github.com/sunstrike228/canton-x402).
