// Mines calculator — WASM protocol v3.
//
// 5x5 minefield. Player reveals tiles, avoids mines, cashes out.
//
// block_update receives 32-byte DKG seed directly.
// Two-list architecture: active bets (idle, have timeout deadline) and
// RNG-pending bets (waiting for seed to resolve reveal). block_update
// skips iteration when no RNG pending and no timeouts due — O(1) idle cost.
//
// Exports: alloc, dealloc, place_bet, bet_action, block_update, query, info, init_game
// Imports: env.kv_get, env.kv_set, env.kv_delete,
//          env.reserve, env.settle, env.get_bettor, env.emit_event
package main

import (
	"encoding/binary"
	"unsafe"
)

// ---------------------------------------------------------------------------
// Host imports
// ---------------------------------------------------------------------------

//go:wasmimport env kv_get
func kv_get(keyPtr, keyLen uint32) uint64

//go:wasmimport env kv_set
func kv_set(keyPtr, keyLen, valPtr, valLen uint32)

//go:wasmimport env kv_delete
func host_kv_delete(keyPtr, keyLen uint32)

//go:wasmimport env reserve
func host_reserve(betID, amount uint64) uint32

//go:wasmimport env settle
func host_settle(betID, payout uint64, kind uint32) uint32

//go:wasmimport env get_bettor
func host_get_bettor(betID uint64, outPtr uint32) uint32

//go:wasmimport env emit_event
func host_emit_event(topicPtr, topicLen, dataPtr, dataLen uint32)

//go:wasmimport env get_gas_budget
func get_gas_budget() uint64

//go:wasmimport env get_gas_used
func get_gas_used() uint64

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

//export alloc
func alloc(size uint32) *byte {
	if size == 0 {
		size = 1
	}
	buf := make([]byte, size)
	return &buf[0]
}

//export dealloc
func dealloc(ptr *byte, size uint32) {}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	boardSize   = 25
	maxMines    = 13
	minMines    = 1
	houseEdgeBP = 100
	maxReveals  = 5
	timeoutBlks = 40

	// Capacity caps — keep block_update gas usage bounded.
	//
	//  - maxBetsPerBlock bounds new place_bet rate (~80-100K WASM gas each;
	//    80 fits comfortably under the 10M per-block budget after accounting
	//    for the per-bet kvSet overhead on the bets-this-block counter and
	//    chunked active list append).
	//  - maxConcurrentBets bounds scanTimeouts cost. scanTimeouts walks the
	//    full active list each time it fires (~5K SDK gas per kvGet); 1000
	//    bets => ~5M gas, leaving margin under the 10M cap.
	//
	// To scale beyond these caps, deploy a second calculator on the same
	// bankroll. Both caps reject cleanly with status codes (no calc kill).
	maxBetsPerBlock   = 100
	maxConcurrentBets = 1000

	// Chunk size for the active-bet list. Each chunk is its own KV (≤100
	// betIDs = 800 bytes), so place_bet's append cost is bounded regardless
	// of how many bets are active. Without chunking, the flat list would
	// grow to 8000 bytes at 1000 active bets, making each new place_bet's
	// kvSet cost ~242K SDK gas — quickly exhausting the per-block budget.
	activeChunkSize = 100

	kindWin    uint32 = 1
	kindLoss   uint32 = 2
	kindRefund uint32 = 3

	phaseActive     byte = 0
	phaseWaitingRNG byte = 1

	maxAddrBuf = 64
)

// Bet layout (32 bytes):
//   [0..7]   bet_id        u64
//   [8..15]  stake         u64
//   [16]     mines_count   u8
//   [17]     revealed      u8
//   [18]     phase         u8
//   [19..22] board_mask    u32
//   [23]     pending_tile  u8
//   [24..31] timeout_at    u64 (absolute block counter deadline)
const betSize = 32

// KV keys.
//
// Active list is chunked: stored as a sequence of independent KV chunks
// (al_K) plus a count of how many chunks exist (an). This keeps append
// (place_bet) and remove (settle/timeout) costs bounded by chunk size,
// not total active bet count.
var (
	keyRNGList         = []byte("rl") // bet IDs in phaseWaitingRNG (flat; transient/small)
	keyBlockCount      = []byte("bc") // u64 block counter
	keyMinTimeout      = []byte("mt") // u64 earliest timeout_at (0 = none)
	keyBetsThisBlock   = []byte("bb") // u64 per-block place_bet counter (reset by block_update)
	keyActiveChunkCnt  = []byte("an") // u64 number of active-list chunks created
	keyActiveTotal     = []byte("at") // u64 cached total active-bet count (for O(1) cap check)
)

// activeChunkKeyBuf is reused to avoid heap allocation per call.
var activeChunkKeyBuf [3]byte

// activeChunkKey builds the KV key for chunk K of the active list.
func activeChunkKey(idx uint64) []byte {
	activeChunkKeyBuf[0] = 'a'
	activeChunkKeyBuf[1] = 'c'
	activeChunkKeyBuf[2] = byte(idx)
	return activeChunkKeyBuf[:]
}

// betKeyBuf is reused across calls to avoid heap allocation.
var betKeyBuf [9]byte

func betKey(id uint64) []byte {
	betKeyBuf[0] = 'b'
	binary.LittleEndian.PutUint64(betKeyBuf[1:], id)
	return betKeyBuf[:]
}

// ---------------------------------------------------------------------------
// Block counter + min-timeout helpers
// ---------------------------------------------------------------------------

func getBlockCount() uint64 {
	v := kvGetBytes(keyBlockCount)
	if v == nil || len(v) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(v)
}

func setBlockCount(bc uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], bc)
	kvSet(keyBlockCount, buf[:])
}

func getMinTimeout() uint64 {
	v := kvGetBytes(keyMinTimeout)
	if v == nil || len(v) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(v)
}

func setMinTimeout(mt uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], mt)
	kvSet(keyMinTimeout, buf[:])
}

func getBetsThisBlock() uint64 {
	v := kvGetBytes(keyBetsThisBlock)
	if v == nil || len(v) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(v)
}

func setBetsThisBlock(n uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], n)
	kvSet(keyBetsThisBlock, buf[:])
}

// ---------------------------------------------------------------------------
// Active list — chunked. Each chunk holds ≤activeChunkSize betIDs.
// Append/remove/iterate operations stay bounded per-call regardless of how
// many bets are active.
// ---------------------------------------------------------------------------

func getActiveChunkCount() uint64 {
	v := kvGetBytes(keyActiveChunkCnt)
	if v == nil || len(v) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(v)
}

func setActiveChunkCount(n uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], n)
	kvSet(keyActiveChunkCnt, buf[:])
}

// activeListSize returns the cached total number of active bets.
// O(1) — single 8-byte KV read. Updated by appendToActiveList /
// removeFromActiveList.
func activeListSize() uint64 {
	v := kvGetBytes(keyActiveTotal)
	if v == nil || len(v) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(v)
}

func setActiveTotal(n uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], n)
	kvSet(keyActiveTotal, buf[:])
}

// appendToActiveList adds betID to the last chunk, creating a new one if full.
// Maintains the cached total count.
func appendToActiveList(betID uint64) {
	count := getActiveChunkCount()
	if count == 0 {
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, betID)
		kvSet(activeChunkKey(0), buf)
		setActiveChunkCount(1)
		setActiveTotal(activeListSize() + 1)
		return
	}
	cur := count - 1
	chunk := kvGetBytes(activeChunkKey(cur))
	if uint64(len(chunk)/8) >= activeChunkSize {
		// Last chunk full — create a new one.
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, betID)
		kvSet(activeChunkKey(count), buf)
		setActiveChunkCount(count + 1)
		setActiveTotal(activeListSize() + 1)
		return
	}
	buf := make([]byte, len(chunk)+8)
	copy(buf, chunk)
	binary.LittleEndian.PutUint64(buf[len(chunk):], betID)
	kvSet(activeChunkKey(cur), buf)
	setActiveTotal(activeListSize() + 1)
}

// removeFromActiveList finds betID across chunks and removes it via
// swap-with-last-entry-of-last-chunk. If the last chunk becomes empty,
// the chunk count is decremented. Maintains the cached total count
// (decrements on successful removal only).
func removeFromActiveList(betID uint64) {
	count := getActiveChunkCount()
	if count == 0 {
		return
	}
	// Find which chunk holds the betID.
	var foundChunk uint64
	var foundOff int
	found := false
	for c := uint64(0); c < count; c++ {
		chunk := kvGetBytes(activeChunkKey(c))
		for off := 0; off+8 <= len(chunk); off += 8 {
			if binary.LittleEndian.Uint64(chunk[off:]) == betID {
				foundChunk = c
				foundOff = off
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return
	}
	// Locate last entry (in last chunk).
	lastChunkIdx := count - 1
	lastChunk := kvGetBytes(activeChunkKey(lastChunkIdx))
	lastOff := len(lastChunk) - 8
	if lastOff < 0 {
		// Defensive: empty last chunk; decrement count and retry.
		kvDelete(activeChunkKey(lastChunkIdx))
		setActiveChunkCount(lastChunkIdx)
		return
	}
	// Same chunk + same slot? Just truncate.
	if foundChunk == lastChunkIdx && foundOff == lastOff {
		if len(lastChunk) == 8 {
			kvDelete(activeChunkKey(lastChunkIdx))
			setActiveChunkCount(lastChunkIdx)
		} else {
			kvSet(activeChunkKey(lastChunkIdx), lastChunk[:lastOff])
		}
		decrementActiveTotal()
		return
	}
	lastID := binary.LittleEndian.Uint64(lastChunk[lastOff:])
	// Same chunk, different slot.
	if foundChunk == lastChunkIdx {
		copy(lastChunk[foundOff:foundOff+8], lastChunk[lastOff:lastOff+8])
		_ = lastID // (unused — overwrite via copy above)
		if len(lastChunk) == 8 {
			kvDelete(activeChunkKey(lastChunkIdx))
			setActiveChunkCount(lastChunkIdx)
		} else {
			kvSet(activeChunkKey(lastChunkIdx), lastChunk[:lastOff])
		}
		decrementActiveTotal()
		return
	}
	// Different chunks — write last entry into found slot, then truncate last chunk.
	foundChunkBytes := kvGetBytes(activeChunkKey(foundChunk))
	binary.LittleEndian.PutUint64(foundChunkBytes[foundOff:foundOff+8], lastID)
	kvSet(activeChunkKey(foundChunk), foundChunkBytes)
	if len(lastChunk) == 8 {
		kvDelete(activeChunkKey(lastChunkIdx))
		setActiveChunkCount(lastChunkIdx)
	} else {
		kvSet(activeChunkKey(lastChunkIdx), lastChunk[:lastOff])
	}
	decrementActiveTotal()
}

func decrementActiveTotal() {
	cur := activeListSize()
	if cur > 0 {
		setActiveTotal(cur - 1)
	}
}

// loadActiveList walks all chunks and returns a flat []uint64 of betIDs.
// Used by scanTimeouts. Memory cost = O(N), KV reads = O(chunks).
func loadActiveList() []uint64 {
	count := getActiveChunkCount()
	if count == 0 {
		return nil
	}
	var out []uint64
	for c := uint64(0); c < count; c++ {
		chunk := kvGetBytes(activeChunkKey(c))
		for off := 0; off+8 <= len(chunk); off += 8 {
			out = append(out, binary.LittleEndian.Uint64(chunk[off:]))
		}
	}
	return out
}

// updateMinTimeout sets min_timeout to the lesser of current and candidate.
func updateMinTimeout(candidate uint64) {
	cur := getMinTimeout()
	if cur == 0 || candidate < cur {
		setMinTimeout(candidate)
	}
}

// ---------------------------------------------------------------------------
// init_game — set up block counters
// ---------------------------------------------------------------------------

//export init_game
func init_game(sentinelID, bankrollID, calculatorID uint64) {
	setBlockCount(0)
	setMinTimeout(0)
}

// multTable holds fair-multiplier values (in basis points) for every
// (mines, reveals) combination. Indexed as multTable[mines-1][reveals-1].
// Hardcoded literal — generated once via the formula in /tmp/mult_table_gen.go.
// Lives in the WASM binary (~2.5KB) so lookups are O(1) and cost zero gas.
// Cells beyond the safe-tile count for a given mines count are 0 (unused).
var multTable = [maxMines][boardSize - 1]uint64{
	{ // mines=1, safe=24
		10416, 10869, 11363, 11904, 12500, 13157,
		13888, 14705, 15625, 16666, 17857, 19230,
		20833, 22727, 25000, 27777, 31250, 35714,
		41666, 50000, 62500, 83333, 125000, 250000,
	},
	{ // mines=2, safe=23
		10869, 11857, 12987, 14285, 15789, 17543,
		19607, 22058, 25000, 28571, 32967, 38461,
		45454, 54545, 66666, 83333, 107142, 142857,
		200000, 300000, 500000, 1000000, 3000000, 0,
	},
	{ // mines=3, safe=22
		11363, 12987, 14935, 17293, 20175, 23735,
		28186, 33823, 41071, 50549, 63186, 80419,
		104545, 139393, 191666, 273809, 410714, 657142,
		1150000, 2300000, 5750000, 23000000, 0, 0,
	},
	{ // mines=4, safe=21
		11904, 14285, 17293, 21136, 26109, 32636,
		41339, 53151, 69505, 92673, 126373, 176923,
		255555, 383333, 602380, 1003968, 1807142, 3614285,
		8433333, 25300000, 126500000, 0, 0, 0,
	},
	{ // mines=5, safe=20
		12500, 15789, 20175, 26109, 34268, 45691,
		62009, 85859, 121634, 176923, 265384, 412820,
		670833, 1150000, 2108333, 4216666, 9487500, 25300000,
		88550000, 531300000, 0, 0, 0, 0,
	},
	{ // mines=6, safe=19
		13157, 17543, 23735, 32636, 45691, 65273,
		95399, 143099, 221153, 353846, 589743, 1032051,
		1916666, 3833333, 8433333, 21083333, 63250000, 253000000,
		1771000000, 0, 0, 0, 0, 0,
	},
	{ // mines=7, safe=18
		13888, 19607, 28186, 41339, 62009, 95399,
		151049, 247171, 420192, 747008, 1400641, 2801282,
		6069444, 14566666, 40058333, 133527777, 600875000, 4807000000,
		0, 0, 0, 0, 0, 0,
	},
	{ // mines=8, safe=17
		14705, 22058, 33823, 53151, 85859, 143099,
		247171, 444909, 840384, 1680769, 3601648, 8403846,
		21850000, 65550000, 240350000, 1201750000, 10815750000, 0,
		0, 0, 0, 0, 0, 0,
	},
	{ // mines=9, safe=16
		15625, 25000, 41071, 69505, 121634, 221153,
		420192, 840384, 1785817, 4081868, 10204670, 28573076,
		92862500, 371450000, 2042975000, 20429750000, 0, 0,
		0, 0, 0, 0, 0, 0,
	},
	{ // mines=10, safe=15
		16666, 28571, 50549, 92673, 176923, 353846,
		747008, 1680769, 4081868, 10884981, 32654945, 114292307,
		495266666, 2971600000, 32687600000, 0, 0, 0,
		0, 0, 0, 0, 0, 0,
	},
	{ // mines=11, safe=14
		17857, 32967, 63186, 126373, 265384, 589743,
		1400641, 3601648, 10204670, 32654945, 122456043, 571461538,
		3714500000, 44574000000, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0,
	},
	{ // mines=12, safe=13
		19230, 38461, 80419, 176923, 412820, 1032051,
		2801282, 8403846, 28573076, 114292307, 571461538, 4000230769,
		52003000000, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0,
	},
	{ // mines=13, safe=12
		20833, 45454, 104545, 255555, 670833, 1916666,
		6069444, 21850000, 92862500, 495266666, 3714500000, 52003000000,
		0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0,
	},
}

// getFairMultBP returns the fair multiplier in bp for (mines-1, reveals-1).
// O(1) array lookup — no KV ops, no SDK gas.
func getFairMultBP(minesIdx, revealsIdx uint64) uint64 {
	if minesIdx >= maxMines || revealsIdx >= boardSize-1 {
		return 0
	}
	return multTable[minesIdx][revealsIdx]
}

// ---------------------------------------------------------------------------
// place_bet
// ---------------------------------------------------------------------------

//export place_bet
func place_bet(betID, bankrollID, calculatorID, stake uint64, paramsPtr, paramsLen uint32) uint32 {
	params := ptrToSlice(paramsPtr, paramsLen)
	if len(params) < 21 {
		return 11
	}
	minesCount := params[20]
	if minesCount < minMines || minesCount > maxMines {
		return 12
	}

	// Concurrent active-bet cap — bounds scanTimeouts cost in block_update.
	rngListBytes := kvGetBytes(keyRNGList)
	concurrentBets := activeListSize() + uint64(len(rngListBytes)/8)
	if concurrentBets >= maxConcurrentBets {
		return 13
	}
	// Per-block place_bet cap — bounds new-bet WASM gas in this block.
	if getBetsThisBlock() >= maxBetsPerBlock {
		return 14
	}

	safe := uint64(boardSize) - uint64(minesCount)
	reveals := uint64(maxReveals)
	if reveals > safe {
		reveals = safe
	}
	maxFairBP := getFairMultBP(uint64(minesCount-1), reveals-1)
	maxEdgedBP := maxFairBP * (10000 - houseEdgeBP) / 10000
	maxPayout := mulDiv(stake, maxEdgedBP, 10000)

	if host_reserve(betID, maxPayout) != 0 {
		return 3
	}

	bc := getBlockCount()
	deadline := bc + timeoutBlks

	bet := make([]byte, betSize)
	binary.LittleEndian.PutUint64(bet[0:], betID)
	binary.LittleEndian.PutUint64(bet[8:], stake)
	bet[16] = minesCount
	bet[17] = 0
	bet[18] = phaseActive
	binary.LittleEndian.PutUint64(bet[24:], deadline)
	kvSet(betKey(betID), bet)

	appendToActiveList(betID)
	updateMinTimeout(deadline)
	setBetsThisBlock(getBetsThisBlock() + 1)

	addr := getBettorAddr(betID)
	emitJSON("joined", "bet_id", betID, "addr", addr, "stake", stake, "mines", uint64(minesCount))
	return 0
}

// ---------------------------------------------------------------------------
// bet_action
// ---------------------------------------------------------------------------

//export bet_action
func bet_action(betID uint64, actionPtr, actionLen uint32) uint32 {
	action := ptrToSlice(actionPtr, actionLen)
	if len(action) < 1 {
		return 1
	}
	switch action[0] {
	case 1:
		return handleReveal(betID, action[1:])
	case 2:
		return handleCashout(betID)
	default:
		return 2
	}
}

func handleReveal(betID uint64, payload []byte) uint32 {
	bet := kvGetBytes(betKey(betID))
	if bet == nil || len(bet) < betSize {
		return 30
	}
	if bet[18] != phaseActive {
		return 31
	}
	if len(payload) < 1 {
		return 33
	}
	tile := payload[0]
	if tile >= boardSize {
		return 34
	}
	boardMask := binary.LittleEndian.Uint32(bet[19:23])
	if boardMask&(1<<tile) != 0 {
		return 35
	}

	bet[18] = phaseWaitingRNG
	bet[23] = tile
	kvSet(betKey(betID), bet)

	// Move from active list to RNG list.
	removeFromActiveList(betID)
	appendToList(keyRNGList, betID)
	return 0
}

func handleCashout(betID uint64) uint32 {
	bet := kvGetBytes(betKey(betID))
	if bet == nil || len(bet) < betSize {
		return 40
	}
	if bet[18] != phaseActive {
		return 41
	}
	revealed := bet[17]
	if revealed == 0 {
		return 43
	}

	minesCount := bet[16]
	stake := binary.LittleEndian.Uint64(bet[8:16])
	currentMultBP := getFairMultBP(uint64(minesCount-1), uint64(revealed-1))
	edgedMultBP := currentMultBP * (10000 - houseEdgeBP) / 10000
	payout := mulDiv(stake, edgedMultBP, 10000)

	addr := getBettorAddr(betID)
	host_settle(betID, payout, kindWin)
	emitJSON("settled", "bet_id", betID, "addr", addr, "payout", payout, "kind", uint64(kindWin), "reason", "cashout", "stake", stake, "mines", uint64(minesCount), "revealed", uint64(revealed), "mult_bp", edgedMultBP)
	kvDelete(betKey(betID))
	removeFromActiveList(betID)
	return 0
}

// ---------------------------------------------------------------------------
// block_update — fast-path skip, two-list processing
// ---------------------------------------------------------------------------

//export block_update
func block_update(seedPtr uint32) {
	bc := getBlockCount() + 1
	setBlockCount(bc)
	// Reset per-block place_bet counter. All place_bet calls inside this
	// block share this counter; capped at maxBetsPerBlock above.
	setBetsThisBlock(0)

	// 1. Resolve RNG-pending bets.
	rngList := loadList(keyRNGList)
	if len(rngList) > 0 && seedPtr != 0 {
		seed := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(seedPtr))), 32)
		// Process pending — resolving may add bets back to active list.
		budget := get_gas_budget()
		var unresolved []uint64
		for _, betID := range rngList {
			if get_gas_used()+80_000 > budget {
				unresolved = append(unresolved, betID)
				continue
			}
			resolveReveal(betID, seed, bc)
		}
		// Rewrite RNG list with any unresolved bets.
		if len(unresolved) == 0 {
			kvDelete(keyRNGList)
		} else {
			buf := make([]byte, len(unresolved)*8)
			for i, id := range unresolved {
				binary.LittleEndian.PutUint64(buf[i*8:], id)
			}
			kvSet(keyRNGList, buf)
		}
	}

	// 2. Check timeouts — only scan if earliest deadline reached.
	mt := getMinTimeout()
	if mt > 0 && bc >= mt {
		scanTimeouts(bc)
	}
}

func scanTimeouts(bc uint64) {
	list := loadActiveList()
	if len(list) == 0 {
		setMinTimeout(0)
		return
	}

	budget := get_gas_budget()
	var newMin uint64
	timedOut := make([]uint64, 0, 8)

	for _, betID := range list {
		bet := kvGetBytes(betKey(betID))
		if bet == nil || len(bet) < betSize {
			timedOut = append(timedOut, betID)
			continue
		}
		deadline := binary.LittleEndian.Uint64(bet[24:32])
		if bc >= deadline {
			if get_gas_used()+80_000 > budget {
				// Can't process this timeout — leave for next block.
				if newMin == 0 || deadline < newMin {
					newMin = deadline
				}
				continue
			}
			timedOut = append(timedOut, betID)
			handleTimeout(betID, bet)
		} else {
			if newMin == 0 || deadline < newMin {
				newMin = deadline
			}
		}
	}

	// Remove timed-out bets from active list.
	for _, betID := range timedOut {
		removeFromActiveList(betID)
	}
	setMinTimeout(newMin)
}

func resolveReveal(betID uint64, seed []byte, bc uint64) {
	bet := kvGetBytes(betKey(betID))
	if bet == nil || len(bet) < betSize {
		return
	}

	stake := binary.LittleEndian.Uint64(bet[8:16])
	minesCount := bet[16]
	revealed := bet[17]
	tile := bet[23]

	safe := uint64(boardSize) - uint64(minesCount)
	effectiveMax := uint64(maxReveals)
	if effectiveMax > safe {
		effectiveMax = safe
	}

	// Determine mine/safe via inline sha256. The reveal_audit emit below
	// touches the hash bytes; that secondary read defeats a TinyGo codegen
	// quirk that otherwise produced non-deterministic hashes for some
	// (seed, bet_id) pairs. Removing the emit may re-trigger the bug.
	remaining := uint64(boardSize) - uint64(revealed)
	var betBuf [8]byte
	binary.BigEndian.PutUint64(betBuf[:], betID)
	entropy := make([]byte, len(seed)+8)
	copy(entropy, seed)
	copy(entropy[len(seed):], betBuf[:])
	h := sha256sum(entropy)
	rngVal := binary.BigEndian.Uint64(h[0:8]) % remaining
	isMine := rngVal < uint64(minesCount)

	addr := getBettorAddr(betID)
	// reveal_audit: per-reveal RNG audit trail. Players can verify the chain
	// computed mine/safe correctly by re-deriving sha256(seed||bet_id) and
	// applying the same modulo. The wide field set is also a defensive
	// workaround against a TinyGo WASM codegen quirk that produced
	// non-deterministic hashes when the rng-derivation code stood alone —
	// touching the hash bytes via emit forces the compiler to keep them.
	emitJSON("reveal_audit",
		"bet_id", betID,
		"rng", rngVal,
		"remaining", remaining,
		"mines", uint64(minesCount),
		"revealed", uint64(revealed),
		"tile", uint64(tile),
		"e0", uint64(entropy[0]),
		"e1", uint64(entropy[1]),
		"e2", uint64(entropy[2]),
		"e30", uint64(entropy[30]),
		"e31", uint64(entropy[31]),
		"e32", uint64(entropy[32]),
		"e33", uint64(entropy[33]),
		"e38", uint64(entropy[38]),
		"e39", uint64(entropy[39]),
	)

	if isMine {
		host_settle(betID, 0, kindLoss)
		emitJSON("reveal", "bet_id", betID, "addr", addr, "tile", uint64(tile), "safe", uint64(0), "revealed", uint64(revealed), "mult_bp", uint64(0), "payout", uint64(0))
		emitJSON("settled", "bet_id", betID, "addr", addr, "payout", uint64(0), "kind", uint64(kindLoss), "reason", "mine", "stake", stake, "mines", uint64(minesCount), "revealed", uint64(revealed), "mult_bp", uint64(0))
		kvDelete(betKey(betID))
		return
	}

	// Safe tile.
	revealed++
	boardMask := binary.LittleEndian.Uint32(bet[19:23])
	boardMask |= 1 << tile
	bet[17] = revealed
	binary.LittleEndian.PutUint32(bet[19:23], boardMask)
	bet[18] = phaseActive

	currentMultBP := getFairMultBP(uint64(minesCount-1), uint64(revealed-1))
	edgedMultBP := currentMultBP * (10000 - houseEdgeBP) / 10000
	payout := mulDiv(stake, edgedMultBP, 10000)

	emitJSON("reveal", "bet_id", betID, "addr", addr, "tile", uint64(tile), "safe", uint64(1), "revealed", uint64(revealed), "mult_bp", edgedMultBP, "payout", payout)

	// Auto-cashout at max reveals.
	if uint64(revealed) >= effectiveMax {
		host_settle(betID, payout, kindWin)
		emitJSON("settled", "bet_id", betID, "addr", addr, "payout", payout, "kind", uint64(kindWin), "reason", "max_reveals", "stake", stake, "mines", uint64(minesCount), "revealed", uint64(revealed), "mult_bp", edgedMultBP)
		kvDelete(betKey(betID))
		return
	}

	// Reset timeout and move back to active list.
	deadline := bc + timeoutBlks
	binary.LittleEndian.PutUint64(bet[24:], deadline)
	kvSet(betKey(betID), bet)
	appendToActiveList(betID)
	updateMinTimeout(deadline)
}

func handleTimeout(betID uint64, bet []byte) {
	revealed := bet[17]
	addr := getBettorAddr(betID)

	if revealed == 0 {
		stake := binary.LittleEndian.Uint64(bet[8:16])
		host_settle(betID, stake, kindRefund)
		minesCount := bet[16]
		emitJSON("settled", "bet_id", betID, "addr", addr, "payout", stake, "kind", uint64(kindRefund), "reason", "timeout_refund", "stake", stake, "mines", uint64(minesCount), "revealed", uint64(0), "mult_bp", uint64(10000))
		kvDelete(betKey(betID))
		return
	}

	minesCount := bet[16]
	stake := binary.LittleEndian.Uint64(bet[8:16])
	currentMultBP := getFairMultBP(uint64(minesCount-1), uint64(revealed-1))
	edgedMultBP := currentMultBP * (10000 - houseEdgeBP) / 10000
	payout := mulDiv(stake, edgedMultBP, 10000)

	host_settle(betID, payout, kindWin)
	emitJSON("settled", "bet_id", betID, "addr", addr, "payout", payout, "kind", uint64(kindWin), "reason", "timeout_cashout", "stake", stake, "mines", uint64(minesCount), "revealed", uint64(revealed), "mult_bp", edgedMultBP)
	kvDelete(betKey(betID))
}

// ---------------------------------------------------------------------------
// List helpers — swap-with-last removal (no allocation)
// ---------------------------------------------------------------------------

func appendToList(key []byte, betID uint64) {
	list := kvGetBytes(key)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], betID)
	if list == nil {
		kvSet(key, buf[:])
	} else {
		newList := make([]byte, len(list)+8)
		copy(newList, list)
		copy(newList[len(list):], buf[:])
		kvSet(key, newList)
	}
}

func loadList(key []byte) []uint64 {
	list := kvGetBytes(key)
	if list == nil {
		return nil
	}
	n := len(list) / 8
	ids := make([]uint64, n)
	for i := 0; i < n; i++ {
		ids[i] = binary.LittleEndian.Uint64(list[i*8:])
	}
	return ids
}

func removeFromList(key []byte, betID uint64) {
	list := kvGetBytes(key)
	if list == nil {
		return
	}
	n := len(list)
	for i := 0; i+8 <= n; i += 8 {
		if binary.LittleEndian.Uint64(list[i:]) == betID {
			last := n - 8
			if i != last {
				copy(list[i:i+8], list[last:last+8])
			}
			if last == 0 {
				kvDelete(key)
			} else {
				kvSet(key, list[:last])
			}
			return
		}
	}
}

func listLen(key []byte) int {
	list := kvGetBytes(key)
	if list == nil {
		return 0
	}
	return len(list) / 8
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func getBettorAddr(betID uint64) string {
	buf := make([]byte, maxAddrBuf)
	n := host_get_bettor(betID, uint32(uintptr(unsafe.Pointer(&buf[0]))))
	if n == 0 || n > maxAddrBuf {
		return ""
	}
	return string(buf[:n])
}

func ptrToSlice(ptr, length uint32) []byte {
	if length == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
}

// ---------------------------------------------------------------------------
// query
// ---------------------------------------------------------------------------

//export query
func query() *byte {
	data := []byte(`{}`)
	result := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(result[0:4], uint32(len(data)))
	copy(result[4:], data)
	return &result[0]
}

// ---------------------------------------------------------------------------
// info
// ---------------------------------------------------------------------------

//export info
func info() *byte {
	data := []byte(`{
		"name":"Mines-firstexo",
		"engine":"mines",
		"mode":"v3",
		"house_edge_bp":100,
		"developer":"ExoHash",
		"description":"5x5 minefield — reveal tiles, avoid mines, cashout anytime",
		"errors":{
			"place_bet":{
				"3":"Insufficient bankroll liquidity",
				"11":"Invalid parameters — expected sender(20) + mines_count(1)",
				"12":"Mines count out of range — must be between 1 and 13",
				"13":"Game full — max 1000 concurrent active bets, retry later",
				"14":"Block full — max 100 bets per block, retry next block"
			},
			"bet_action":{
				"1":"Invalid action format",
				"2":"Unknown action type — use 1 (reveal) or 2 (cashout)",
				"30":"No active bet found",
				"31":"Bet not in active phase — waiting for RNG",
				"33":"Missing tile index in reveal action",
				"34":"Tile index out of range — must be 0 to 24",
				"35":"Tile already revealed",
				"40":"No active session found",
				"41":"Session not in active phase",
				"43":"Must reveal at least 1 tile before cashing out"
			}
		}
	}`)
	result := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(result[0:4], uint32(len(data)))
	copy(result[4:], data)
	return &result[0]
}

// ---------------------------------------------------------------------------
// KV helpers
// ---------------------------------------------------------------------------

func kvGetBytes(key []byte) []byte {
	if len(key) == 0 {
		return nil
	}
	packed := kv_get(uint32(uintptr(unsafe.Pointer(&key[0]))), uint32(len(key)))
	if packed == 0 {
		return nil
	}
	ptr := uint32(packed >> 32)
	length := uint32(packed & 0xFFFFFFFF)
	if length == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
}

func kvSet(key, value []byte) {
	if len(key) == 0 || len(value) == 0 {
		return
	}
	kv_set(
		uint32(uintptr(unsafe.Pointer(&key[0]))), uint32(len(key)),
		uint32(uintptr(unsafe.Pointer(&value[0]))), uint32(len(value)),
	)
}

func kvDelete(key []byte) {
	if len(key) == 0 {
		return
	}
	host_kv_delete(uint32(uintptr(unsafe.Pointer(&key[0]))), uint32(len(key)))
}

// ---------------------------------------------------------------------------
// Math
// ---------------------------------------------------------------------------

func mulDiv(a, b, c uint64) uint64 {
	if c == 0 {
		return 0
	}
	hi, lo := mul64(a, b)
	if hi == 0 {
		return lo / c
	}
	return div128(hi, lo, c)
}

func mul64(a, b uint64) (uint64, uint64) {
	aH, aL := a>>32, a&0xFFFFFFFF
	bH, bL := b>>32, b&0xFFFFFFFF
	lo := aL * bL
	mid := aH*bL + aL*bH
	hi := aH * bH
	lo2 := lo + (mid << 32)
	if lo2 < lo {
		hi++
	}
	hi += mid >> 32
	return hi, lo2
}

func div128(hi, lo, d uint64) uint64 {
	if d == 0 {
		return 0
	}
	q := hi / d
	r := hi % d
	return (q << 32) + (((r << 32) | (lo >> 32)) / d)
}

// ---------------------------------------------------------------------------
// Event helpers
// ---------------------------------------------------------------------------

func emitJSON(topic string, pairs ...interface{}) {
	json := fmtJSON(pairs...)
	topicBytes := []byte(topic)
	jsonBytes := []byte(json)
	host_emit_event(
		uint32(uintptr(unsafe.Pointer(&topicBytes[0]))), uint32(len(topicBytes)),
		uint32(uintptr(unsafe.Pointer(&jsonBytes[0]))), uint32(len(jsonBytes)),
	)
}

func fmtJSON(pairs ...interface{}) string {
	buf := make([]byte, 0, 256)
	buf = append(buf, '{')
	for i := 0; i < len(pairs)-1; i += 2 {
		if i > 0 {
			buf = append(buf, ',')
		}
		key := pairs[i].(string)
		buf = append(buf, '"')
		buf = append(buf, key...)
		buf = append(buf, '"', ':')
		switch v := pairs[i+1].(type) {
		case uint64:
			buf = appendUint(buf, v)
		case string:
			buf = append(buf, '"')
			buf = append(buf, v...)
			buf = append(buf, '"')
		}
	}
	buf = append(buf, '}')
	return string(buf)
}

func appendUint(buf []byte, v uint64) []byte {
	if v == 0 {
		return append(buf, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	return append(buf, tmp[i:]...)
}

// ---------------------------------------------------------------------------
// SHA-256 (inline — TinyGo's crypto/sha256 panics in WASM)
// ---------------------------------------------------------------------------

var sha256K = [64]uint32{
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
}

func sha256sum(data []byte) [32]byte {
	h0 := uint32(0x6a09e667)
	h1 := uint32(0xbb67ae85)
	h2 := uint32(0x3c6ef372)
	h3 := uint32(0xa54ff53a)
	h4 := uint32(0x510e527f)
	h5 := uint32(0x9b05688c)
	h6 := uint32(0x1f83d9ab)
	h7 := uint32(0x5be0cd19)
	msgLen := len(data)
	bitLen := uint64(msgLen) * 8
	padLen := 64 - ((msgLen + 9) % 64)
	if padLen == 64 {
		padLen = 0
	}
	totalLen := msgLen + 1 + padLen + 8
	buf := make([]byte, totalLen)
	copy(buf, data)
	buf[msgLen] = 0x80
	binary.BigEndian.PutUint64(buf[totalLen-8:], bitLen)

	var w [64]uint32
	for off := 0; off < len(buf); off += 64 {
		block := buf[off : off+64]
		for i := 0; i < 16; i++ {
			w[i] = binary.BigEndian.Uint32(block[i*4:])
		}
		for i := 16; i < 64; i++ {
			s0 := rotr32(w[i-15], 7) ^ rotr32(w[i-15], 18) ^ (w[i-15] >> 3)
			s1 := rotr32(w[i-2], 17) ^ rotr32(w[i-2], 19) ^ (w[i-2] >> 10)
			w[i] = w[i-16] + s0 + w[i-7] + s1
		}
		a, b, c, d, e, f, g, h := h0, h1, h2, h3, h4, h5, h6, h7
		for i := 0; i < 64; i++ {
			S1 := rotr32(e, 6) ^ rotr32(e, 11) ^ rotr32(e, 25)
			ch := (e & f) ^ (^e & g)
			temp1 := h + S1 + ch + sha256K[i] + w[i]
			S0 := rotr32(a, 2) ^ rotr32(a, 13) ^ rotr32(a, 22)
			maj := (a & b) ^ (a & c) ^ (b & c)
			temp2 := S0 + maj
			h = g; g = f; f = e; e = d + temp1; d = c; c = b; b = a; a = temp1 + temp2
		}
		h0 += a; h1 += b; h2 += c; h3 += d; h4 += e; h5 += f; h6 += g; h7 += h
	}
	var out [32]byte
	binary.BigEndian.PutUint32(out[0:], h0)
	binary.BigEndian.PutUint32(out[4:], h1)
	binary.BigEndian.PutUint32(out[8:], h2)
	binary.BigEndian.PutUint32(out[12:], h3)
	binary.BigEndian.PutUint32(out[16:], h4)
	binary.BigEndian.PutUint32(out[20:], h5)
	binary.BigEndian.PutUint32(out[24:], h6)
	binary.BigEndian.PutUint32(out[28:], h7)
	return out
}

func rotr32(x uint32, n uint) uint32 { return (x >> n) | (x << (32 - n)) }

func main() {}
