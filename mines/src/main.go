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
var (
	keyActiveList = []byte("al") // bet IDs in phaseActive
	keyRNGList    = []byte("rl") // bet IDs in phaseWaitingRNG
	keyBlockCount = []byte("bc") // u64 block counter
	keyMinTimeout = []byte("mt") // u64 earliest timeout_at (0 = none)
)

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

// updateMinTimeout sets min_timeout to the lesser of current and candidate.
func updateMinTimeout(candidate uint64) {
	cur := getMinTimeout()
	if cur == 0 || candidate < cur {
		setMinTimeout(candidate)
	}
}

// ---------------------------------------------------------------------------
// init_game — precompute multiplier table
// ---------------------------------------------------------------------------

//export init_game
func init_game(sentinelID, bankrollID, calculatorID uint64) {
	tableSize := maxMines * (boardSize - 1) * 8
	table := make([]byte, tableSize)
	for m := uint64(1); m <= maxMines; m++ {
		safe := uint64(boardSize) - m
		num := uint64(1)
		den := uint64(1)
		for k := uint64(1); k <= safe && k < boardSize; k++ {
			num *= (uint64(boardSize) - k + 1)
			den *= (safe - k + 1)
			g := gcd(num, den)
			num /= g
			den /= g
			val := num * 10000 / den
			off := ((m - 1) * (boardSize - 1) * 8) + ((k - 1) * 8)
			binary.LittleEndian.PutUint64(table[off:], val)
		}
	}
	kvSet([]byte("mult_table"), table)
	setBlockCount(0)
	setMinTimeout(0)
}

func getFairMultBP(minesIdx, revealsIdx uint64) uint64 {
	table := kvGetBytes([]byte("mult_table"))
	if table == nil {
		return 0
	}
	off := (minesIdx * (boardSize - 1) * 8) + (revealsIdx * 8)
	if int(off+8) > len(table) {
		return 0
	}
	return binary.LittleEndian.Uint64(table[off:])
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
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

	appendToList(keyActiveList, betID)
	updateMinTimeout(deadline)

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
	removeFromList(keyActiveList, betID)
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
	removeFromList(keyActiveList, betID)
	return 0
}

// ---------------------------------------------------------------------------
// block_update — fast-path skip, two-list processing
// ---------------------------------------------------------------------------

//export block_update
func block_update(seedPtr uint32) {
	bc := getBlockCount() + 1
	setBlockCount(bc)

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
	list := loadList(keyActiveList)
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
		removeFromList(keyActiveList, betID)
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

	// Determine mine/safe.
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
	appendToList(keyActiveList, betID)
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
				"11":"Invalid parameters — expected sender(20) + mines_count(1)",
				"12":"Mines count out of range — must be between 1 and 13",
				"3":"Insufficient bankroll liquidity"
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
// SHA-256 (inline — no crypto/sha256, FIPS panics in WASM)
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
	data = append(data, 0x80)
	for len(data)%64 != 56 {
		data = append(data, 0)
	}
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], bitLen)
	data = append(data, lenBuf[:]...)
	var w [64]uint32
	for off := 0; off < len(data); off += 64 {
		block := data[off : off+64]
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
