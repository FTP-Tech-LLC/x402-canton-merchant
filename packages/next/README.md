# @ftptech/x402-canton-next

Next.js App Router wrapper that charges per request in Canton Coin or a CIP-56 token. The payment is verified and settled before your Route Handler runs.

## Install

```bash
npm i @ftptech/x402-canton-next
```

## Before you start

You need a party to receive payments, with a live `TransferPreapproval`:

```bash
npx @ftptech/canton-agent-wallet create --relay-url https://facilitator.ftptech.xyz
npx @ftptech/canton-agent-wallet address       # your party id: payTo
# send a few CC to it, then
npx @ftptech/canton-agent-wallet claim
npx @ftptech/canton-agent-wallet preapproval   # lasts 30 days; renew early with --force
```

## Example

```ts
// lib/x402.ts
// FTP MainNet values, copy as is
const DSO = "DSO::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc";
export const extra = {
  assetTransferMethod: "transfer-factory",
  feePayer: "ftp-x402-infrastructure::1220da867ceb28e1531ebfbea9ca9b25f75be06755999f48f0ec4b96f4174e3cd133",
  synchronizerId: "global-domain::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc",
  instrumentId: { admin: DSO, id: "Amulet" },
  executeBeforeSeconds: 120,
} as const;

// Your price
export const price = {
  scheme: "exact",
  network: "canton:mainnet",
  asset: "CC",
  amount: "100000000", // 0.01 CC (1 CC = 10^10)
  payTo: "<your party id>",
  maxTimeoutSeconds: 120,
  extra,
} as const;
```

```ts
// app/api/data/route.ts
import { withCantonPayment } from "@ftptech/x402-canton-next";
import { price } from "@/lib/x402";

export const GET = withCantonPayment(
  async () => Response.json({ data: "paid" }),
  { accepts: [price], facilitatorUrl: "https://facilitator.ftptech.xyz" },
);
```

`curl -i http://localhost:3000/api/data` now answers 402. The current MainNet values are in [`/supported`](https://facilitator.ftptech.xyz/supported).

## Charging a token

Create the token's preapproval once (`preapproval --admin <registrar> --id USDCx`), then add an entry for it:

```ts
const USDCX = "decentralized-usdc-interchain-rep::12208115f1e168dd7e792320be9c4ca720c751a02a3053c7606e1c1cd3dad9bf60ef";

export const priceUSDCx = {
  ...price,
  asset: `${USDCX}::USDCx`,
  amount: "25000000", // 0.0025 USDCx, 10 decimals like CC
  extra: { ...extra, instrumentId: { admin: USDCX, id: "USDCx" } },
} as const;

// accepts: [price, priceUSDCx]
```

The FTP facilitator settles the tokens listed in [`/supported`](https://facilitator.ftptech.xyz/supported) under `extra.instruments`.

## Options

| Option | Notes |
| --- | --- |
| `accepts` | Ways to pay for this route. |
| `facilitatorUrl` | `https://facilitator.ftptech.xyz` |
| `description`, `mimeType` | Optional, shown to the payer. |
| `redeemed` | Store of served payments. Default: in memory, per instance. |
| `fetch` | Defaults to `globalThis.fetch`. |

## Good to know

- **One payment, one response.** A replay gets 402 `payment_already_redeemed`. On serverless or several instances pass a shared `redeemed` store, or each instance serves the payment once:
  ```ts
  const redeemed = {
    claim: async (id: string) => (await redis.set(`x402:${id}`, "1", { NX: true, EX: 93600 })) === "OK",
    release: async (id: string) => { await redis.del(`x402:${id}`); },
  };
  ```
- **Header size.** The payment header is 8 to 10 KiB; every proxy must accept 16 KiB. nginx: `large_client_header_buffers 4 16k;`. Node: `--max-http-header-size=32768`.
- **Settlement runs before your handler.** If the handler fails afterwards, the payer has already paid.
- **502** means the facilitator could not be reached or did not confirm. The payer should not pay again.
- No `next` dependency: the wrapper uses the Web `Request` and `Response` types.

## Links

- [Merchant guide](https://x402-docs.ftptech.xyz/x402-merchant)
- [Facilitator `/supported`](https://facilitator.ftptech.xyz/supported)
- [Spec](https://github.com/x402-foundation/x402/blob/main/specs/schemes/exact/scheme_exact_canton.md)
