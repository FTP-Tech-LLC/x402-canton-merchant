// Package canton is the FTP layer of the Canton x402 merchant plugin for the
// official x402 Go SDK. Merchants import this package and call NewMiddleware.
//
// The official SDK alone can charge a payer without delivering, or deliver one
// payment twice. This package closes those gaps around the SDK middleware:
// settle retries, one delivery per payment, correct error codes for unknown
// outcomes, and a payer value for the handler. See OPERATIONS.md for the rules.
package canton
