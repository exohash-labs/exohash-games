// Long-run PnL audit — 1,000,000 rounds with 5 players per round, random
// cashout strategy:
//
//   - autoTick uniform in [1, 134]
//   - 20% chance per player of scheduling a manual cashout at uniform tick
//     in [1, autoTick]; bet_action sent when current_tick reaches manual-1
//
// After each round: predict expected payout deterministically (handles auto,
// manual, and cap-fired branches) and compare to actual. On mismatch: log +
// fail + stop immediately.
//
// Every 1000 rounds: append a progress line (cumulative stats, realized
// house edge, calculator gas balance) to longrun_pnl.res.txt and Sync, so
// the file can be tail -f'd while the test runs.
//
// After each round: c.PurgeSettledBets() is called to mirror production
// behavior (real chain auto-deletes settled bets in the host_settle path).
//
//   go test -run TestLongRunPnl -v -timeout 2h

package tests

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/exohash-labs/exohash-devkit/chainsim"
)

func TestLongRunPnl(t *testing.T) {
	const (
		wasmPath      = "../crash.wasm"
		houseEdgeBp   = 100
		stakeUusdc    = 1_000_000
		bigHouseFunds = 10_000_000_000_000_000
		rounds          = 100_000
		manualProb      = 0.20
		maxTick         = 134
		resPath         = "longrun_pnl.res.txt"
		progressEvery   = 1000
		playersPerRound = 5
	)

	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read %s: %v", wasmPath, err)
	}

	c := chainsim.NewWithParams(chainsim.DefaultParams(), 42)
	defer c.Close()
	c.Deposit("house", bigHouseFunds)
	brID, err := c.CreateBankroll("house", bigHouseFunds, "Test", false)
	if err != nil {
		t.Fatalf("create bankroll: %v", err)
	}
	if err := c.RegisterGame(1, wasm, "crash", houseEdgeBp); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := c.AttachGame(brID, 1); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := c.InitGame(1, brID); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Pre-compute tick mults.
	tickMult := make([]uint64, maxTick+1)
	tickMult[0] = 10000
	for i := 1; i <= maxTick; i++ {
		next := tickMult[i-1] * 10350 / 10000
		if next <= tickMult[i-1] {
			next = tickMult[i-1] + 1
		}
		if next >= 1_000_000 {
			next = 1_000_000
		}
		tickMult[i] = next
	}

	queryPhaseTick := func() (string, uint64) {
		raw, _ := c.GameQuery(1)
		if raw == nil {
			return "?", 0
		}
		s := string(raw)
		phase := "?"
		if i := strings.Index(s, `"phase":"`); i >= 0 {
			rest := s[i+9:]
			if j := strings.Index(rest, `"`); j > 0 {
				phase = rest[:j]
			}
		}
		var tick uint64
		if i := strings.Index(s, `"tick":`); i >= 0 {
			rest := s[i+7:]
			end := 0
			for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
				end++
			}
			fmt.Sscanf(rest[:end], "%d", &tick)
		}
		return phase, tick
	}

	queryMult := func() uint64 {
		raw, _ := c.GameQuery(1)
		if raw == nil {
			return 0
		}
		s := string(raw)
		var mult uint64
		if i := strings.Index(s, `"mult_bp":`); i >= 0 {
			rest := s[i+10:]
			end := 0
			for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
				end++
			}
			fmt.Sscanf(rest[:end], "%d", &mult)
		}
		return mult
	}

	advanceTo := func(want string, max int) error {
		for i := 0; i < max; i++ {
			ph, _ := queryPhaseTick()
			if ph == want {
				return nil
			}
			c.AdvanceBlock()
		}
		ph, tick := queryPhaseTick()
		calc, _ := c.GetCalculator(1)
		calcStatus := -1
		if calc != nil {
			calcStatus = int(calc.Status)
		}
		gasBal := c.GasBalance(1)
		return fmt.Errorf("never reached phase %q within %d blocks (last phase=%q tick=%d calc_status=%d gas_balance=%d)",
			want, max, ph, tick, calcStatus, gasBal)
	}

	// Open results file fresh; keep handle open for streaming.
	f, err := os.Create(resPath)
	if err != nil {
		t.Fatalf("create %s: %v", resPath, err)
	}
	defer f.Close()
	writeLine := func(s string) {
		_, _ = f.WriteString(s)
		_ = f.Sync()
	}

	writeLine(fmt.Sprintf("=== Crash — long-run PnL audit (%d rounds) ===\n", rounds))
	writeLine(fmt.Sprintf("Started: %s\n", time.Now().Format(time.RFC3339)))
	writeLine(fmt.Sprintf("Strategy: %d players per round, each with autoTick uniform [1..%d] and %.0f%% chance of manual cashout at tick [1..autoTick]\n", playersPerRound, maxTick, manualProb*100))
	writeLine("\n")
	writeLine(fmt.Sprintf("%-8s %-9s %-9s %-15s %-15s %-12s %-14s\n",
		"round", "wins", "losses", "stake_uusdc", "paid_uusdc", "edge_bp", "gas_balance"))
	writeLine(strings.Repeat("-", 95) + "\n")

	rng := rand.New(rand.NewSource(42))

	wins, losses := 0, 0
	totalStake, totalPaid := uint64(0), uint64(0)
	t0 := time.Now()

	type player struct {
		idx        int
		addr       string
		betID      uint64
		autoTick   int
		manualTick int
		manualSent bool
	}

	for r := 1; r <= rounds; r++ {
		if err := advanceTo("open", 400); err != nil {
			writeLine(fmt.Sprintf("\n✗ FAIL round %d: %v\n", r, err))
			t.Fatalf("round %d: %v", r, err)
		}

		players := make([]*player, playersPerRound)
		for i := 0; i < playersPerRound; i++ {
			autoTick := 1 + rng.Intn(maxTick)
			manualTick := 0
			if rng.Float64() < manualProb {
				manualTick = 1 + rng.Intn(autoTick)
			}
			addr := fmt.Sprintf("p_r%d_p%d", r, i)
			c.Deposit(addr, 10_000_000)
			params := make([]byte, 8)
			binary.LittleEndian.PutUint64(params, uint64(autoTick))
			betID, err := c.PlaceBet(addr, 1, 1, stakeUusdc, params)
			if err != nil {
				writeLine(fmt.Sprintf("\n✗ FAIL round %d player %d place_bet: %v\n", r, i, err))
				t.Fatalf("round %d player %d place_bet: %v", r, i, err)
			}
			players[i] = &player{idx: i, addr: addr, betID: betID, autoTick: autoTick, manualTick: manualTick}
		}

		var maxSurvived uint64
		for step := 0; step < 200; step++ {
			c.AdvanceBlock()
			ph, tick := queryPhaseTick()
			if ph == "tick" && tick > maxSurvived {
				maxSurvived = tick
			}
			// Send each pending manual cashout when current_tick == manualTick-1.
			if ph == "tick" {
				for _, p := range players {
					if !p.manualSent && p.manualTick > 0 && tick == uint64(p.manualTick-1) {
						if e := c.BetAction(p.addr, p.betID, nil); e == nil {
							p.manualSent = true
						}
					}
				}
			}
			// Round done when all players' bets are settled.
			allSettled := true
			for _, p := range players {
				b := c.GetBet(p.betID)
				if b == nil || b.Status == chainsim.BetOpen {
					allSettled = false
					break
				}
			}
			if allSettled {
				break
			}
		}
		// Detect "round capped" — when the multiplier ceiling fires, the WASM
		// pays everyone at maxMult but does NOT bump ticksElapsed past the last
		// pre-cap tick. So query reports tick=133 even though the cap fired at
		// the tick-134 attempt. Promote maxSurvived to 134 in that case so the
		// prediction logic treats autoTick=134 bets as winners at the cap mult.
		if queryMult() == 1_000_000 {
			maxSurvived = uint64(maxTick)
		}

		// Verify each player's payout matches prediction.
		for _, p := range players {
			bet := c.GetBet(p.betID)
			if bet == nil {
				writeLine(fmt.Sprintf("\n✗ FAIL round %d player %d: bet missing\n", r, p.idx))
				t.Fatalf("round %d player %d: bet missing", r, p.idx)
			}

			var expected uint64
			var outcome string
			if p.manualTick > 0 {
				if p.manualSent {
					if maxSurvived >= uint64(p.manualTick) {
						expected = stakeUusdc * tickMult[p.manualTick] / 10000
						outcome = fmt.Sprintf("MANUAL_WIN(@tick%d)", p.manualTick)
					} else {
						expected = 0
						outcome = "MANUAL_LOSS_AT_FIRE"
					}
				} else {
					expected = 0
					outcome = "MANUAL_LOSS_BEFORE_SEND"
				}
			} else {
				if maxSurvived >= uint64(p.autoTick) {
					expected = stakeUusdc * tickMult[p.autoTick] / 10000
					outcome = fmt.Sprintf("AUTO_WIN(@tick%d)", p.autoTick)
				} else {
					expected = 0
					outcome = "AUTO_LOSS"
				}
			}

			if bet.Payout != expected {
				detail := fmt.Sprintf(
					"\n✗ FAIL round %d player %d PNL MISMATCH\n  autoTick=%d manualTick=%d manualSent=%v maxSurvived=%d\n  outcome=%s actual=%d expected=%d\n",
					r, p.idx, p.autoTick, p.manualTick, p.manualSent, maxSurvived, outcome, bet.Payout, expected,
				)
				writeLine(detail)
				t.Fatal(strings.TrimSpace(detail))
			}

			totalStake += stakeUusdc
			totalPaid += bet.Payout
			if bet.Payout > 0 {
				wins++
			} else {
				losses++
			}
		}

		// Mirror production: real chain auto-deletes settled bets in
		// host_settle. Without this, chainsim accumulates bet records in
		// memory across the long run.
		c.PurgeSettledBets()

		if r%progressEvery == 0 {
			edgeBp := 0.0
			if totalStake > 0 {
				edgeBp = float64(int64(totalStake)-int64(totalPaid)) / float64(totalStake) * 10000
			}
			writeLine(fmt.Sprintf("%-8d %-9d %-9d %-15d %-15d %-12.1f %-14d\n",
				r, wins, losses, totalStake, totalPaid, edgeBp, c.GasBalance(1)))
		}
	}

	dur := time.Since(t0)
	rtp := float64(totalPaid) / float64(totalStake) * 100
	edgeBp := float64(int64(totalStake)-int64(totalPaid)) / float64(totalStake) * 10000

	writeLine("\n")
	writeLine(fmt.Sprintf("Completed %d rounds in %s\n", rounds, dur.Round(time.Millisecond)))
	writeLine(fmt.Sprintf("Final: wins=%d losses=%d stake=%d paid=%d\n", wins, losses, totalStake, totalPaid))
	writeLine(fmt.Sprintf("Realized RTP: %.3f%% | Realized edge: %.1f bp (target 100bp)\n", rtp, edgeBp))
	writeLine(fmt.Sprintf("PnL settlement check: %d/%d rounds matched expected payout. ✓\n", rounds, rounds))
}
