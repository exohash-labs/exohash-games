# Mines

5×5 minefield — reveal tiles, avoid mines, cashout anytime.

**House edge: 1.00%** (100 bp). Each reveal uses a fresh beacon seed — mines are not pre-placed.

**Source:** [`src/main.go`](./src/main.go) · **Binary:** [`mines.wasm`](./mines.wasm)

---

## Test results

_Generated 2026-04-17T10:13:59Z · Chainsim run · Duration 1m21.843s_

| Section | Pass | Fail |
|---------|-----:|-----:|
| House edge convergence (target 1.00% / 100bp, 4σ dynamic tolerance, 50000 bets per config) | 10 | 0 |
| Wrong-player behavior (rejection semantics) | 21 | 0 |
| Gas (instrumentation + accounting) | 4 | 0 |

## House edge convergence (target 1.00% / 100bp, 4σ dynamic tolerance, 50000 bets per config) — PASS

| Mines | Reveals | P(all safe) | Multiplier | Bets   | Realized edge | Tol (4σ) | Delta | Verdict |
|-------|---------|-------------|------------|--------|---------------|----------|-------|---------|
|     1 |       1 |     0.9600  |     1.031x |  50000 |     108.9 bp  |   36.1 bp |   8.9 | ✓ |
|     1 |       3 |     0.8800  |     1.125x |  50000 |      92.3 bp  |   65.4 bp |   7.7 | ✓ |
|     1 |       5 |     0.8000  |     1.238x |  50000 |      72.0 bp  |   88.5 bp |  28.0 | ✓ |
|     3 |       1 |     0.8800  |     1.125x |  50000 |      94.8 bp  |   65.4 bp |   5.2 | ✓ |
|     3 |       3 |     0.6696  |     1.479x |  50000 |     102.0 bp  |  124.4 bp |   2.0 | ✓ |
|     3 |       5 |     0.4957  |     1.997x |  50000 |      57.4 bp  |  178.6 bp |  42.6 | ✓ |
|     5 |       2 |     0.6333  |     1.563x |  50000 |      31.2 bp  |  134.8 bp |  68.8 | ✓ |
|     5 |       4 |     0.3830  |     2.585x |  50000 |      87.2 bp  |  224.8 bp |  12.8 | ✓ |
|     8 |       1 |     0.6800  |     1.456x |  50000 |      84.9 bp  |  121.5 bp |  15.1 | ✓ |
|     8 |       3 |     0.2957  |     3.349x |  50000 |     175.8 bp  |  273.3 bp |  75.8 | ✓ |

- ✓ **mines=1 reveals=1** — `realized=108.9bp tol(4σ)=36.1bp delta=8.9bp staked=50000000000 paid=49455680400`
- ✓ **mines=1 reveals=3** — `realized=92.3bp tol(4σ)=65.4bp delta=7.7bp staked=50000000000 paid=49538346200`
- ✓ **mines=1 reveals=5** — `realized=72.0bp tol(4σ)=88.5bp delta=28.0bp staked=50000000000 paid=49639837500`
- ✓ **mines=3 reveals=1** — `realized=94.8bp tol(4σ)=65.4bp delta=5.2bp staked=50000000000 paid=49525972300`
- ✓ **mines=3 reveals=3** — `realized=102.0bp tol(4σ)=124.4bp delta=2.0bp staked=50000000000 paid=49489830500`
- ✓ **mines=3 reveals=5** — `realized=57.4bp tol(4σ)=178.6bp delta=42.6bp staked=50000000000 paid=49712797000`
- ✓ **mines=5 reveals=2** — `realized=31.2bp tol(4σ)=134.8bp delta=68.8bp staked=50000000000 paid=49844132800`
- ✓ **mines=5 reveals=4** — `realized=87.2bp tol(4σ)=224.8bp delta=12.8bp staked=50000000000 paid=49564207200`
- ✓ **mines=8 reveals=1** — `realized=84.9bp tol(4σ)=121.5bp delta=15.1bp staked=50000000000 paid=49575319200`
- ✓ **mines=8 reveals=3** — `realized=175.8bp tol(4σ)=273.3bp delta=75.8bp staked=50000000000 paid=49121028000`

## Wrong-player behavior (rejection semantics) — PASS

- ✓ **params too short (<21 bytes)** — `place_bet rejected (status=11)`
- ✓ **mines=0 out of [1,13]** — `place_bet rejected (status=12)`
- ✓ **mines=14 out of [1,13]** — `place_bet rejected (status=12)`
- ✓ **mines=255 out of [1,13]** — `place_bet rejected (status=12)`
- ✓ **stake=0** — `stake must be > 0`
- ✓ **stake below MinStakeUusdc** — `stake 1000 below minimum 100000`
- ✓ **stake > player balance** — `insufficient balance: 50000 < 1000000`
- ✓ **max_payout exceeds bankroll cap** — `place_bet rejected (status=3)`
- ✓ **bet_action: empty payload** — `bet_action rejected (status=1)`
- ✓ **bet_action: unknown action=0** — `bet_action rejected (status=2)`
- ✓ **bet_action: unknown action=3** — `bet_action rejected (status=2)`
- ✓ **bet_action: unknown action=255** — `bet_action rejected (status=2)`
- ✓ **reveal: tile index >= 25** — `bet_action rejected (status=34)`
- ✓ **reveal: payload missing tile byte** — `bet_action rejected (status=33)`
- ✓ **reveal: same tile twice** — `bet_action rejected (status=35)`
- ✓ **reveal: during waiting-RNG phase** — `bet_action rejected (status=31)`
- ✓ **cashout: before any reveal** — `bet_action rejected (status=43)`
- ✓ **cashout: during waiting-RNG phase** — `bet_action rejected (status=41)`
- ✓ **bet on unattached game** — `game 1 not active on bankroll 1`
- ✓ **bet on killed calculator** — `calculator 1 is not active (status=2)`
- ✓ **bet while beacon down** — `beacon randomness unavailable — games paused`

## Gas (instrumentation + accounting) — PASS

- ✓ **initial gas balance = params.GasInitialCredits** — `want=1000000000 got=1000000000`
- ✓ **per-round gas is O(1) in history** — `avg WASM gas/round: 100→348725, 500→352631, 2000→352181 (2000/100 = 1.01x)`
- ✓ **gas balance stays under (initial + N·credit)** — `initial=1000000000 final=1065127423 ceiling(initial+n·credit)=1100000000`
- ✓ **gas exhaustion → calculator killed** — `killedAt=2 status=2 (kill reason emitted as event)`

---

_Reproduce: `go run .` from this directory._
