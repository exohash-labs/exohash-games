# Crash

Multiplayer crash — rising multiplier with random crash point.

**House edge: 1.00%** (100 bp) applied at the first tick; subsequent ticks are fair (survival probability = previous_mult / next_mult).

**Source:** [`src/main.go`](./src/main.go) · **Binary:** [`crash.wasm`](./crash.wasm)

---

## Test results

_Generated 2026-04-17T10:23:00Z · Chainsim run · Duration 21.806s_

| Section | Pass | Fail |
|---------|-----:|-----:|
| House edge convergence (target 1.00% / 100bp, 4σ dynamic tolerance, 200 rounds × 50 bets per config) | 5 | 0 |
| Wrong-player behavior (rejection semantics) | 11 | 0 |
| Gas (instrumentation + accounting) | 4 | 0 |

## House edge convergence (target 1.00% / 100bp, 4σ dynamic tolerance, 200 rounds × 50 bets per config) — PASS

| Strategy          | Target | Bets    | Realized edge | Tol (4σ) | Delta | Verdict |
|-------------------|--------|---------|---------------|----------|-------|---------|
| cashout at 1.05x  |  1.05x |   10000 |    -199.1 bp  |  689.1 bp | 299.1 | ✓ |
| cashout at 1.20x  |  1.20x |   10000 |     332.8 bp  | 1289.7 bp | 232.8 | ✓ |
| cashout at 1.50x  |  1.50x |   10000 |    1167.9 bp  | 2009.8 bp | 1067.9 | ✓ |
| cashout at 2.00x  |  2.00x |   10000 |    1799.5 bp  | 2828.3 bp | 1699.5 | ✓ |
| cashout at 3.00x  |  3.00x |   10000 |    1605.4 bp  | 3989.9 bp | 1505.4 | ✓ |

- ✓ **cashout at 1.05x** — `realized=-199.1bp tol(4σ)=689.1bp delta=299.1bp staked=10000000000 paid=10199120000 (N=10000 bets across 200 rounds)`
- ✓ **cashout at 1.20x** — `realized=332.8bp tol(4σ)=1289.7bp delta=232.8bp staked=10000000000 paid=9667200000 (N=10000 bets across 200 rounds)`
- ✓ **cashout at 1.50x** — `realized=1167.9bp tol(4σ)=2009.8bp delta=1067.9bp staked=10000000000 paid=8832080000 (N=10000 bets across 200 rounds)`
- ✓ **cashout at 2.00x** — `realized=1799.5bp tol(4σ)=2828.3bp delta=1699.5bp staked=10000000000 paid=8200500000 (N=10000 bets across 200 rounds)`
- ✓ **cashout at 3.00x** — `realized=1605.4bp tol(4σ)=3989.9bp delta=1505.4bp staked=10000000000 paid=8394570000 (N=10000 bets across 200 rounds)`

## Wrong-player behavior (rejection semantics) — PASS

- ✓ **place_bet during tick phase** — `place_bet rejected (status=10)`
- ✓ **place_bet during crashed phase** — `place_bet rejected (status=10)`
- ✓ **stake=0** — `stake must be > 0`
- ✓ **stake below MinStakeUusdc** — `stake 1000 below minimum 100000`
- ✓ **stake > player balance** — `insufficient balance: 50000 < 1000000`
- ✓ **max_payout exceeds bankroll cap** — `place_bet rejected (status=3)`
- ✓ **bet_action during open phase** — `bet_action rejected (status=20)`
- ✓ **bet_action on already-cashed bet** — `bet_action rejected (status=21)`
- ✓ **bet on unattached game** — `game 1 not active on bankroll 1`
- ✓ **bet on killed calculator** — `calculator 1 is not active (status=2)`
- ✓ **bet while beacon down** — `beacon randomness unavailable — games paused`

## Gas (instrumentation + accounting) — PASS

- ✓ **initial gas balance = params.GasInitialCredits** — `want=1000000000 got=1000000000`
- ✓ **per-round gas is O(1) in history** — `avg WASM gas/round: 20→6363035, 100→6454170, 300→6780413 (300/20 = 1.07x)`
- ✓ **gas balance stays under (initial + N·credit)** — `initial=1000000000 final=986216239 ceiling(initial+90·credit)=1090000000`
- ✓ **gas exhaustion → calculator killed** — `killedAt=10 status=2 (block_update consumed gas until kill)`

---

_Reproduce: `go run .` from this directory._
