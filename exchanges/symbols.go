package exchanges

// Symbol pairs the scanner's identifier for a market with the venue's own.
//
// Before step 1.4 every connector held its own translation table: a switch for
// Kraken, a map for Paradex, a string template inline for OKX and Gate, and
// symbol[:3] for Hyperliquid - which worked only because all four configured
// bases happened to be three characters long, and would have subscribed to
// "DOG" for DOGEUSDT. Those tables were the reason adding a pair meant editing
// Go in five places. They now come from config.yaml, and a connector receives
// the translation already done.
//
// Standard is what the rest of the system calls the market and what goes on the
// wire; Venue is what this venue calls it and what goes into the subscription.
type Symbol struct {
	Standard string
	Venue    string
}

// VenueSymbols is the subscription list.
func VenueSymbols(symbols []Symbol) []string {
	out := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		out = append(out, symbol.Venue)
	}
	return out
}

// StandardOf translates an identifier coming back from the venue. An unknown one
// yields "", so a connector can drop a message for a market it never subscribed
// to instead of inventing a symbol for it.
func StandardOf(symbols []Symbol, venueSymbol string) string {
	for _, symbol := range symbols {
		if symbol.Venue == venueSymbol {
			return symbol.Standard
		}
	}
	return ""
}

// ConnectFunc is what every venue connector looks like. It reconnects on its
// own and writes normalized public market data into the feeds it is given, and
// it returns when the feeds' context is cancelled.
//
// The source name is a parameter, not a literal inside the connector: which
// stream this is called is configuration. Hardcoding it meant two config entries
// naming the same connector both reported under one name, so the second one
// appeared in the dashboard's status list and never produced a price.
//
// Step 1.5 replaced the three channel parameters with Feeds. A connector's whole
// body is now a call to RunStream: dialling, backing off, keeping alive and
// stopping are shared, and only parsing is per venue.
type ConnectFunc func(source string, symbols []Symbol, f Feeds)
