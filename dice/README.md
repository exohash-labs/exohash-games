# Dice

Provably fair dice — single bet, single outcome, next-block settlement.

**House edge: 1.00%** (100 bp). RTP 99.00%.

**Source:** [`src/main.go`](./src/main.go) · **Binary:** [`dice.wasm`](./dice.wasm)

---

## Test results

_Generated 2026-04-17T12:41:47Z · Chainsim run · Duration 34.42s_

| Section | Pass | Fail |
|---------|-----:|-----:|
| House edge convergence (target 1.00% / 100bp, 4σ dynamic tolerance, 100000 bets per config) | 10 | 0 |
| Wrong-player behavior (rejection semantics) | 13 | 0 |
| Gas (instrumentation + accounting) | 4 | 0 |

## House edge convergence (target 1.00% / 100bp, 4σ dynamic tolerance, 100000 bets per config) — PASS

| Mode  | Chance | Bets    | Realized edge | Tol (4σ) | Delta | Verdict |
|-------|--------|---------|---------------|----------|-------|---------|
| over  |  10.0% |  100000 |     278.0 bp  |  375.7 bp | 178.0 | ✓ |
| over  |  25.0% |  100000 |     203.2 bp  |  216.9 bp | 103.2 | ✓ |
| over  |  50.0% |  100000 |     117.6 bp  |  125.2 bp |  17.6 | ✓ |
| over  |  75.0% |  100000 |      82.9 bp  |   72.3 bp |  17.1 | ✓ |
| over  |  90.0% |  100000 |      80.2 bp  |   41.7 bp |  19.8 | ✓ |
| under |  10.0% |  100000 |     274.0 bp  |  375.7 bp | 174.0 | ✓ |
| under |  25.0% |  100000 |     145.6 bp  |  216.9 bp |  45.6 | ✓ |
| under |  50.0% |  100000 |      85.8 bp  |  125.2 bp |  14.2 | ✓ |
| under |  75.0% |  100000 |      67.4 bp  |   72.3 bp |  32.6 | ✓ |
| under |  90.0% |  100000 |      79.1 bp  |   41.7 bp |  20.9 | ✓ |

- ✓ **over mode / chance=10.0%** — `realized=278.0bp tol(4σ)=375.7bp delta=178.0bp staked=100000000000 paid=97220000000`
- ✓ **over mode / chance=25.0%** — `realized=203.2bp tol(4σ)=216.9bp delta=103.2bp staked=100000000000 paid=97968000000`
- ✓ **over mode / chance=50.0%** — `realized=117.6bp tol(4σ)=125.2bp delta=17.6bp staked=100000000000 paid=98824000000`
- ✓ **over mode / chance=75.0%** — `realized=82.9bp tol(4σ)=72.3bp delta=17.1bp staked=100000000000 paid=99170854000`
- ✓ **over mode / chance=90.0%** — `realized=80.2bp tol(4σ)=41.7bp delta=19.8bp staked=100000000000 paid=99197896900`
- ✓ **under mode / chance=10.0%** — `realized=274.0bp tol(4σ)=375.7bp delta=174.0bp staked=100000000000 paid=97260000000`
- ✓ **under mode / chance=25.0%** — `realized=145.6bp tol(4σ)=216.9bp delta=45.6bp staked=100000000000 paid=98544000000`
- ✓ **under mode / chance=50.0%** — `realized=85.8bp tol(4σ)=125.2bp delta=14.2bp staked=100000000000 paid=99142000000`
- ✓ **under mode / chance=75.0%** — `realized=67.4bp tol(4σ)=72.3bp delta=32.6bp staked=100000000000 paid=99325516800`
- ✓ **under mode / chance=90.0%** — `realized=79.1bp tol(4σ)=41.7bp delta=20.9bp staked=100000000000 paid=99209007900`

## Wrong-player behavior (rejection semantics) — PASS

- ✓ **params too short (<9 bytes)** — `place_bet rejected (status=1)`
- ✓ **mode=0 (invalid)** — `place_bet rejected (status=2)`
- ✓ **mode=3 (invalid)** — `place_bet rejected (status=2)`
- ✓ **mode=255 (invalid)** — `place_bet rejected (status=2)`
- ✓ **chance=0% (under, threshold=0)** — `place_bet rejected (status=2)`
- ✓ **chance=100% (under, threshold=10000)** — `place_bet rejected (status=2)`
- ✓ **stake=0** — `stake must be > 0`
- ✓ **stake below MinStakeUusdc** — `stake 1000 below minimum 100000`
- ✓ **stake > player balance** — `insufficient balance: 50000 < 1000000`
- ✓ **max_payout exceeds bankroll cap** — `place_bet rejected (status=3)`
- ✓ **bet on unattached game** — `game 1 not active on bankroll 1`
- ✓ **bet on killed calculator** — `calculator 1 is not active (status=2)`
- ✓ **bet while beacon down** — `beacon randomness unavailable — games paused`

## Gas (instrumentation + accounting) — PASS

- ✓ **initial gas balance = params.GasInitialCredits** — `want=1000000000 got=1000000000`
- ✓ **per-bet gas is O(1) in history** — `avg WASM gas/bet: 100→45838, 1000→45849, 5000→46134 (5000/100 = 1.01x)`
- ✓ **gas balance stays under (initial + N·credit)** — `initial=1000000000 final=1178193818 ceiling(initial+n·credit)=1200000000`
- ✓ **gas exhaustion → calculator killed** — `killedAt=3 err="place_bet: gas balance exhausted (used 47953)" status=2`

---

_Reproduce: `go run .` from this directory._
