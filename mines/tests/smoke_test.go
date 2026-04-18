// Mines smoke test — core lifecycle + cap rejections.
//
//   go test -run TestSmoke -v
//
// Six sub-tests:
//   1. Cashout              — reveal 2 safe tiles, manual cashout → win at table mult
//   2. MineHit              — reveal until mine → loss
//   3. AutoMaxReveals       — reveal 5 safe tiles → auto-cashout fires at max
//   4. TimeoutRefund        — no reveal for 40+ blocks → refund (payout = stake)
//   5. TimeoutCashout       — reveal 1 then idle 40+ blocks → auto-cashout at last mult
//   6. Reject101PerBlock    — 101st place_bet in one block → status=14
//   7. Reject1001Concurrent — 1001st concurrent active bet → status=13
//
// Writes smoke.res.txt with per-scenario results.

package tests

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/exohash-labs/exohash-devkit/chainsim"
)

const (
	smWasmPath      = "../mines.wasm"
	smHouseEdgeBp   = 100
	smStakeUusdc    = 1_000_000
	smBigHouseFunds = 10_000_000_000_000_000
	smBoardSize     = 25
)

// minesTickMult mirrors the WASM's getFairMultBP table:
//   for safe = boardSize - mines, the fair multiplier at k reveals is
//     prod_{i=1..k} (boardSize - i + 1) / (safe - i + 1)
// Edged with 1% house edge: edged = fair * (10000 - 100) / 10000.
func minesEdgedMultBp(mines, reveals int) uint64 {
	safe := smBoardSize - mines
	num := uint64(1)
	den := uint64(1)
	for k := 1; k <= reveals; k++ {
		num *= uint64(smBoardSize - k + 1)
		den *= uint64(safe - k + 1)
	}
	fair := num * 10000 / den
	return fair * (10000 - smHouseEdgeBp) / 10000
}

func smNewChain(t *testing.T) *chainsim.Chain {
	t.Helper()
	wasm, err := os.ReadFile(smWasmPath)
	if err != nil {
		t.Fatalf("read %s: %v", smWasmPath, err)
	}
	c := chainsim.NewWithParams(chainsim.DefaultParams(), 42)
	c.Deposit("house", smBigHouseFunds)
	brID, err := c.CreateBankroll("house", smBigHouseFunds, "Test", false)
	if err != nil {
		t.Fatalf("create bankroll: %v", err)
	}
	if err := c.RegisterGame(1, wasm, "mines", smHouseEdgeBp); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := c.AttachGame(brID, 1); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := c.InitGame(1, brID); err != nil {
		t.Fatalf("init: %v", err)
	}
	return c
}

func smPlace(t *testing.T, c *chainsim.Chain, addr string, mines byte) uint64 {
	t.Helper()
	c.Deposit(addr, 1_000_000_000)
	id, err := c.PlaceBet(addr, 1, 1, smStakeUusdc, []byte{mines})
	if err != nil {
		t.Fatalf("place_bet (%s, mines=%d): %v", addr, mines, err)
	}
	return id
}

// smRevealAndResolve calls bet_action(reveal, tile) then advances one block
// to let block_update resolve the RNG. Returns the bet's status after.
func smRevealAndResolve(t *testing.T, c *chainsim.Chain, addr string, betID uint64, tile byte) chainsim.BetStatus {
	t.Helper()
	if err := c.BetAction(addr, betID, []byte{1, tile}); err != nil {
		t.Fatalf("bet_action reveal tile=%d: %v", tile, err)
	}
	c.AdvanceBlock() // block_update resolves the reveal
	b := c.GetBet(betID)
	if b == nil {
		return chainsim.BetSettled
	}
	return b.Status
}

func TestSmoke(t *testing.T) {
	var report strings.Builder
	fmt.Fprintln(&report, "=== Mines — smoke test ===")
	fmt.Fprintln(&report)

	// -------------------------------------------------------------------------
	// 1. Cashout — reveal 2 safe tiles (mines=3), manual cashout
	// -------------------------------------------------------------------------
	t.Run("Cashout", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()
		const minesCount = 3
		id := smPlace(t, c, "alice", minesCount)

		revealed := 0
		nextTile := byte(0)
		// Reveal up to 2 safe tiles; if a mine hits, skip subtest as not-applicable.
		for revealed < 2 {
			st := smRevealAndResolve(t, c, "alice", id, nextTile)
			if st == chainsim.BetSettled {
				b := c.GetBet(id)
				fmt.Fprintln(&report, "--- 1. Cashout (mines=3, target reveals=2) ---")
				fmt.Fprintf(&report, "  ⚠ mine hit during setup at tile %d (payout=%d) — skip\n", nextTile, b.Payout)
				fmt.Fprintln(&report)
				t.Skip("mine hit during reveal phase — statistical")
				return
			}
			revealed++
			nextTile++
		}

		if err := c.BetAction("alice", id, []byte{2}); err != nil {
			t.Fatalf("bet_action cashout: %v", err)
		}
		bet := c.GetBet(id)
		expected := smStakeUusdc * minesEdgedMultBp(minesCount, revealed) / 10000
		fmt.Fprintln(&report, "--- 1. Cashout (mines=3, target reveals=2) ---")
		fmt.Fprintf(&report, "  reveals=%d expected_payout=%d actual=%d\n", revealed, expected, bet.Payout)
		if bet.Payout == expected && bet.Status == chainsim.BetSettled {
			fmt.Fprintln(&report, "  ✓ cashout paid at table multiplier")
		} else {
			fmt.Fprintf(&report, "  ✗ mismatch (status=%v)\n", bet.Status)
			t.Errorf("expected payout=%d status=settled, got payout=%d status=%v", expected, bet.Payout, bet.Status)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// 2. MineHit — reveal until mine (high mines count → likely to hit)
	// -------------------------------------------------------------------------
	t.Run("MineHit", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()
		const minesCount = 13 // 13/25 = 52% mine probability; will hit fast
		id := smPlace(t, c, "bob", minesCount)

		var hitMine bool
		var revealedSafely int
		for tile := byte(0); tile < smBoardSize; tile++ {
			st := smRevealAndResolve(t, c, "bob", id, tile)
			if st == chainsim.BetSettled {
				b := c.GetBet(id)
				if b == nil || b.Payout == 0 {
					hitMine = true
				}
				break
			}
			revealedSafely++
			if revealedSafely >= 5 {
				// Auto-cashout would fire at 5 — wasn't a mine hit.
				break
			}
		}

		bet := c.GetBet(id)
		fmt.Fprintln(&report, "--- 2. MineHit (mines=13) ---")
		fmt.Fprintf(&report, "  reveals_before_outcome=%d hit_mine=%v final_payout=%d status=%v\n",
			revealedSafely, hitMine, bet.Payout, bet.Status)
		if hitMine && bet.Payout == 0 {
			fmt.Fprintln(&report, "  ✓ mine hit → loss with payout=0")
		} else if !hitMine && revealedSafely >= 5 {
			fmt.Fprintln(&report, "  ⚠ no mine within 5 reveals (auto-cashout fired)")
			t.Skip("no mine hit; skipping (statistical)")
		} else {
			fmt.Fprintf(&report, "  ✗ unexpected outcome\n")
			t.Errorf("expected mine hit, got hit=%v payout=%d", hitMine, bet.Payout)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// 3. AutoMaxReveals — mines=1, reveal 5 → auto-cashout fires
	// -------------------------------------------------------------------------
	t.Run("AutoMaxReveals", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()
		const minesCount = 1
		id := smPlace(t, c, "carol", minesCount)

		var hit bool
		for tile := byte(0); tile < 5; tile++ {
			st := smRevealAndResolve(t, c, "carol", id, tile)
			if st == chainsim.BetSettled {
				b := c.GetBet(id)
				if b == nil || b.Payout == 0 {
					hit = true
					break
				}
				// Auto-cashout fired — break.
				break
			}
		}

		bet := c.GetBet(id)
		expected := smStakeUusdc * minesEdgedMultBp(minesCount, 5) / 10000
		fmt.Fprintln(&report, "--- 3. AutoMaxReveals (mines=1, reveals=5) ---")
		fmt.Fprintf(&report, "  expected_payout=%d actual=%d status=%v\n", expected, bet.Payout, bet.Status)
		if hit {
			fmt.Fprintln(&report, "  ⚠ mine hit during 5 reveals (4% chance) — skip")
			t.Skip("mine hit during 5 reveals (statistical)")
		} else if bet.Payout == expected && bet.Status == chainsim.BetSettled {
			fmt.Fprintln(&report, "  ✓ auto-cashout fired at max reveals (5) at table multiplier")
		} else {
			fmt.Fprintf(&report, "  ✗ mismatch\n")
			t.Errorf("expected payout=%d status=settled, got payout=%d status=%v", expected, bet.Payout, bet.Status)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// 4. TimeoutRefund — no reveal for 40+ blocks → refund
	// -------------------------------------------------------------------------
	t.Run("TimeoutRefund", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()
		id := smPlace(t, c, "dave", 5)

		// Advance 45 blocks (timeout = 40); never call bet_action.
		for i := 0; i < 45; i++ {
			c.AdvanceBlock()
			b := c.GetBet(id)
			if b != nil && b.Status != chainsim.BetOpen {
				break
			}
		}

		bet := c.GetBet(id)
		fmt.Fprintln(&report, "--- 4. TimeoutRefund (no reveal, wait 40+ blocks) ---")
		fmt.Fprintf(&report, "  status=%v payout=%d (stake=%d)\n", bet.Status, bet.Payout, smStakeUusdc)
		if bet.Status == chainsim.BetRefunded && bet.Payout == smStakeUusdc {
			fmt.Fprintln(&report, "  ✓ refunded at stake")
		} else {
			fmt.Fprintf(&report, "  ✗ expected refund of stake\n")
			t.Errorf("expected refund=%d, got status=%v payout=%d", smStakeUusdc, bet.Status, bet.Payout)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// 5. TimeoutCashout — reveal 1 safe tile, then idle 40+ blocks → auto-cashout
	//    at the multiplier of the last safe reveal (mines=1, reveals=1 → 1.031x).
	// -------------------------------------------------------------------------
	t.Run("TimeoutCashout", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()
		const minesCount = 1
		id := smPlace(t, c, "eve", minesCount)

		// Reveal 1 tile (very likely safe with mines=1: 96% probability).
		st := smRevealAndResolve(t, c, "eve", id, 0)
		if st == chainsim.BetSettled {
			b := c.GetBet(id)
			fmt.Fprintln(&report, "--- 5. TimeoutCashout (reveal 1, idle 40+ blocks) ---")
			fmt.Fprintf(&report, "  ⚠ mine hit on first reveal (4%% chance) — skip (payout=%d)\n", b.Payout)
			fmt.Fprintln(&report)
			t.Skip("mine hit on first reveal")
			return
		}

		// Idle 45 blocks; never call bet_action again. Timeout fires at +40.
		for i := 0; i < 45; i++ {
			c.AdvanceBlock()
			b := c.GetBet(id)
			if b != nil && b.Status != chainsim.BetOpen {
				break
			}
		}

		bet := c.GetBet(id)
		expected := smStakeUusdc * minesEdgedMultBp(minesCount, 1) / 10000
		fmt.Fprintln(&report, "--- 5. TimeoutCashout (reveal 1, idle 40+ blocks) ---")
		fmt.Fprintf(&report, "  expected_payout=%d actual=%d status=%v\n", expected, bet.Payout, bet.Status)
		if bet.Status == chainsim.BetSettled && bet.Payout == expected {
			fmt.Fprintln(&report, "  ✓ auto-cashout fired at timeout, paid at last revealed mult")
		} else {
			fmt.Fprintf(&report, "  ✗ mismatch\n")
			t.Errorf("expected status=settled payout=%d, got status=%v payout=%d", expected, bet.Status, bet.Payout)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// 6. Reject101PerBlock — 101st place_bet in one block returns status=14
	// -------------------------------------------------------------------------
	t.Run("Reject101PerBlock", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()

		ok := 0
		var failErr error
		for i := 0; i < 101; i++ {
			addr := fmt.Sprintf("p101_%d", i)
			c.Deposit(addr, 1_000_000_000)
			_, err := c.PlaceBet(addr, 1, 1, smStakeUusdc, []byte{1})
			if err != nil {
				failErr = err
				break
			}
			ok++
		}

		fmt.Fprintln(&report, "--- 5. Reject101PerBlock (per-block bet cap) ---")
		fmt.Fprintf(&report, "  succeeded=%d failure=%v\n", ok, failErr)
		if ok == 100 && failErr != nil && strings.Contains(failErr.Error(), "status=14") {
			fmt.Fprintln(&report, "  ✓ 100 placed; 101st rejected with status=14 (block full)")
		} else {
			fmt.Fprintf(&report, "  ✗ expected 100 + status=14\n")
			t.Errorf("expected 100 OK + status=14, got OK=%d err=%v", ok, failErr)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// 6. Reject1001Concurrent — 1001st concurrent active bet → status=13
	// -------------------------------------------------------------------------
	t.Run("Reject1001Concurrent", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()

		// Adaptive placement: try place_bet greedily; on any error in setup,
		// AdvanceBlock to reset per-block budget and continue. Stop when
		// we've reached 1000 active bets OR safety cap.
		placed := 0
		const safetyBlocks = 50
		for blockIdx := 0; blockIdx < safetyBlocks && placed < 1000; blockIdx++ {
			for i := 0; placed < 1000; i++ {
				addr := fmt.Sprintf("c1k_%d_%d", blockIdx, i)
				c.Deposit(addr, 1_000_000_000)
				_, err := c.PlaceBet(addr, 1, 1, smStakeUusdc, []byte{1})
				if err != nil {
					// Expected mid-setup: per-block gas exceeded OR per-block
					// bet cap (status=14). Both indicate "block full, try
					// next block". Advance and retry.
					if strings.Contains(err.Error(), "per-block") || strings.Contains(err.Error(), "status=14") {
						break
					}
					t.Fatalf("setup place_bet unexpected error block=%d i=%d: %v", blockIdx, i, err)
				}
				placed++
			}
			c.AdvanceBlock()
		}
		if placed != 1000 {
			t.Fatalf("setup: only reached %d/1000 active bets within %d blocks", placed, safetyBlocks)
		}

		// Try to place 1001st in current (next) block.
		c.Deposit("intruder", 1_000_000_000)
		_, err := c.PlaceBet("intruder", 1, 1, smStakeUusdc, []byte{1})

		fmt.Fprintln(&report, "--- 6. Reject1001Concurrent (concurrent active cap) ---")
		fmt.Fprintf(&report, "  placed=%d failure=%v\n", placed, err)
		if placed == 1000 && err != nil && strings.Contains(err.Error(), "status=13") {
			fmt.Fprintln(&report, "  ✓ 1000 active; 1001st rejected with status=13 (game full)")
		} else {
			fmt.Fprintf(&report, "  ✗ expected 1000 + status=13\n")
			t.Errorf("expected 1000 placed + status=13, got placed=%d err=%v", placed, err)
		}
		fmt.Fprintln(&report)
	})

	// Write report.
	t.Log("\n" + report.String())
	if e := os.WriteFile("smoke.res.txt", []byte(report.String()), 0644); e != nil {
		t.Fatalf("write res: %v", e)
	}
}
