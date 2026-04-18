// Four scenarios in one round each:
//
//   1.  AutoTick1   — 100 bets autocashout=1, verify all settle at tick 1 mult
//   2.  ManualTick1 — 100 bets autocashout=134, all manually cashout before
//                     first tick fires, verify all settle at tick 1 mult
//   2b. ManualTick5 — 100 bets autocashout=134, all manually cashout when
//                     current_tick=4, verify all settle at tick 5 mult
//   3.  Revert101   — 101 bets in one OPEN block, 101st must be rejected
//                     with status=14 (round full)
//
//   go test -run TestFourScenarios -v
//
// Writes four_scenarios.res.txt.

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
	tsWasmPath      = "../crash.wasm"
	tsHouseEdgeBp   = 100
	tsStakeUusdc    = 1_000_000
	tsBigHouseFunds = 10_000_000_000_000_000
	tsTickMult1Bp   = uint64(10350) // tick 1 multiplier (1.035x)
)

func tsNewChain(t *testing.T) *chainsim.Chain {
	t.Helper()
	wasm, err := os.ReadFile(tsWasmPath)
	if err != nil {
		t.Fatalf("read %s: %v", tsWasmPath, err)
	}
	c := chainsim.NewWithParams(chainsim.DefaultParams(), 42)
	c.Deposit("house", tsBigHouseFunds)
	brID, err := c.CreateBankroll("house", tsBigHouseFunds, "Test", false)
	if err != nil {
		t.Fatalf("create bankroll: %v", err)
	}
	if err := c.RegisterGame(1, wasm, "crash", tsHouseEdgeBp); err != nil {
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

func tsPhase(c *chainsim.Chain) string {
	raw, _ := c.GameQuery(1)
	if raw == nil {
		return "?"
	}
	s := string(raw)
	i := strings.Index(s, `"phase":"`)
	if i < 0 {
		return "?"
	}
	rest := s[i+9:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return "?"
	}
	return rest[:j]
}

func tsAdvanceTo(c *chainsim.Chain, want string, max int) error {
	for i := 0; i < max; i++ {
		if tsPhase(c) == want {
			return nil
		}
		c.AdvanceBlock()
	}
	return fmt.Errorf("never reached phase %q within %d blocks (last %q)", want, max, tsPhase(c))
}

func TestFourScenarios(t *testing.T) {
	var report strings.Builder
	fmt.Fprintln(&report, "=== Crash — four scenarios ===")
	fmt.Fprintln(&report)

	// -------------------------------------------------------------------------
	// Scenario 1: 100 bets autocashout=tick 1
	// -------------------------------------------------------------------------
	t.Run("AutoTick1", func(t *testing.T) {
		c := tsNewChain(t)
		defer c.Close()
		if err := tsAdvanceTo(c, "open", 40); err != nil {
			t.Fatal(err)
		}
		params := make([]byte, 8)
		binary.LittleEndian.PutUint64(params, 1)

		ids := make([]uint64, 0, 100)
		for i := 0; i < 100; i++ {
			addr := fmt.Sprintf("a_%d", i)
			c.Deposit(addr, 1_000_000_000)
			id, err := c.PlaceBet(addr, 1, 1, tsStakeUusdc, params)
			if err != nil {
				t.Fatalf("place_bet %d: %v", i, err)
			}
			ids = append(ids, id)
		}

		// Drive round to completion.
		for step := 0; step < 200; step++ {
			c.AdvanceBlock()
			settled := 0
			for _, id := range ids {
				b := c.GetBet(id)
				if b != nil && b.Status != chainsim.BetOpen {
					settled++
				}
			}
			if settled == 100 {
				break
			}
		}

		wins, losses, payouts := 0, 0, uint64(0)
		expectedPayout := tsStakeUusdc * tsTickMult1Bp / 10000
		mismatched := 0
		for _, id := range ids {
			b := c.GetBet(id)
			if b == nil {
				continue
			}
			payouts += b.Payout
			if b.Payout > 0 {
				wins++
				if b.Payout != expectedPayout {
					mismatched++
				}
			} else {
				losses++
			}
		}

		fmt.Fprintln(&report, "--- 1. AutoTick1: 100 bets autocashout=tick 1 ---")
		fmt.Fprintf(&report, "  wins=%d losses=%d total_payout=%d uusdc\n", wins, losses, payouts)
		fmt.Fprintf(&report, "  expected payout per win: %d uusdc (1.035x stake)\n", expectedPayout)
		if wins == 100 && mismatched == 0 {
			fmt.Fprintln(&report, "  ✓ all 100 won at tick 1 mult")
		} else if losses == 100 {
			fmt.Fprintln(&report, "  ⚠ tick 1 crashed this seed — all 100 lost (rerun: ~96% survive)")
			t.Skip("tick 1 crashed; statistical scenario, not a failure")
		} else {
			fmt.Fprintf(&report, "  ✗ unexpected: wins=%d losses=%d mismatched=%d\n", wins, losses, mismatched)
			t.Errorf("expected all-win or all-loss; got wins=%d losses=%d mismatched=%d", wins, losses, mismatched)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// Scenario 2: 100 bets autocashout=tick 134, all manually cashout at tick 1
	// -------------------------------------------------------------------------
	t.Run("ManualTick1", func(t *testing.T) {
		c := tsNewChain(t)
		defer c.Close()
		if err := tsAdvanceTo(c, "open", 40); err != nil {
			t.Fatal(err)
		}

		// Place 100 bets with default autocashout (= tick 134).
		ids := make([]uint64, 0, 100)
		addrs := make([]string, 0, 100)
		for i := 0; i < 100; i++ {
			addr := fmt.Sprintf("m_%d", i)
			addrs = append(addrs, addr)
			c.Deposit(addr, 1_000_000_000)
			id, err := c.PlaceBet(addr, 1, 1, tsStakeUusdc, nil)
			if err != nil {
				t.Fatalf("place_bet %d: %v", i, err)
			}
			ids = append(ids, id)
		}

		// Wait until phase=tick. This is the moment the OPEN window expired
		// and the next block_update will run the tick 1 crash check.
		if err := tsAdvanceTo(c, "tick", 40); err != nil {
			t.Fatal(err)
		}

		// Send bet_action for all 100 — they queue in keyCashout. Next
		// block_update's handleTick will migrate them to bucket[1] and drain
		// at tick 1's mult (if tick 1 survives the crash check).
		for i, id := range ids {
			if err := c.BetAction(addrs[i], id, nil); err != nil {
				t.Fatalf("bet_action %d: %v", i, err)
			}
		}

		// One more block: handleTick fires tick 1 check.
		for step := 0; step < 200; step++ {
			c.AdvanceBlock()
			settled := 0
			for _, id := range ids {
				b := c.GetBet(id)
				if b != nil && b.Status != chainsim.BetOpen {
					settled++
				}
			}
			if settled == 100 {
				break
			}
		}

		wins, losses, payouts := 0, 0, uint64(0)
		expectedPayout := tsStakeUusdc * tsTickMult1Bp / 10000
		mismatched := 0
		for _, id := range ids {
			b := c.GetBet(id)
			if b == nil {
				continue
			}
			payouts += b.Payout
			if b.Payout > 0 {
				wins++
				if b.Payout != expectedPayout {
					mismatched++
				}
			} else {
				losses++
			}
		}

		fmt.Fprintln(&report, "--- 2. ManualTick1: 100 bets autocashout=134, manual cashout at tick 1 ---")
		fmt.Fprintf(&report, "  wins=%d losses=%d total_payout=%d uusdc\n", wins, losses, payouts)
		fmt.Fprintf(&report, "  expected payout per win: %d uusdc (1.035x stake)\n", expectedPayout)
		if wins == 100 && mismatched == 0 {
			fmt.Fprintln(&report, "  ✓ all 100 manually cashed out at tick 1 mult (overrode autocashout=134)")
		} else if losses == 100 {
			fmt.Fprintln(&report, "  ⚠ tick 1 crashed this seed — manual cashouts settled as loss (per existing rule)")
			t.Skip("tick 1 crashed; statistical scenario")
		} else {
			fmt.Fprintf(&report, "  ✗ unexpected: wins=%d losses=%d mismatched=%d\n", wins, losses, mismatched)
			t.Errorf("expected all-win or all-loss; got wins=%d losses=%d mismatched=%d", wins, losses, mismatched)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// Scenario 2b: 100 bets autocashout=tick 134, all manually cashout at tick 5
	// -------------------------------------------------------------------------
	t.Run("ManualTick5", func(t *testing.T) {
		c := tsNewChain(t)
		defer c.Close()
		if err := tsAdvanceTo(c, "open", 40); err != nil {
			t.Fatal(err)
		}

		ids := make([]uint64, 0, 100)
		addrs := make([]string, 0, 100)
		for i := 0; i < 100; i++ {
			addr := fmt.Sprintf("m5_%d", i)
			addrs = append(addrs, addr)
			c.Deposit(addr, 1_000_000_000)
			id, err := c.PlaceBet(addr, 1, 1, tsStakeUusdc, nil)
			if err != nil {
				t.Fatalf("place_bet %d: %v", i, err)
			}
			ids = append(ids, id)
		}

		// Wait until phase=tick AND tick reaches 4 (so next handleTick will
		// process cashouts at tick 5's mult).
		if err := tsAdvanceTo(c, "tick", 40); err != nil {
			t.Fatal(err)
		}
		// Drive blocks until current tick == 4.
		var observedTick uint64
		crashedEarly := false
		for step := 0; step < 50; step++ {
			raw, _ := c.GameQuery(1)
			if raw != nil {
				s := string(raw)
				if i := strings.Index(s, `"tick":`); i >= 0 {
					rest := s[i+7:]
					end := 0
					for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
						end++
					}
					fmt.Sscanf(rest[:end], "%d", &observedTick)
				}
			}
			if tsPhase(c) == "crashed" {
				crashedEarly = true
				break
			}
			if observedTick >= 4 {
				break
			}
			c.AdvanceBlock()
		}
		if crashedEarly {
			fmt.Fprintln(&report, "--- 2b. ManualTick5: 100 bets autocashout=134, manual cashout at tick 5 ---")
			fmt.Fprintln(&report, "  ⚠ round crashed before tick 4 reached — cannot exercise scenario")
			fmt.Fprintln(&report)
			t.Skip("round crashed before tick 4")
			return
		}

		// Send bet_action for all 100 — handleTick on the next block will
		// process the cashouts at tick 5's nextMult (~1.1875x).
		for i, id := range ids {
			if err := c.BetAction(addrs[i], id, nil); err != nil {
				t.Fatalf("bet_action %d: %v", i, err)
			}
		}

		// Drive until all settle.
		for step := 0; step < 200; step++ {
			c.AdvanceBlock()
			settled := 0
			for _, id := range ids {
				b := c.GetBet(id)
				if b != nil && b.Status != chainsim.BetOpen {
					settled++
				}
			}
			if settled == 100 {
				break
			}
		}

		const tick5MultBp uint64 = 11875 // 10000 * 1.035^5
		expectedPayout := tsStakeUusdc * tick5MultBp / 10000

		wins, losses, payouts := 0, 0, uint64(0)
		mismatched := 0
		for _, id := range ids {
			b := c.GetBet(id)
			if b == nil {
				continue
			}
			payouts += b.Payout
			if b.Payout > 0 {
				wins++
				if b.Payout != expectedPayout {
					mismatched++
				}
			} else {
				losses++
			}
		}

		fmt.Fprintln(&report, "--- 2b. ManualTick5: 100 bets autocashout=134, manual cashout at tick 5 ---")
		fmt.Fprintf(&report, "  observed tick at bet_action: %d (target 4)\n", observedTick)
		fmt.Fprintf(&report, "  wins=%d losses=%d total_payout=%d uusdc\n", wins, losses, payouts)
		fmt.Fprintf(&report, "  expected payout per win: %d uusdc (1.1875x stake)\n", expectedPayout)
		if wins == 100 && mismatched == 0 {
			fmt.Fprintln(&report, "  ✓ all 100 manually cashed out at tick 5 mult (overrode autocashout=134)")
		} else if losses == 100 {
			fmt.Fprintln(&report, "  ⚠ tick 5 crashed this seed — cashouts settled as loss")
			t.Skip("tick 5 crashed; statistical scenario")
		} else {
			fmt.Fprintf(&report, "  ✗ unexpected: wins=%d losses=%d mismatched=%d\n", wins, losses, mismatched)
			t.Errorf("expected all-win or all-loss; got wins=%d losses=%d mismatched=%d", wins, losses, mismatched)
		}
		fmt.Fprintln(&report)
	})

	// -------------------------------------------------------------------------
	// Scenario 3: 101 bets in one block — 101st must revert
	// -------------------------------------------------------------------------
	t.Run("Revert101", func(t *testing.T) {
		c := tsNewChain(t)
		defer c.Close()
		if err := tsAdvanceTo(c, "open", 40); err != nil {
			t.Fatal(err)
		}

		ok := 0
		var failErr error
		for i := 0; i < 101; i++ {
			addr := fmt.Sprintf("r_%d", i)
			c.Deposit(addr, 1_000_000_000)
			_, err := c.PlaceBet(addr, 1, 1, tsStakeUusdc, nil)
			if err != nil {
				failErr = err
				break
			}
			ok++
		}

		fmt.Fprintln(&report, "--- 3. Revert101: 101 bets in one OPEN block ---")
		fmt.Fprintf(&report, "  succeeded: %d / 101\n", ok)
		fmt.Fprintf(&report, "  101st failure: %v\n", failErr)
		if ok == 100 && failErr != nil &&
			(strings.Contains(failErr.Error(), "status=14") || strings.Contains(failErr.Error(), "status=15")) {
			fmt.Fprintln(&report, "  ✓ 100 placed; 101st correctly rejected (round/block cap)")
		} else {
			fmt.Fprintln(&report, "  ✗ expected 100 OK + status=14 (or 15) on 101st")
			t.Errorf("expected 100 OK + status=14|15, got OK=%d err=%v", ok, failErr)
		}
		fmt.Fprintln(&report)
	})

	// Always write report (even if some sub-tests skipped).
	t.Log("\n" + report.String())
	if err := os.WriteFile("four_scenarios.res.txt", []byte(report.String()), 0644); err != nil {
		t.Fatalf("write res: %v", err)
	}
}
