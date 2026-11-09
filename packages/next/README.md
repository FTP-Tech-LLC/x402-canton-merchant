# @ftptech/x402-canton-next

Next.js App Router wrapper that charges per request in Canton Coin or a CIP-56 token. The facilitator verifies and settles the payment before your Route Handler runs.

## Install

```bash
npm install @ftptech/x402-canton-next
```

## Before you start

The party that receives payments needs a live `TransferPreapproval`:

```bash
npx @ftptech/canton-agent-wallet create --relay-url https://facilitator.ftptech.xyz
npx @ftptech/canton-agent-wallet address       # your party id: payTo
# send it a few CC, then
npx @ftptech/canton-agent-wallet claim
npx @ftptech/canton-agent-wallet preapproval   # lasts 30 days
```

## Quickstart

```ts
// app/api/data/route.ts
import { withCantonPayment } from "@ftptech/x402-canton-next";

// FTP MainNet values, copy as is (current ones: https://facilitator.ftptech.xyz/supported)
const DSO = "DSO::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc";
const price = {
  scheme: "exact",
  network: "canton:mainnet",
  asset: "CC",
  amount: "100000000", // 0.01 CC (1 CC = 10^10)
  payTo: "<your party id>",
  maxTimeoutSeconds: 120,
  extra: {
    assetTransferMethod: "transfer-factory",
    feePayer: "ftp-x402-infrastructure::1220da867ceb28e1531ebfbea9ca9b25f75be06755999f48f0ec4b96f4174e3cd133",
    synchronizerId: "global-domain::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc",
    instrumentId: { admin: DSO, id: "Amulet" },
    executeBeforeSeconds: 120,
  },
} as const;

export const GET = withCantonPayment(
  async () => Response.json({ data: "paid" }),
  { accepts: [price], facilitatorUrl: "https://facilitator.ftptech.xyz", description: "Premium data" },
);
```

`curl -i http://localhost:3000/api/data` now answers 402.

## Money safety

- One payment, one response: a replay gets 402 `payment_already_redeemed`. On serverless or several instances, pass one shared `redeemed` store; otherwise the same payment is served once per instance.
- Settlement runs before your handler. If it throws or answers 5xx, the payer can retry the same payment.
- A 502 means the facilitator could not confirm the payment, for example `settlement_pending`. The payer retries the same payment and never pays again.
- The payment header is 8 to 10 KiB. Every proxy in front must accept 16 KiB: nginx `large_client_header_buffers 4 16k;`, Node `--max-http-header-size=32768`.
- The preapproval lasts 30 days, and payments to an expired one fail. Run `preapproval` again before then; `--force` renews early.
- `~/.canton-agent/wallet.json` holds the key of the party you are paid to. Back it up with `npx @ftptech/canton-agent-wallet export`.
- No `next` dependency: the wrapper uses the Web `Request` and `Response` types.

## Docs

[Merchant integration](https://x402-docs.ftptech.xyz/x402-merchant) on the FTP docs site.
