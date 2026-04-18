// Dice smoke test — core lifecycle + cap rejection.
//
//   go test -run TestSmoke -v
//
// Sub-tests:
//   1. WinAndLoss        — place a few bets, advance one block, verify each
//                          bet either won (payout > 0) or lost (payout = 0).
//   2. ChanceOutOfRange  — chance < 1% or > 98% → status=2.
//   3. Reject101PerBlock — 101st place_bet in one block → status=4.
//
// Writes smoke.res.txt with per-scenario results.

package tests

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/exohash-labs/exohash-devkit/chainsim"
)

const (
	smWasmPath      = "../dice.wasm"
	smHouseEdgeBp   = 100
	smStakeUusdc    = 1_000_000
	smBigHouseFunds = 10_000_000_000_000_000
)

// Bet mode 1 = "over": player wins if roll >= (10000 - effChance)
// Bet mode 2 = "under": player wins if roll < effChance
// threshold for mode 1 is the lower bound; for mode 2 is the cutoff.
// chance = 10000-threshold (mode 1) or threshold (mode 2)

// makeParams builds a place_bet param payload: [mode(1)][threshold(8)] = 9 bytes
// (chainsim prepends 20-byte sender automatically).
func makeParams(mode byte, threshold uint64) []byte {
	p := make([]byte, 9)
	p[0] = mode
	binary.LittleEndian.PutUint64(p[1:], threshold)
	return p
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
	if err := c.RegisterGame(1, wasm, "dice", smHouseEdgeBp); err != nil {
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

func TestSmoke(t *testing.T) {
	var report strings.Builder
	fmt.Fprintln(&report, "=== Dice — smoke test ===")
	fmt.Fprintln(&report)

	// -----------------------------------------------------------------
	// 1. WinAndLoss: place 5 bets at 50% chance, settle next block.
	//    Each bet must end as either a win (payout > 0) or a loss (payout = 0).
	// -----------------------------------------------------------------
	t.Run("WinAndLoss", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()

		ids := make([]uint64, 0, 5)
		for i := 0; i < 5; i++ {
			addr := fmt.Sprintf("p%d", i)
			c.Deposit(addr, 1_000_000_000)
			// mode=2 (under), threshold=5000 → 50% chance
			id, err := c.PlaceBet(addr, 1, 1, smStakeUusdc, makeParams(2, 5000))
			if err != nil {
				t.Fatalf("place_bet %d: %v", i, err)
			}
			ids = append(ids, id)
		}
		// Advance one block — block_update settles all pending.
		c.AdvanceBlock()

		wins, losses := 0, 0
		for _, id := range ids {
			b := c.GetBet(id)
			if b == nil || b.Status == chainsim.BetOpen {
				t.Errorf("bet %d not settled", id)
				continue
			}
			if b.Payout > 0 {
				wins++
			} else {
				losses++
			}
		}

		fmt.Fprintln(&report, "--- 1. WinAndLoss (5 bets, 50% chance) ---")
		fmt.Fprintf(&report, "  wins=%d losses=%d\n", wins, losses)
		if wins+losses == 5 {
			fmt.Fprintln(&report, "  ✓ all 5 bets settled")
		} else {
			fmt.Fprintf(&report, "  ✗ only %d/5 settled\n", wins+losses)
			t.Errorf("expected 5 settled, got %d", wins+losses)
		}
		fmt.Fprintln(&report)
	})

	// -----------------------------------------------------------------
	// 2. ChanceOutOfRange: chance below 1% or above 98% → status=2.
	// -----------------------------------------------------------------
	t.Run("ChanceOutOfRange", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()
		c.Deposit("alice", 1_000_000_000)

		// chance=99 (below min 100): mode=2, threshold=99
		_, errLow := c.PlaceBet("alice", 1, 1, smStakeUusdc, makeParams(2, 99))
		// chance=9900 (above max 9800): mode=2, threshold=9900
		_, errHigh := c.PlaceBet("alice", 1, 1, smStakeUusdc, makeParams(2, 9900))

		fmt.Fprintln(&report, "--- 2. ChanceOutOfRange ---")
		fmt.Fprintf(&report, "  chance=99: %v\n", errLow)
		fmt.Fprintf(&report, "  chance=9900: %v\n", errHigh)
		ok := errLow != nil && strings.Contains(errLow.Error(), "status=2") &&
			errHigh != nil && strings.Contains(errHigh.Error(), "status=2")
		if ok {
			fmt.Fprintln(&report, "  ✓ both rejected with status=2")
		} else {
			fmt.Fprintf(&report, "  ✗ expected both to fail with status=2\n")
			t.Errorf("expected both rejected, got low=%v high=%v", errLow, errHigh)
		}
		fmt.Fprintln(&report)
	})

	// -----------------------------------------------------------------
	// 3. Reject101PerBlock: 101st place_bet in one block returns status=4.
	// -----------------------------------------------------------------
	t.Run("Reject101PerBlock", func(t *testing.T) {
		c := smNewChain(t)
		defer c.Close()

		ok := 0
		var failErr error
		for i := 0; i < 101; i++ {
			addr := fmt.Sprintf("p101_%d", i)
			c.Deposit(addr, 1_000_000_000)
			_, err := c.PlaceBet(addr, 1, 1, smStakeUusdc, makeParams(2, 5000))
			if err != nil {
				failErr = err
				break
			}
			ok++
		}

		fmt.Fprintln(&report, "--- 3. Reject101PerBlock (per-block bet cap) ---")
		fmt.Fprintf(&report, "  succeeded=%d failure=%v\n", ok, failErr)
		if ok == 100 && failErr != nil && strings.Contains(failErr.Error(), "status=4") {
			fmt.Fprintln(&report, "  ✓ 100 placed; 101st rejected with status=4 (block full)")
		} else {
			fmt.Fprintf(&report, "  ✗ expected 100 + status=4\n")
			t.Errorf("expected 100 OK + status=4, got OK=%d err=%v", ok, failErr)
		}
		fmt.Fprintln(&report)
	})

	t.Log("\n" + report.String())
	if e := os.WriteFile("smoke.res.txt", []byte(report.String()), 0644); e != nil {
		t.Fatalf("write res: %v", e)
	}
}
