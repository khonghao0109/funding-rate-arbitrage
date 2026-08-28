// Package notify delivers alerts to humans — Telegram first, Discord optional.
//
// Alerts are throttled per opportunity key so a persistent condition does not
// become a stream of identical messages.
//
// Nothing in this package may block the data path. A notification that fails to
// send is logged and dropped; it never stalls ingestion or execution.
//
// Introduced in: PLAN.md phase 3, step 3.4.
package notify
