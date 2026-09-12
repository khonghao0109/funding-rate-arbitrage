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
