// Dice long-run PnL audit — 100K games, random strategy, deterministic
// PnL prediction.
//
//   - random mode ∈ {1 (over), 2 (under)}
//   - random threshold so chance ∈ [200, 9700]
//   - place + advance one block → settle
//   - predict: roll = sha256(seed || bet_id_BE || calc_id_LE)[:8] BE % 10000
//             win = (mode==1 && roll >= 10000-effChance) || (mode==2 && roll < effChance)
//             where effChance = chance × 99 / 100 (1% house edge)
//   - verify actual payout matches; mismatch → fail + stop.
//
// Stream every 1000 games to longrun_pnl.res.txt.
//
//   go test -run TestDiceLongRunPnl -v -timeout 30m

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

func TestDiceLongRunPnl(t *testing.T) {
	const (
		wasmPath      = "../dice.wasm"
		houseEdgeBp   = 100
		stakeUusdc    = 1_000_000
		bigHouseFunds = 10_000_000_000_000_000
		rounds        = 100_000
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
	if err := c.RegisterGame(1, wasm, "dice", houseEdgeBp); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := c.AttachGame(brID, 1); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := c.InitGame(1, brID); err != nil {
		t.Fatalf("init: %v", err)
	}
	calcID := uint64(1) // chainsim assigns sequential calculator IDs starting at 1

	// Mirror chainsim's beacon seed: sha256(chain_seed_LE || height_LE).
	beaconSeed := func(height uint64) [32]byte {
		var buf [16]byte
		binary.LittleEndian.PutUint64(buf[0:], chainSeed)
		binary.LittleEndian.PutUint64(buf[8:], height)
		return sha256.Sum256(buf[:])
	}

	// Predict the roll the WASM will compute: sha256(seed || bet_id_BE || calc_id_LE)[:8] BE % 10000
	predictRoll := func(height, betID, calcID uint64) uint64 {
		seed := beaconSeed(height)
		var ebuf [16]byte
		binary.BigEndian.PutUint64(ebuf[0:], betID)
		binary.LittleEndian.PutUint64(ebuf[8:], calcID)
		entropy := append(seed[:], ebuf[:]...)
		h := sha256.Sum256(entropy)
		return binary.BigEndian.Uint64(h[0:8]) % 10000
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

	writeLine(fmt.Sprintf("=== Dice — long-run PnL audit (%d games) ===\n", rounds))
	writeLine(fmt.Sprintf("Started: %s\n", time.Now().Format(time.RFC3339)))
	writeLine("Strategy: 1 game at a time. Random mode ∈ {1,2}, random chance ∈ [200, 9700] bp.\n\n")
	writeLine(fmt.Sprintf("%-7s %-9s %-9s %-15s %-15s %-12s %-14s\n",
		"round", "wins", "losses", "stake_uusdc", "paid_uusdc", "edge_bp", "gas_balance"))
	writeLine(strings.Repeat("-", 95) + "\n")

	rng := rand.New(rand.NewSource(42))
	wins, losses := 0, 0
	totalStake, totalPaid := uint64(0), uint64(0)
	t0 := time.Now()

	for r := 1; r <= rounds; r++ {
		// Pick mode and chance.
		mode := byte(1 + rng.Intn(2))           // 1 or 2
		chance := uint64(200 + rng.Intn(9501))  // chance ∈ [200, 9700]
		var threshold uint64
		if mode == 1 {
			threshold = 10000 - chance
		} else {
			threshold = chance
		}

		params := make([]byte, 9)
		params[0] = mode
		binary.LittleEndian.PutUint64(params[1:], threshold)

		addr := fmt.Sprintf("p%d", r)
		c.Deposit(addr, 10_000_000)
		betID, err := c.PlaceBet(addr, 1, 1, stakeUusdc, params)
		if err != nil {
			writeLine(fmt.Sprintf("\n✗ FAIL round %d place_bet: %v\n", r, err))
			t.Fatalf("round %d place_bet: %v", r, err)
		}

		// Settle next block.
		heightOfResolve := c.Height() + 1
		c.AdvanceBlock()

		// Predict.
		effChance := chance * (10000 - uint64(houseEdgeBp)) / 10000
		roll := predictRoll(heightOfResolve, betID, calcID)
		var predictedWin bool
		switch mode {
		case 1:
			predictedWin = roll >= 10000-effChance
		case 2:
			predictedWin = roll < effChance
		}

		// Compute expected payout.
		// fairMult = 10000^2 / chance (mirrors WASM's fairMultBP)
		fairMult := uint64(10000) * 10000 / chance
		var expected uint64
		if predictedWin {
			expected = stakeUusdc * fairMult / 10000
		}

		bet := c.GetBet(betID)
		if bet == nil {
			writeLine(fmt.Sprintf("\n✗ FAIL round %d: bet missing\n", r))
			t.Fatalf("round %d: bet missing", r)
		}
		if bet.Payout != expected {
			detail := fmt.Sprintf("\n✗ FAIL round %d PNL MISMATCH\n  mode=%d chance=%d threshold=%d effChance=%d\n  betID=%d height=%d roll=%d predictedWin=%v\n  actual=%d expected=%d (fairMult=%d)\n",
				r, mode, chance, threshold, effChance, betID, heightOfResolve, roll, predictedWin, bet.Payout, expected, fairMult)
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

		// Mirror production cleanup.
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
