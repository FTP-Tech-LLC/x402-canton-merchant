# canton-x402-merchant

Resource-server middleware for the Canton x402 stack. Gate HTTP routes
behind a Canton payment that is verified and settled through an x402
facilitator, for Express and for the Next.js App Router.

## Packages

| Package | npm | Purpose |
| --- | --- | --- |
| `@ftptech/x402-canton-express` | [npm](https://www.npmjs.com/package/@ftptech/x402-canton-express) | Express middleware to gate routes behind Canton x402 payment via a facilitator. |
| `@ftptech/x402-canton-next` | [npm](https://www.npmjs.com/package/@ftptech/x402-canton-next) | Next.js App Router wrapper to gate Route Handlers behind Canton x402 payment via a facilitator. |

See each package README for usage:
[`packages/express`](packages/express/README.md),
[`packages/next`](packages/next/README.md).

## Install

```bash
# Express
npm i @ftptech/x402-canton-express

# Next.js
npm i @ftptech/x402-canton-next
```

Both depend on `@ftptech/x402-canton-core` from
[canton-x402-core](https://github.com/sunstrike228/canton-x402-core).

## Part of the canton-x402 suite

- [canton-x402-core](https://github.com/sunstrike228/canton-x402-core): shared types + ledger primitives.
- [canton-x402-merchant](https://github.com/sunstrike228/canton-x402-merchant) (this repo): Express and Next.js middleware to gate routes behind payment.
- [canton-x402-agent](https://github.com/sunstrike228/canton-x402-agent): payer-side client SDK, agent wallet CLI, and MCP server.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
