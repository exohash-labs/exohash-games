# ExoHash Games

Canonical source for the three reference games that ship on the [ExoHash](https://exohash.io) protocol — **dice**, **crash**, and **mines**. This is the repo that ExoScan's verification panel links to.

## What's here

```
dice/
  src/main.go       Game source code (TinyGo → WASM)
  dice.wasm         Compiled bytecode deployed on-chain
  README.md         Game description + latest test report

crash/              ditto — multiplayer session pattern
mines/              ditto — multi-step reveal pattern
```

## Verifying a deployed game

Every calculator on-chain carries a WASM hash and a source URL. To verify a deployment matches this repo:

```bash
# 1. Grab the on-chain hash from ExoScan (or the chain itself).
# 2. Rebuild the WASM locally with TinyGo:

cd dice/src
tinygo build -o ../dice-verify.wasm -target=wasi -no-debug -opt=2 .

# 3. Hash both and compare:
sha256sum ../dice.wasm ../dice-verify.wasm
```

Matching hashes → the on-chain game is exactly this source.

## House edges (as shipped)

| Game  | House edge | Pattern |
|-------|:----------:|---------|
| dice  | 1.00%      | solo instant — bet, roll, settle next block |
| mines | 1.00%      | solo multi-step — reveal tiles, cashout |
| crash | 1.00%      | multiplayer session — rising multiplier, multi-player cashouts |

Per-game READMEs ([dice](dice/README.md) · [crash](crash/README.md) · [mines](mines/README.md)) contain full test reports — house edge convergence over 100k simulated bets, error semantics, gas accounting.

## Building a new game

These three games are templates — the WASM ABI, event protocol, and game patterns are documented in the [ExoHash DevKit](https://github.com/exohash-labs/exohash-devkit).

## License

MIT
