package bybit

import "futures-arbitrage-scanner/internal/broker"

// The V5 endpoints this package calls, each quoted from its page (read
// 2026-09-17, docs repository master branch).
//
// Bybit has no request WEIGHT: every request counts one against the IP's
// "600 requests within a 5-second window", so WeightIP is 1 throughout and
// broker.BybitRequestsPerMin carries the conservative per-minute translation.
// The per-UID, per-endpoint limits quoted beside each entry are a separate
// bucket this package does not meter (a named debt; volumes here are a few
// requests per open).
var (
	// GET /v5/market/time is broker.BybitTimePath, read by broker.Client's
	// own clock (bybit_sign.go), not from here.

	// POST /v5/order/create — linear 10/s per UID. The answer is
	// {orderId, orderLinkId} and nothing else.
	epOrderCreate = broker.Endpoint{Path: "/v5/order/create", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/order/create-order"}

	// POST /v5/order/cancel — linear 10/s per UID. "You can only cancel
	// unfilled or partially filled orders."
	epOrderCancel = broker.Endpoint{Path: "/v5/order/cancel", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/order/cancel-order"}

	// GET /v5/order/realtime — 50/s. Open orders plus "recent 500 closed
	// status (Cancelled, Filled) orders"; "After a server release or restart,
	// filled, cancelled, and rejected orders of Unified account should only be
	// queried through order history."
	epOrderRealtime = broker.Endpoint{Path: "/v5/order/realtime", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/order/open-order"}

	// GET /v5/order/history — 50/s. "As order creation/cancellation is
	// asynchronous, the data returned from this endpoint may delay." Fully
	// cancelled and rejected orders are kept 24 hours, other closed ones 7
	// days, and beyond that only orders with fills.
	epOrderHistory = broker.Endpoint{Path: "/v5/order/history", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/order/order-list"}

	// GET /v5/position/list — 50/s. "If symbol passed, it returns data
	// regardless of having position or not."
	epPositionList = broker.Endpoint{Path: "/v5/position/list", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/position"}

	// GET /v5/account/wallet-balance?accountType=UNIFIED — 50/s.
	epWalletBalance = broker.Endpoint{Path: "/v5/account/wallet-balance", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/account/wallet-balance"}

	// GET /v5/market/instruments-info — public.
	epInstruments = broker.Endpoint{Path: "/v5/market/instruments-info", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/market/instrument"}

	// GET /v5/market/orderbook — public; "spot: [1, 200]", "linear & inverse:
	// [1, 500]" (re-read 2026-09-17; an earlier note here said 1000).
	epOrderbook = broker.Endpoint{Path: "/v5/market/orderbook", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/market/orderbook"}

	// GET /v5/market/funding/history — public; limit [1, 200].
	epFundingHistory = broker.Endpoint{Path: "/v5/market/funding/history", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/market/history-fund-rate"}

	// GET /v5/execution/list — "Query users' execution records"; limit
	// [1, 100]. The only place a fill's fee and its currency are stated.
	epExecutionList = broker.Endpoint{Path: "/v5/execution/list", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/order/execution"}

	// GET /v5/account/transaction-log — UTA; "endTime - startTime <= 7 days";
	// limit [1, 50]. type=SETTLEMENT is "USDT Perp funding settlement".
	epTransactionLog = broker.Endpoint{Path: "/v5/account/transaction-log", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/account/transaction-log"}

	// GET /v5/account/fee-rate — this account's maker/taker rate per symbol.
	epFeeRate = broker.Endpoint{Path: "/v5/account/fee-rate", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/account/fee-rate"}

	// GET /v5/market/tickers — public; markPrice, fundingRate, nextFundingTime.
	epTickers = broker.Endpoint{Path: "/v5/market/tickers", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/market/tickers"}

	// GET /v5/market/risk-limit — public; "category=linear returns a data set
	// of 15 symbols in each response. Please use the cursor param".
	epRiskLimit = broker.Endpoint{Path: "/v5/market/risk-limit", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/market/risk-limit"}

	// GET /v5/user/query-api — 10/s. "Any permission can access this
	// endpoint." Not in the demo page's endpoint table, but the same page says
	// to call API Key Info on api-demo.bybit.com with the demo key.
	epQueryAPI = broker.Endpoint{Path: "/v5/user/query-api", WeightIP: 1,
		DocURL: "https://bybit-exchange.github.io/docs/v5/user/apikey-info"}
)

// The two categories this package sends: "Product type. spot, linear, inverse,
// option". inverse and option are never sent.
const (
	categoryLinear = "linear"
	categorySpot   = "spot"
)

// settleCoinUSDT is the only settle coin this package trades on linear, and the
// asset a linear fill's commission is taken in.
const settleCoinUSDT = "USDT"

// GET /v5/account/info — "marginMode": ISOLATED_MARGIN, REGULAR_MARGIN or
// PORTFOLIO_MARGIN (read 2026-09-17). Read by the cross-venue margin guard,
// because "All account wide fields are not applicable to isolated margin"
// (wallet-balance page): on an isolated account accountMMRate says nothing about
// the positions at risk. The per-UID limit for this endpoint was not read.
var epAccountInfo = broker.Endpoint{Path: "/v5/account/info", WeightIP: 1,
	DocURL: "https://bybit-exchange.github.io/docs/v5/account/account-info"}
