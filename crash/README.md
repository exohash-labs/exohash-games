# Crash

Multiplayer rocket-multiplier game. The multiplier rises every block from 1.00x. Cash out before it crashes — or lose your stake.

## How to play

1. Place a bet during the **open** phase. Set an autocashout target (1.10x to 100x) or ride to the cap.
2. The round starts. Multiplier rises ~3.5% per block.
3. Either the chain reaches your autocashout — you win `stake × multiplier`. Or it crashes first — you lose your stake.
4. Round resets after 5 blocks. Next round opens.

Up to **100 players per round**.

## The math

| Property | Value |
|---|---|
| RTP | 99% |
| House edge | 1% |
| Multiplier curve | `1.035 ^ tick` (3.5% growth per block) |
| Min multiplier | 1.07x (tick 2) |
| Max multiplier | 100.00x (tick 134) |
| Crash distribution | Geometric, edge-adjusted from beacon randomness |
| Max bet/round | 100 |
| Max bet/block | 100 |

The crash point is determined by mixing the validator-DKG beacon seed with the round number — committed *before* any player joins. Operators cannot influence it; players cannot predict it.

## Provable fairness

- **RNG source**: validator BLS-DKG randomness beacon (one fresh 32-byte seed per block).
- **Mixed with**: round id + calculator id (instance de-correlation).
- **Source**: [`src/main.go`](src/main.go).
- **Build**:
  ```bash
  tinygo build -o crash.wasm -target=wasi -opt=z -no-debug ./src
  ```
- **Verify**: `sha256sum crash.wasm` must match the deployed calculator's hash on [ExoScan](https://devnet.exohash.io).

Current shipped hash: `687ba794fc8aad37a64bd6226adf2a47f10e5dea8b62b1bd276725d3df3e1e25`

---

<details>
<summary>Developer reference</summary>

### Place bet params

After the 20-byte sender prefix prepended by the chain:

| Offset | Size | Field |
|---|---|---|
| 20..27 | u64 LE | `autocashout_tick` (1..134; 0 ⇒ default 134) |

### Bet actions

| Action byte | Meaning |
|---|---|
| any | Manual cashout request (will pay at next tick if round still alive) |

### Events

| Topic | Fields | When |
|---|---|---|
| `state` | `phase`, `round`, `mult_bp`, `tick`, `blocks_left`, `players`, `active`, `cashed` | Every block |
| `joined` | `bet_id`, `addr`, `stake`, `players`, `autocashout_tick` | Player joins |
| `cashout` | `bet_id`, `addr`, `round`, `stake`, `tick`, `mult_bp`, `payout` | Auto-cashout fires |
| `settled` | `bet_id`, `addr`, `round`, `stake`, `payout`, `kind`, `mult_bp` | Round ends (crash or max) |

`kind`: 1 = win, 2 = loss.

### Tests

```bash
go test ./tests/...                                 # smoke
go test -run TestLongRunPnL ./tests/... -timeout 30m  # 10M-round PnL prediction
```

</details>
