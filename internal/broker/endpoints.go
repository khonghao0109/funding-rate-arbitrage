package broker

// Endpoint is one documented REST endpoint: its path, the request weight the
// venue charges for it, and the page that says so.
//
// Weight travels WITH the path rather than as a separate argument for one
// reason: the limiter can only block before a call if it knows what the call
// costs, and a bare int at the call site is a number someone will one day
// forget to update when the venue re-prices the endpoint. Here the citation
// sits beside the number, so re-checking it is reading one struct.
type Endpoint struct {
	Path     string
	WeightIP int
	DocURL   string
}

// The endpoints step 4.1 needs, and no others. There is deliberately no order
// endpoint here: 4.1 is the transport, 4.2 is the order interface.
var (
	// GET /fapi/v1/time — "Request Weight: 1", {"serverTime": …}
	FuturesServerTime = Endpoint{
		Path: BinanceFuturesTimePath, WeightIP: BinanceTimeWeight,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Check-Server-Time",
	}

	// GET /api/v3/time — "IP Weight 1", {"serverTime": …}
	SpotServerTime = Endpoint{
		Path: BinanceSpotTimePath, WeightIP: BinanceTimeWeight,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/general-endpoints",
	}

	// GET /fapi/v3/balance — USER_DATA, weight 5. Answers an array of
	// {accountAlias, asset, balance, crossWalletBalance, crossUnPnl,
	// availableBalance, maxWithdrawAmount, marginAvailable, updateTime}.
	FuturesAccountBalance = Endpoint{
		Path: "/fapi/v3/balance", WeightIP: 5,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Futures-Account-Balance-V3",
	}

	// GET /api/v3/account — USER_DATA, IP weight 20. Answers an object whose
	// `balances` array holds {asset, free, locked}.
	SpotAccount = Endpoint{
		Path: "/api/v3/account", WeightIP: 20,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/account-endpoints",
	}
)

// The step-4.2 order endpoints.
//
// READ THE PROVENANCE OF EACH WEIGHT. Four were quoted from the venue's own
// pages on 2026-09-13; three could NOT be, and carry a conservative
// over-estimate with the label on it (CLAUDE.md rule 5). Over-stating a weight
// only throttles this client harder, which is the safe direction; under-stating
// one is a 429 and then an automatic IP ban.
var (
	// POST /fapi/v1/order — "1 on 10s order rate limit(X-MBX-ORDER-COUNT-10S);
	// 1 on 1min order rate limit(X-MBX-ORDER-COUNT-1M); 0 on IP rate
	// limit(x-mbx-used-weight-1m)". Parameters travel in the REQUEST BODY.
	//
	// WeightIP is therefore 0, and that is the documented figure, not an
	// omission. The ORDER-COUNT limits are a SEPARATE bucket that this package
	// does not yet meter — measured on testnet the same day, they are 1200/min
	// and 300/10s on futures, 50/10s and 160000/day on spot. Step 4.2 places
	// two orders and cannot approach them; metering them belongs with 4.4,
	// where order volume becomes real. Recorded rather than silently ignored.
	// https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/New-Order
	FuturesNewOrder = Endpoint{
		Path: "/fapi/v1/order", WeightIP: 0,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/New-Order",
	}

	// GET /fapi/v1/order — "IP Weight 1". "Either orderId or
	// origClientOrderId must be sent, and the orderId will prevail if both
	// are sent."
	FuturesQueryOrder = Endpoint{
		Path: "/fapi/v1/order", WeightIP: 1,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/Query-Order",
	}

	// DELETE /fapi/v1/order — "IP Weight 1". "Either orderId or
	// origClientOrderId must be sent."
	FuturesCancelOrder = Endpoint{
		Path: "/fapi/v1/order", WeightIP: 1,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/Query-Order",
	}

	// GET /fapi/v1/openOrders — **WEIGHT UNVERIFIED**. The Current-All-Open-
	// Orders page was not reachable from this environment on 2026-09-13 (the
	// endpoint is listed in the navigation, the weight line did not render).
	// 40 is charged here: it is the widely reported cost of the NO-SYMBOL
	// form, so charging it unconditionally over-states the with-symbol call
	// rather than under-stating either. Replace with the quoted figure and
	// the URL when the page can be read.
	FuturesOpenOrders = Endpoint{
		Path: "/fapi/v1/openOrders", WeightIP: 40,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api",
	}

	// GET /fapi/v3/account — "IP Weight 5". Its `positions` array is how a
	// position is read; see binance.parsePositions for what that array does
	// and does NOT publish.
	FuturesAccount = Endpoint{
		Path: "/fapi/v3/account", WeightIP: 5,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Account-Information-V3",
	}

	// GET /fapi/v1/userTrades — USER_DATA, "IP Weight5", read 2026-09-13.
	// Parameters: symbol (required), timestamp (required), orderId, startTime,
	// endTime, fromId, limit (default 500, max 1000), recvWindow (max 60000).
	// Answers an array of {buyer, commission, commissionAsset, id, maker,
	// orderId, price, qty, quoteQty, baseQty, marginAsset, realizedPnl, side,
	// positionSide, symbol, pair, time}.
	//
	// This is the ONLY place USDⓈ-M states a commission: the order answer does
	// not carry one, which is why step 4.2's LegResult.FeeQuote was 0 on every
	// path and said so.
	FuturesUserTrades = Endpoint{
		Path: "/fapi/v1/userTrades", WeightIP: 5,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/Account-Trade-List",
	}

	// GET /api/v3/myTrades — USER_DATA, "IP Weight: Without orderId: 20; With
	// orderId: 5", read 2026-09-13. Answers an array of {symbol, id, orderId,
	// orderListId, price, qty, quoteQty, commission, commissionAsset, time,
	// isBuyer, isMaker, isBestMatch}.
	//
	// 20 is charged here, not 5, because the budget reserves BEFORE the call
	// and this endpoint's weight depends on a parameter: over-charging the
	// with-orderId form wastes budget, under-charging the other earns a 429,
	// and only one of those two mistakes costs an IP ban.
	SpotMyTrades = Endpoint{
		Path: "/api/v3/myTrades", WeightIP: 20,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/account-endpoints",
	}

	// GET /fapi/v1/income — USER_DATA, "Request Weight: 30" (IP), read
	// 2026-09-13. Parameters: timestamp (required), symbol, incomeType,
	// startTime, endTime, page, limit (default 100, max 1000), recvWindow.
	// incomeType values include TRANSFER, WELCOME_BONUS, REALIZED_PNL,
	// FUNDING_FEE, COMMISSION, INSURANCE_CLEAR, REFERRAL_KICKBACK,
	// COMMISSION_REBATE. Answers {symbol, incomeType, income, asset, info,
	// time, tranId, tradeId}.
	//
	// FUNDING_FEE is what CLAUDE.md rule 6 means by counting settlements: each
	// row IS one settlement that was actually paid or received, so nothing
	// multiplies a rate by a holding time.
	FuturesIncome = Endpoint{
		Path: "/fapi/v1/income", WeightIP: 30,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Get-Income-History",
	}

	// GET /fapi/v1/premiumIndex — mark price and funding rate. Weight 1 for a
	// single symbol. Carries `nextFundingTime` (ms) and `lastFundingRate`.
	// exchanges/binance/funding_rest.go polls the same endpoint on MAINNET for
	// public data; this is the TESTNET one, and it is here because 4.5 has to
	// know when the next settlement is before it opens a position.
	FuturesPremiumIndex = Endpoint{
		Path: "/fapi/v1/premiumIndex", WeightIP: 1,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Mark-Price",
	}

	// POST /api/v3/order — "IP Weight 1", parameters in the REQUEST BODY.
	SpotNewOrder = Endpoint{
		Path: "/api/v3/order", WeightIP: 1,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/trading-endpoints",
	}

	// DELETE /api/v3/order — "IP Weight 1", parameters in the QUERY STRING.
	SpotCancelOrder = Endpoint{
		Path: "/api/v3/order", WeightIP: 1,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/trading-endpoints",
	}

	// GET /api/v3/order — **WEIGHT UNVERIFIED**. The weight line for this
	// endpoint did not render from this environment on 2026-09-13, while the
	// POST and DELETE on the same page did ("IP Weight1" each). 4 is charged:
	// higher than the 1 its neighbours cost and at least as high as any figure
	// reported for it.
	SpotQueryOrder = Endpoint{
		Path: "/api/v3/order", WeightIP: 4,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/trading-endpoints",
	}

	// GET /api/v3/openOrders — **WEIGHT UNVERIFIED**, same reason. 80 is
	// charged, the widely reported no-symbol cost, so the with-symbol call is
	// over-charged rather than either being under-charged.
	SpotOpenOrders = Endpoint{
		Path: "/api/v3/openOrders", WeightIP: 80,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/trading-endpoints",
	}
)

// Public endpoints step 4.2's acceptance needs: the market's own rules, and a
// price to place a resting order far away from.
var (
	// GET /fapi/v1/exchangeInfo — the rateLimits array this project reads its
	// weight ceiling from, and the symbol filters an order is rounded by.
	// Weight is not stated on the page; 1 is charged, and the call is made
	// once per run.
	FuturesExchangeInfo = Endpoint{
		Path: "/fapi/v1/exchangeInfo", WeightIP: 1,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/general-info",
	}

	// GET /api/v3/exchangeInfo — **WEIGHT UNVERIFIED** (the page did not
	// render this line from here on 2026-09-13). 20 is charged, comfortably
	// above any reported figure, and the call is made once per run.
	SpotExchangeInfo = Endpoint{
		Path: "/api/v3/exchangeInfo", WeightIP: 20,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/general-endpoints",
	}

	// GET /api/v3/ticker/price — "Parameter Symbols Provided Weight symbol 1
	// 2 omitted 4": weight 2 with a symbol. Answers {"symbol","price"}.
	SpotTickerPrice = Endpoint{
		Path: "/api/v3/ticker/price", WeightIP: 2,
		DocURL: "https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints",
	}

	// GET /fapi/v1/ticker/price — **WEIGHT UNVERIFIED**: the Symbol Price
	// Ticker page was not reachable from here on 2026-09-13. 2 is charged, the
	// same as spot's with-symbol figure and at or above any reported value.
	FuturesTickerPrice = Endpoint{
		Path: "/fapi/v1/ticker/price", WeightIP: 2,
		DocURL: "https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api",
	}
)
