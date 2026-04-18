# Mines

Reveal safe tiles to grow your multiplier. Hit a mine, lose everything. Cash out any time before that.

## How to play

1. Place a bet. Pick how many mines hide on the 5×5 board (1 to 13).
2. Click tiles. Each safe reveal raises your multiplier.
3. After **5 safe reveals**, the round auto-cashes at your current multiplier — you can't keep going.
4. Cash out earlier any time you want. Hit a mine, you lose your stake.

Solo game — your bet is independent of other players.

## The math

| Property | Value |
|---|---|
| RTP | 99% |
| House edge | 1% |
| Board | 5×5 (25 tiles) |
| Mines per round | 1 to 13 |
| Max reveals per round | 5 |
| Multiplier table | Hardcoded in source — `multTable[mines][reveals]` |
| Max bet/block | 100 |
| Max concurrent bets | 1000 |

The multiplier curve is `safe / remaining_safe`, edge-adjusted by 1%. Higher mine count = bigger jumps per reveal but lower survival odds.

## Provable fairness

- **RNG source**: validator BLS-DKG randomness beacon (one fresh 32-byte seed per block).
- **Mixed with**: bet id + calculator id (per-bet, per-instance de-correlation).
- **Mine layout**: derived from RNG at first reveal — committed *before* the player picks a tile.
- **Source**: [`src/main.go`](src/main.go).
- **Build**:
  ```bash
  tinygo build -o mines.wasm -target=wasi -opt=z -no-debug ./src
  ```
- **Verify**: `sha256sum mines.wasm` must match the deployed calculator's hash on [ExoScan](https://devnet.exohash.io).

Current shipped hash: `8801762827addfa851c8910090376861574e300607b2b217f4efdc3eadcfb3ac`

Every reveal also emits a `reveal_audit` event containing the entropy bytes used — third parties can replay the RNG and verify each tile placement.

---

<details>
<summary>Developer reference</summary>

### Place bet params

After the 20-byte sender prefix:

| Offset | Size | Field |
|---|---|---|
| 20 | u8 | `mines_count` (1..13) |

### Bet actions

| Bytes | Meaning |
|---|---|
| `[1, tile]` | Reveal tile (0..24) |
| `[2]` | Cashout |

### Events

| Topic | Fields | When |
|---|---|---|
| `joined` | `bet_id`, `addr`, `stake`, `mines` | place_bet |
| `reveal` | `bet_id`, `addr`, `tile`, `safe`, `revealed`, `mult_bp`, `payout` | Each reveal |
| `reveal_audit` | entropy bytes | Each reveal (audit trail) |
| `settled` | `bet_id`, `addr`, `payout`, `kind`, `reason`, `stake`, `mines`, `revealed`, `mult_bp` | Cashout, mine hit, or auto-cashout at 5 reveals |

`kind`: 1 = win, 2 = loss. `reason`: `cashout`, `mine`, `auto_cashout`.

### Tests

```bash
go test ./tests/...                                 # smoke
go test -run TestLongRunPnL ./tests/... -timeout 30m  # 10M-round PnL prediction
```

</details>
