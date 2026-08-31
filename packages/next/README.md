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

## Merchant HTTP requirements: the payment header is ~8-10 KiB

The inline payment travels in one request header. Its prepared transaction is
gzipped, but the header is a base64 JSON envelope, so on real MainNet payments
the header is **~8-10 KiB** — and an x402 server must accept a payment header
of **at least 16 KiB end to end** (every proxy in front of it included).
Defaults that silently break this: nginx (8k buffers) answers
`400 Request Header Or Cookie Too Large` before your app ever runs; a Node
origin caps ALL headers at 16 KiB combined.

```nginx
# nginx in front of the merchant
large_client_header_buffers 4 16k;
```

```bash
# Node origin
node --max-http-header-size=32768 server.js
```

Cloudflare passes these sizes by default. A payer hitting the limit sees the
client throw `MERCHANT_HEADER_LIMIT` naming this exact fix.

## No `next` dependency

This package uses plain Web Fetch `Request`/`Response`/`Headers`.
Next.js's `NextRequest` extends `Request`, so the wrapper composes
without pinning a `next` version.

## Project

[github.com/sunstrike228/canton-x402](https://github.com/sunstrike228/canton-x402).
