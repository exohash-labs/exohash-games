# ExoHash Games

Open-source casino games for [ExoHash](https://exohash.io). The exact code that runs onchain — anyone can rebuild it, hash it, and verify.

---

## Why this matters

- **The casino can't cheat.** Every bet's outcome is computed by code anyone can read.
- **The blockchain enforces it.** Stake is escrowed on a smart calculator, payout is automatic, no operator override.
- **You can prove it yourself.** Build the WASM, compare its SHA-256 to the on-chain calculator hash. If they match, the game running onchain is *exactly* this source — nothing more, nothing less.

No proprietary RNG. No off-chain "fair" servers. Just deterministic WASM, signed validator randomness, and an audit trail.

---

## The games

| Game  | Pattern                | House edge | Max bet/block | Notes |
|-------|------------------------|:----------:|:-------------:|-------|
| [Crash](crash/) | Multiplayer session    | 1% | 100 | Up to 100 players per round, autocashout 1.10x–100x |
| [Mines](mines/) | Solo multi-step        | 1% | 100 | 5×5 board, 1–13 mines, max 5 reveals per round |
| [Dice](dice/)   | Solo instant settle    | 1% | 100 | Pick over/under threshold, win chance 1%–98%  |

RTP is 99% across all three games. Edge is fixed in code, not adjustable per round, per player, or by the operator.

---

## Verify the build matches the chain

The chain stores the SHA-256 of every deployed calculator. To prove the onchain game is this exact source:

```bash
git clone https://github.com/exohash-labs/exohash-games
cd exohash-games
for g in crash mines dice; do
  (cd $g && tinygo build -o $g.wasm -target=wasi -opt=z -no-debug ./src)
done
sha256sum crash/crash.wasm mines/mines.wasm dice/dice.wasm
```

Then on the [ExoScan explorer](https://devnet.exohash.io), open any calculator and copy its WASM hash. If the hashes match, you are looking at the same bytes.

Current shipped hashes (TinyGo 0.40.1, WASI):

```
crash.wasm   687ba794fc8aad37a64bd6226adf2a47f10e5dea8b62b1bd276725d3df3e1e25
mines.wasm   8801762827addfa851c8910090376861574e300607b2b217f4efdc3eadcfb3ac
dice.wasm    c569f2ea7e92d53ddc2086d11167b30f8a7e8e57e1f0ca77f5ff9e21ed6aa38b
```

---

## Operator-proof guarantees

Things the operator **cannot** do, because the rules live in WASM and run inside consensus:

- Change the house edge.
- Increase max bet or stake limits beyond what is in the source.
- Refuse to pay a winning bet.
- Re-roll an unfavorable outcome.
- Inspect or front-run a pending bet (RNG comes from the validator-signed beacon, not the operator).
- Run more than the per-block gas budget — calculators that exceed limits are killed, not silently throttled.

---

## For developers

<details>
<summary>Repo layout</summary>

```
crash/
  src/main.go         WASM source (TinyGo)
  crash.wasm          Compiled binary deployed onchain
  tests/
    smoke_test.go     Functional tests
    longrun_pnl_test.go   Deterministic PnL prediction (millions of rounds)
mines/                ditto
dice/                 ditto
```

</details>

<details>
<summary>WASM ABI</summary>

Every calculator exports four functions:

| Export | Purpose |
|--------|---------|
| `place_bet(params)` | Validate stake + game-specific params, escrow, store bet. |
| `bet_action(action)` | Mid-round actions (cashout, reveal, etc). Pure noop for instant games. |
| `block_update(seed)` | Called every block by the chain with 32-byte beacon randomness. Settles bets, advances rounds. |
| `info()` | Returns calculator metadata (name, edge, version). |

Host-imported functions: `kv_get/set/delete`, `host_settle`, `emit_event`. See [DevKit](https://github.com/exohash-labs/exohash-devkit) for full ABI.

</details>

<details>
<summary>Build & test</summary>

```bash
cd crash
tinygo build -o crash.wasm -target=wasi -opt=z -no-debug ./src   # build
go test ./tests/...                                              # smoke
go test -run TestLongRunPnL ./tests/... -timeout 30m             # PnL prediction
```

</details>

<details>
<summary>Adding a new game</summary>

Use any of the three games as a template. Patterns covered:

- **Solo instant** (dice) — bet → settle next block.
- **Solo multi-step** (mines) — bet → multiple actions → cashout.
- **Multiplayer session** (crash) — N players → shared round state → per-player settle.

The WASM ABI, event protocol, and gas budget are documented in the [ExoHash DevKit](https://github.com/exohash-labs/exohash-devkit).

</details>

---

## License

MIT.
