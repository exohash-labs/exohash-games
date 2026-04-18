// Mines long-run PnL audit — 100K games, 1 game at a time, random strategy.
//
//   - random mines ∈ [1..13]
//   - random K_target ∈ [1..5]
//   - if K_target == 5: rely on auto-cashout (auto fires at max reveals)
//     else: send manual bet_action(cashout) after K_target safe reveals
//   - tile selection: random from unopened tiles (skips already revealed)
//
// After each game: predict outcome deterministically by recomputing the
// WASM RNG (sha256(seed || bet_id_BE) % remaining < mines). Verify actual
// payout matches prediction. Mismatch → log + fail + stop.
//
// Stream every 1000 games to longrun_pnl.res.txt with cumulative stats +
// calc gas balance, so the file can be tail -f'd.
//
// Mirrors production: c.PurgeSettledBets() after each game.
//
//   go test -run TestMinesLongRunPnl -v -timeout 30m

package tests

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/exohash-labs/exohash-devkit/chainsim"
)

func TestMinesLongRunPnl(t *testing.T) {
	const (
		wasmPath      = "../mines.wasm"
		houseEdgeBp   = 100
		stakeUusdc    = 1_000_000
		bigHouseFunds = 10_000_000_000_000_000
		boardSize     = 25
		maxMinesGame  = 13
		maxRevealsTgt = 5
		rounds        = 10_000_000
		resPath       = "longrun_pnl.res.txt"
		progressEvery = 1000
		chainSeed     = uint64(42)
	)

	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read %s: %v", wasmPath, err)
	}

	c := chainsim.NewWithParams(chainsim.DefaultParams(), chainSeed)
	defer c.Close()
	c.Deposit("house", bigHouseFunds)
	brID, err := c.CreateBankroll("house", bigHouseFunds, "Test", false)
	if err != nil {
		t.Fatalf("create bankroll: %v", err)
	}
	if err := c.RegisterGame(1, wasm, "mines", houseEdgeBp); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := c.AttachGame(brID, 1); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := c.InitGame(1, brID); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Mirror WASM multiplier formula. fair = prod((25-i+1) / (safe-i+1)) for i=1..reveals.
	// edged = fair * (10000 - houseEdgeBp) / 10000.
	edgedMultBp := func(mines, reveals int) uint64 {
		safe := boardSize - mines
		num := uint64(1)
		den := uint64(1)
		for k := 1; k <= reveals; k++ {
			num *= uint64(boardSize - k + 1)
			den *= uint64(safe - k + 1)
		}
		fair := num * 10000 / den
		return fair * (10000 - houseEdgeBp) / 10000
	}

	// Mirror chainsim's beacon seed at a given height: sha256(chain_seed_LE || height_LE).
	beaconSeed := func(height uint64) [32]byte {
		var buf [16]byte
		binary.LittleEndian.PutUint64(buf[0:], chainSeed)
		binary.LittleEndian.PutUint64(buf[8:], height)
		return sha256.Sum256(buf[:])
	}

	// Mirror WASM resolveReveal: predict if a reveal at the given block height
	// (with revealedSoFar safe reveals already done) lands on a mine.
	predictMine := func(height, betID uint64, revealedSoFar, mines int) bool {
		seed := beaconSeed(height)
		var betBuf [8]byte
		binary.BigEndian.PutUint64(betBuf[:], betID)
		entropy := append(seed[:], betBuf[:]...)
		h := sha256.Sum256(entropy)
		remaining := uint64(boardSize - revealedSoFar)
		rng := binary.BigEndian.Uint64(h[0:8]) % remaining
		return rng < uint64(mines)
	}

	// Open results file; keep handle for streaming.
	f, err := os.Create(resPath)
	if err != nil {
		t.Fatalf("create %s: %v", resPath, err)
	}
	defer f.Close()
	writeLine := func(s string) {
		_, _ = f.WriteString(s)
		_ = f.Sync()
	}

	writeLine(fmt.Sprintf("=== Mines — long-run PnL audit (%d games) ===\n", rounds))
	writeLine(fmt.Sprintf("Started: %s\n", time.Now().Format(time.RFC3339)))
	writeLine(fmt.Sprintf("Strategy: 1 game at a time. Random mines ∈ [1..%d], random K_target ∈ [1..%d].\n", maxMinesGame, maxRevealsTgt))
	writeLine("If K_target == 5 → rely on auto-cashout. Else → manual cashout after K_target safe reveals.\n")
	writeLine("Random tile selection from unopened.\n\n")
	writeLine(fmt.Sprintf("%-7s %-9s %-9s %-15s %-15s %-12s %-14s\n",
		"round", "wins", "losses", "stake_uusdc", "paid_uusdc", "edge_bp", "gas_balance"))
	writeLine(strings.Repeat("-", 95) + "\n")

	rng := rand.New(rand.NewSource(42))
	wins, losses := 0, 0
	totalStake, totalPaid := uint64(0), uint64(0)
	t0 := time.Now()

	for r := 1; r <= rounds; r++ {
		mines := 1 + rng.Intn(maxMinesGame)
		kTarget := 1 + rng.Intn(maxRevealsTgt)

		addr := fmt.Sprintf("p%d", r)
		c.Deposit(addr, 10_000_000)
		betID, err := c.PlaceBet(addr, 1, 1, stakeUusdc, []byte{byte(mines)})
		if err != nil {
			writeLine(fmt.Sprintf("\n✗ FAIL round %d place_bet: %v\n", r, err))
			t.Fatalf("round %d place_bet: %v", r, err)
		}

		// Track which tiles have been opened and predict outcomes.
		var opened [boardSize]bool
		predictedHitMine := false
		actualSafeReveals := 0

		for reveal := 0; reveal < kTarget; reveal++ {
			// Pick a random unopened tile.
			free := make([]int, 0, boardSize)
			for i := 0; i < boardSize; i++ {
				if !opened[i] {
					free = append(free, i)
				}
			}
			tile := free[rng.Intn(len(free))]

			if err := c.BetAction(addr, betID, []byte{1, byte(tile)}); err != nil {
				writeLine(fmt.Sprintf("\n✗ FAIL round %d bet_action(reveal %d): %v\n", r, reveal+1, err))
				t.Fatalf("round %d bet_action: %v", r, err)
			}
			// block_update at next height resolves this reveal.
			heightOfResolve := c.Height() + 1
			advRes := c.AdvanceBlock()
			// Capture reveal_audit events for diagnostic purposes.
			var dbgEvents []string
			for _, ev := range advRes.CalcEvents {
				if ev.Topic == "reveal_audit" {
					dbgEvents = append(dbgEvents, string(ev.Data))
				}
			}

			predictedMine := predictMine(heightOfResolve, betID, reveal, mines)
			b := c.GetBet(betID)
			if b == nil {
				writeLine(fmt.Sprintf("\n✗ FAIL round %d: bet missing after reveal %d\n", r, reveal+1))
				t.Fatalf("round %d: bet missing", r)
			}

			if predictedMine {
				if b.Status != chainsim.BetSettled || b.Payout != 0 {
					seed := beaconSeed(heightOfResolve)
					var betBuf [8]byte
					binary.BigEndian.PutUint64(betBuf[:], betID)
					entropy := append(seed[:], betBuf[:]...)
					h := sha256.Sum256(entropy)
					remaining := uint64(boardSize - reveal)
					rng := binary.BigEndian.Uint64(h[0:8]) % remaining
					detail := fmt.Sprintf("\n✗ FAIL round %d reveal %d: predicted MINE but bet status=%v payout=%d\n"+
						"  mines=%d revealed_so_far=%d height=%d betID=%d\n"+
						"  my_seed_hex=%x\n"+
						"  betBE_hex=%x\n"+
						"  sha256(entropy)_hex=%x\n"+
						"  rng=%d remaining=%d threshold=%d => predicted_mine=%v\n"+
						"  WASM debug events: %v\n",
						r, reveal+1, b.Status, b.Payout,
						mines, reveal, heightOfResolve, betID,
						seed[:], betBuf[:], h[:],
						rng, remaining, mines, rng < uint64(mines),
						dbgEvents)
					writeLine(detail)
					t.Fatal(strings.TrimSpace(detail))
				}
				predictedHitMine = true
				break
			}
			// Predicted SAFE → bet should still be open (unless auto-cashout fires at K_target=5).
			opened[tile] = true
			actualSafeReveals++

			if reveal+1 == kTarget && kTarget == 5 {
				// Auto-cashout fires inside the same block_update for the 5th safe reveal.
				if b.Status != chainsim.BetSettled {
					detail := fmt.Sprintf("\n✗ FAIL round %d: predicted SAFE at K=5 but auto-cashout did not fire (status=%v)\n", r, b.Status)
					writeLine(detail)
					t.Fatal(strings.TrimSpace(detail))
				}
				break
			}

			if b.Status != chainsim.BetOpen {
				// Diagnostic dump: prediction vs reality.
				seed := beaconSeed(heightOfResolve)
				var betBuf [8]byte
				binary.BigEndian.PutUint64(betBuf[:], betID)
				entropy := append(seed[:], betBuf[:]...)
				h := sha256.Sum256(entropy)
				remaining := uint64(boardSize - reveal)
				rng := binary.BigEndian.Uint64(h[0:8]) % remaining
				detail := fmt.Sprintf("\n✗ FAIL round %d reveal %d: predicted SAFE but bet status=%v payout=%d\n"+
					"  mines=%d revealed_so_far=%d kTarget=%d tile=%d\n"+
					"  betID=%d height=%d\n"+
					"  my_seed_hex=%x\n"+
					"  chainsim_seed=%s (height=%d)\n"+
					"  betBE_hex=%x\n"+
					"  sha256(entropy)_hex=%x\n"+
					"  rng=%d remaining=%d threshold=%d => predicted_mine=%v\n"+
					"  WASM debug events: %v\n",
					r, reveal+1, b.Status, b.Payout,
					mines, reveal, kTarget, tile,
					betID, heightOfResolve,
					seed[:],
					c.BeaconSeedHex(heightOfResolve), heightOfResolve,
					betBuf[:], h[:],
					rng, remaining, mines, rng < uint64(mines),
					dbgEvents)
				writeLine(detail)
				t.Fatal(strings.TrimSpace(detail))
			}
		}

		// If reached K_target safe reveals AND K_target < 5: send manual cashout.
		if !predictedHitMine && actualSafeReveals == kTarget && kTarget < 5 {
			if err := c.BetAction(addr, betID, []byte{2}); err != nil {
				writeLine(fmt.Sprintf("\n✗ FAIL round %d bet_action(cashout): %v\n", r, err))
				t.Fatalf("round %d bet_action(cashout): %v", r, err)
			}
		}

		// Compute expected payout.
		var expected uint64
		if !predictedHitMine {
			expected = stakeUusdc * edgedMultBp(mines, kTarget) / 10000
		}

		bet := c.GetBet(betID)
		if bet == nil {
			writeLine(fmt.Sprintf("\n✗ FAIL round %d: bet missing at end\n", r))
			t.Fatalf("round %d: bet missing at end", r)
		}
		if bet.Payout != expected {
			detail := fmt.Sprintf("\n✗ FAIL round %d PNL MISMATCH\n  mines=%d kTarget=%d safe=%d hitMine=%v\n  actual=%d expected=%d\n",
				r, mines, kTarget, actualSafeReveals, predictedHitMine, bet.Payout, expected)
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

		// Mirror production: real chain auto-deletes settled bets.
		c.PurgeSettledBets()

		if r%progressEvery == 0 {
			edgeBp := 0.0
			if totalStake > 0 {
				edgeBp = float64(int64(totalStake)-int64(totalPaid)) / float64(totalStake) * 10000
			}
			writeLine(fmt.Sprintf("%-7d %-9d %-9d %-15d %-15d %-12.1f %-14d\n",
				r, wins, losses, totalStake, totalPaid, edgeBp, c.GasBalance(1)))
		}
	}

	dur := time.Since(t0)
	rtp := float64(totalPaid) / float64(totalStake) * 100
	edgeBp := float64(int64(totalStake)-int64(totalPaid)) / float64(totalStake) * 10000

	writeLine("\n")
	writeLine(fmt.Sprintf("Completed %d games in %s\n", rounds, dur.Round(time.Millisecond)))
	writeLine(fmt.Sprintf("Final: wins=%d losses=%d stake=%d paid=%d\n", wins, losses, totalStake, totalPaid))
	writeLine(fmt.Sprintf("Realized RTP: %.3f%% | Realized edge: %.1f bp (target 100bp)\n", rtp, edgeBp))
	writeLine(fmt.Sprintf("PnL settlement check: %d/%d games matched expected payout. ✓\n", rounds, rounds))
}
