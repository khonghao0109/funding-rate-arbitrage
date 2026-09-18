// Engine 1's side of the exclusive symbol lock (PLAN 4.5k step 4, item 1).
//
// # What this adds, and what it cannot add
//
// Strategy 1 holds spot long + perp short on ONE venue. Engine 2 holds a perp on
// each of two venues. Where those two overlap — the perp on the shared venue —
// they are on the SAME one-way account, so a long from Engine 2 nets against
// Engine 1's short and both engines silently lose their hedge (design §2.1).
// The lock is what stops that, and it only works if BOTH engines ask. This file
// is Engine 1 asking.
//
// Two limits, stated rather than hidden:
//
//   - The lock exists only when Engine 2's desk does, which means only under
//     -crossperp. A portal started without it opens exactly as it always did,
//     asks nobody, and is protected by nothing. That is a deployment
//     precondition: do not run an unlocked Engine-1 portal beside a locked one
//     on the same account.
//   - A portal ALREADY RUNNING when this code shipped is running the old binary
//     and asks nobody either. Nothing here can reach into another process; what
//     protects that case is the coordinator READING its positions during
//     reconcile and refusing to hand Engine 2 a symbol the venues say is busy.
//     That is a weaker guarantee — it closes the window to the length of one
//     reconcile-to-send gap rather than to zero — and it is why item 4 of the
//     acceptance is run on a symbol no other process holds.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"futures-arbitrage-scanner/internal/coordinator"
)

// engine1Settle is what an open must call when it knows what the venues hold.
// It is never nil, so the caller needs no branch.
type engine1Settle func(ctx context.Context, stillHolding bool)

// acquireEngine1 takes the symbol's lock for Engine 1 and marks it durably as
// about to send, in that order, BEFORE any order leaves.
//
// It returns a refusal STRING rather than an error because every caller does the
// same thing with it — puts it in front of the operator and sends nothing — and
// because the coordinator's refusals are already written in the operator's
// language.
//
// When Engine 2 is not wired there is no lock to take: the settle is a no-op and
// the refusal is empty. That is not "permission granted", it is "nobody is
// keeping score", and the page says so through crossOffVI.
func (p *portal) acquireEngine1(ctx context.Context, symbol, intentID, detailsVI string) (engine1Settle, string) {
	noop := func(context.Context, bool) {}
	if p.cross == nil {
		return noop, ""
	}
	coord := p.cross.coord

	// Engine 1's expected figure is not computed here and must not be: the
	// coordinator ranks contenders on a number the caller states together with
	// what it has taken off, and this open is a person pressing a button. A
	// button press therefore competes at zero and says so — it never outranks a
	// measured signal by accident.
	dec, err := coord.TryAcquire(ctx, coordinator.AcquireRequest{
		Symbol: symbol, Engine: coordinator.EngineCashAndCarry, IntentID: intentID,
		Details:                  detailsVI,
		Venues:                   []string{crossVenueBinance},
		PriorityAPROnCapitalFrac: 0,
		PriorityAPRBasisVI:       "người vận hành bấm nút trên portal — KHÔNG có con số kỳ vọng nào được tính, nên ý định này tranh chấp ở mức 0",
	})
	if err != nil {
		return noop, engine1RefusalVI(symbol, dec, err)
	}

	// The mark is written to the lock file before the first order, so a process
	// that dies mid-open leaves behind a lock nobody can hand back on its word
	// alone — only a read of the venues can release it.
	if err := coord.MarkOrdersSent(symbol, coordinator.EngineCashAndCarry, intentID); err != nil {
		if wErr := coord.Withdraw(symbol, coordinator.EngineCashAndCarry, intentID); wErr != nil {
			log.Printf("execportal: %s — không ghi bền được dấu 'sắp gửi lệnh' (%v) VÀ không trả lại được khóa (%v); khóa được GIỮ", symbol, err, wErr)
		}
		return noop, fmt.Sprintf("không ghi bền được dấu 'sắp gửi lệnh' vào bảng khóa cho %s: %v — không gửi lệnh nào trên một khóa chưa ghi được", symbol, err)
	}

	return func(settleCtx context.Context, stillHolding bool) {
		if stillHolding {
			// The venues hold something, or an order that may still fill stands
			// beside them. The lock is Engine 1's until a close proves flat.
			return
		}
		// Nothing was opened. Release still READS both venues — it is the only
		// way back to idle once orders were marked as sent — so a release that
		// cannot prove flat leaves the lock held, which is the right answer.
		if _, err := coord.Release(context.WithoutCancel(settleCtx), symbol, coordinator.EngineCashAndCarry, intentID); err != nil {
			log.Printf("execportal: %s — mở không thành nhưng KHÔNG nhả được khóa: %v; khóa được GIỮ cho tới khi chứng minh được phẳng", symbol, err)
		}
	}, ""
}

// releaseEngine1 gives the symbol back after a close that PROVED both legs flat
// by Engine 1's own machine. The coordinator then proves the perp leg flat again
// on every venue it knows before it writes idle, so the release rests on two
// readings and not on this process's belief (rule 7, and decision Q22).
func (p *portal) releaseEngine1(ctx context.Context, symbol, intentID string) string {
	if p.cross == nil {
		return ""
	}
	report, err := p.cross.coord.Release(context.WithoutCancel(ctx), symbol, coordinator.EngineCashAndCarry, intentID)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, coordinator.ErrNotLocked), errors.Is(err, coordinator.ErrNotOwner):
		// Engine 1 never held this symbol's lock under this intent — a portal
		// started without -crossperp opened it, or a reconcile gave it to
		// somebody else. Nothing to give back, and nothing to hide either.
		return ""
	default:
		log.Printf("execportal: %s — đóng phẳng nhưng KHÔNG nhả được khóa: %v", symbol, err)
		return fmt.Sprintf("hai chân đã phẳng nhưng KHÓA CẶP chưa nhả được: %v — %s", err, report.EvidenceVI)
	}
}

func engine1RefusalVI(symbol string, dec coordinator.Decision, err error) string {
	switch {
	case errors.Is(err, coordinator.ErrNotReconciled):
		return fmt.Sprintf("BỘ KHÓA CẶP chưa đối soát %s với sàn — không cấp khóa trên trạng thái chưa đọc; bấm ĐỐI SOÁT LẠI ở tab Động cơ 2", symbol)
	case errors.Is(err, coordinator.ErrOccupied):
		return fmt.Sprintf("BỘ KHÓA CẶP TỪ CHỐI: %s đang BẬN — %s", symbol, dec.ReasonVI)
	case errors.Is(err, coordinator.ErrConflict):
		return fmt.Sprintf("BỘ KHÓA CẶP TỪ CHỐI: %s ở trạng thái XUNG ĐỘT BẰNG CHỨNG — %s", symbol, dec.ReasonVI)
	case errors.Is(err, coordinator.ErrLostContest):
		return fmt.Sprintf("BỘ KHÓA CẶP TỪ CHỐI: một ý định khác thắng tranh chấp %s — %s", symbol, dec.ReasonVI)
	default:
		return fmt.Sprintf("BỘ KHÓA CẶP TỪ CHỐI %s: %v", symbol, err)
	}
}
