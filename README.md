# x402-canton-merchant

Charge per HTTP request in Canton Coin or a CIP-56 token. Middleware for Express and the Next.js App Router; FTP's facilitator settles the payments.

| Package | Install |
| --- | --- |
| [`@ftptech/x402-canton-express`](packages/express/README.md) | `npm i @ftptech/x402-canton-express` |
| [`@ftptech/x402-canton-next`](packages/next/README.md) | `npm i @ftptech/x402-canton-next` |

## Quickstart

1. Create a wallet and turn on receiving:
   ```bash
   npx @ftptech/canton-agent-wallet create --relay-url https://facilitator.ftptech.xyz
   npx @ftptech/canton-agent-wallet address       # your party id: payTo
   # send a few CC to it, then
   npx @ftptech/canton-agent-wallet claim
   npx @ftptech/canton-agent-wallet preapproval
   ```
2. Gate a route: copy the example from the [Express](packages/express/README.md#example) or [Next.js](packages/next/README.md#example) README and set `payTo` and `amount`.

Full guide: [x402-docs.ftptech.xyz/x402-merchant](https://x402-docs.ftptech.xyz/x402-merchant).

## Related

- [x402-canton-agent](https://github.com/FTP-Tech-LLC/x402-canton-agent): pay x402 endpoints (CLI, SDK, MCP)
- [x402-canton-core](https://github.com/FTP-Tech-LLC/x402-canton-core): shared types and ledger client
- [x402-canton-facilitator](https://github.com/FTP-Tech-LLC/x402-canton-facilitator): the facilitator
- [Spec](https://github.com/x402-foundation/x402/blob/main/specs/schemes/exact/scheme_exact_canton.md)

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
