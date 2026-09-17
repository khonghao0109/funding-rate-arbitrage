package broker

import "time"

// Bybit's two NON-PRODUCTION REST hosts, the only Bybit hosts this package will
// sign for (PLAN 4.5i). There is no flag to add a third.
//
//   - Testnet — a separate venue instance with its own accounts, its own
//     matching engine and its own (thin) books:
//     "REST API Base Endpoint: - Testnet: https://api-testnet.bybit.com"
//     https://bybit-exchange.github.io/docs/v5/guide
//   - Demo trading — "an independent account for demo trading only, and it
//     has its own user ID", created from the mainnet site; "When you create
//     the key from demo trading, please use above domain to connect":
//     "Rest API: https://api-demo.bybit.com"
//     https://bybit-exchange.github.io/docs/v5/demo
//     The same page lists Market "All | all endpoints" as available there, so
//     rules, books and the clock are read from the demo host itself and no
//     request of any kind needs the production host.
//
// Production — api.bybit.com, api.bytick.com and the regional hosts — is refused
// by construction and named in boundary_test.go's refusal list.
const (
	BybitTestnetHost = "api-testnet.bybit.com"
	BybitDemoHost    = "api-demo.bybit.com"

	BybitTestnetBaseURL = "https://" + BybitTestnetHost
	BybitDemoBaseURL    = "https://" + BybitDemoHost

	// BybitTimePath is GET /v5/market/time, the clock every signed request is
	// corrected by.
	BybitTimePath = "/v5/market/time"
)

// BybitRequestsPerMin is the budget this package spends per minute per Bybit
// client.
//
// Bybit's IP limit is stated per FIVE SECONDS — "You are allowed to send 600
// requests within a 5-second window per IP by default"
// (https://bybit-exchange.github.io/docs/v5/rate-limit, read 2026-09-17 from
// the docs repository's master branch)
// — and WeightBudget meters a one-minute window. 600 a MINUTE is the largest
// per-minute figure that cannot exceed 600 inside any 5-second slice of that
// minute, so it is the conservative translation, and it is a twelfth of the
// true ceiling on purpose: a diagnostic and a two-leg open spend a handful of
// requests, and over-throttling is the safe direction.
//
// The per-UID, per-endpoint limits (order/create, order/cancel, …) are a
// SEPARATE bucket this package does not meter, the same named debt as Binance's
// ORDERS limits in endpoints.go.
const BybitRequestsPerMin = 600

// BybitForbiddenCooldown is the "at least 10 minutes" a Bybit HTTP 403 orders.
const BybitForbiddenCooldown = 10 * time.Minute
