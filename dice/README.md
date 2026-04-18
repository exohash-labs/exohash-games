# Dice

Pick a number 1–98. Bet over or under. Settle on the next block.

## How to play

1. Set your stake.
2. Pick a win chance (1% to 98%).
3. Bet **over** or **under** the threshold.
4. The roll happens on the next block. You see the result instantly.

Lower win chance = bigger payout. Higher chance = smaller payout. The math is simple and the same for every bet.

## The math

| Property | Value |
|---|---|
| RTP | 99% |
| House edge | 1% |
| Win chance range | 1% to 98% |
| Payout | `stake × (10000 / chance_bp − edge_adjustment)` |
| Roll range | 0..9999 (basis points) |
| Max bet/block | 100 |

A 50% bet pays ~1.98x. A 1% bet pays ~98x. A 98% bet pays ~1.01x. Edge is taken from the win-probability side, not the multiplier — so the displayed multiplier is what you actually win.

## Provable fairness

- **RNG source**: validator BLS-DKG randomness beacon (one fresh 32-byte seed per block).
- **Mixed with**: bet id + calculator id.
- **Roll**: `SHA-256(seed || bet_id || calc_id) % 10000` — committed *before* settlement.
- **Source**: [`src/main.go`](src/main.go).
- **Build**:
  ```bash
  tinygo build -o dice.wasm -target=wasi -opt=z -no-debug ./src
  ```
- **Verify**: `sha256sum dice.wasm` must match the deployed calculator's hash on [ExoScan](https://devnet.exohash.io).

Current shipped hash: `c569f2ea7e92d53ddc2086d11167b30f8a7e8e57e1f0ca77f5ff9e21ed6aa38b`

Every settlement emits a `roll_audit` event containing the entropy bytes — third parties can replay the RNG and verify the outcome.

---

<details>
<summary>Developer reference</summary>

### Place bet params

After the 20-byte sender prefix:

| Offset | Size | Field |
|---|---|---|
| 20 | u8 | `mode` (1 = over, 2 = under) |
| 21..28 | u64 LE | `threshold_bp` (100..9800; 100 bp = 1%) |

### Bet actions

None. Dice settles automatically next block.

### Events

| Topic | Fields | When |
|---|---|---|
| `bet` | `entry_id`, `stake`, `chance_bp`, `max_payout` | place_bet |
| `settle` | `entry_id`, `roll`, `chance_bp`, `eff_chance_bp`, `mult_bp`, `payout`, `result`, `stake`, `addr` | Next block |
| `roll_audit` | entropy bytes | Settlement (audit trail) |

`result`: 1 = win, 2 = loss.

### Tests

```bash
go test ./tests/...                                 # smoke
go test -run TestLongRunPnL ./tests/... -timeout 30m  # 10M-round PnL prediction
```

</details>
