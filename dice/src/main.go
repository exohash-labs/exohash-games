// Dice calculator — WASM protocol v3.
//
// Exports: alloc, dealloc, place_bet, bet_action, block_update, info
// Imports: env.kv_get, env.kv_set, env.kv_delete,
//          env.reserve, env.settle, env.emit_event,
//          env.get_gas_budget, env.get_gas_used
//
// block_update receives 32-byte DKG seed directly.
// Game manages its own pending bet list in KV.
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
func kv_delete(keyPtr, keyLen uint32)

//go:wasmimport env reserve
func host_reserve(betID, amount uint64) uint32

//go:wasmimport env settle
func host_settle(betID, payout uint64, kind uint32) uint32

//go:wasmimport env emit_event
func host_emit_event(topicPtr, topicLen, dataPtr, dataLen uint32)

//go:wasmimport env get_bettor
func host_get_bettor(betID uint64, outPtr uint32) uint32

//go:wasmimport env get_gas_budget
func get_gas_budget() uint64

//go:wasmimport env get_gas_used
func get_gas_used() uint64

// ---------------------------------------------------------------------------
// Memory management
// ---------------------------------------------------------------------------

//export alloc
func alloc(size uint32) *byte {
	buf := make([]byte, size)
	return &buf[0]
}

//export dealloc
func dealloc(ptr *byte, size uint32) {}

// ---------------------------------------------------------------------------
// Game constants
// ---------------------------------------------------------------------------

const (
	houseEdgeBP = 100
	minChanceBP = 100
	maxChanceBP = 9800

	// Per-block bet cap. With dice settling everything next block and each
	// place_bet ~80K WASM gas, 100/block fits the 10M per-block budget.
	maxBetsPerBlock = 100

	kindWin  = 1
	kindLoss = 2
)

// KV keys
var (
	keyPendingList   = []byte("pl") // pending bet ID list: [betID1 u64][betID2 u64]...
	keyBetsThisBlock = []byte("bb") // u64 per-block place_bet counter (reset on block_update)
	keyCalcID        = []byte("ci") // calculator ID — mixed into RNG entropy to de-correlate instances
)

// ---------------------------------------------------------------------------
// init_game — store calculator ID for RNG de-correlation across instances.
// ---------------------------------------------------------------------------

//export init_game
func init_game(sentinelID, bankrollID, calculatorID uint64) {
	idBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(idBuf, calculatorID)
	kvSet(keyCalcID, idBuf)
}

func getCalcID() uint64 {
	v := kvGetBytes(keyCalcID)
	if v == nil || len(v) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(v)
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
// place_bet — called during MsgPlaceBet tx
// ---------------------------------------------------------------------------

//export place_bet
func place_bet(betID, bankrollID, calculatorID, stake uint64, paramsPtr, paramsLen uint32) uint32 {
	params := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(paramsPtr))), paramsLen)
	if len(params) < 29 {
		return 1
	}
	betMode := params[20]
	threshold := binary.LittleEndian.Uint64(params[21:29])

	chance := chanceBP(betMode, threshold)
	if chance < minChanceBP || chance > maxChanceBP || chance == 0 {
		return 2
	}

	// Per-block bet cap — bounds new-bet WASM gas in this block.
	if getBetsThisBlock() >= maxBetsPerBlock {
		return 4
	}

	maxPayout := mulDiv(stake, fairMultBP(chance), 10000)
	if host_reserve(betID, maxPayout) != 0 {
		return 3
	}

	// Store bet state in KV.
	state := make([]byte, 25)
	binary.LittleEndian.PutUint64(state[0:], betID)
	binary.LittleEndian.PutUint64(state[8:], stake)
	state[16] = betMode
	binary.LittleEndian.PutUint64(state[17:], threshold)
	kvSet(betKey(betID), state)

	// Add to pending list.
	addPending(betID)
	setBetsThisBlock(getBetsThisBlock() + 1)

	emitJSON("bet", "bet_id", betID, "stake", stake, "chance_bp", chance, "max_payout", maxPayout)
	return 0
}

// ---------------------------------------------------------------------------
// block_update — called every block with DKG seed
// ---------------------------------------------------------------------------

//export block_update
func block_update(seedPtr uint32) {
	// Reset per-block place_bet counter at the boundary. All place_bet calls
	// inside this block share this counter; capped at maxBetsPerBlock above.
	setBetsThisBlock(0)

	// Read pending bet list.
	pending := kvGetBytes(keyPendingList)
	if len(pending) == 0 {
		return // no pending bets
	}

	// Need seed to settle.
	if seedPtr == 0 {
		return // no RNG — wait for next block
	}
	seed := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(seedPtr))), 32)

	// Settle pending bets, paging within gas budget.
	budget := get_gas_budget()
	settled := 0
	for i := 0; i+8 <= len(pending); i += 8 {
		if get_gas_used()+50_000 > budget {
			break // leave remaining for next block
		}
		betID := binary.LittleEndian.Uint64(pending[i : i+8])
		settleBet(betID, seed)
		settled++
	}

	// Update pending list: remove settled, keep remainder.
	remaining := pending[settled*8:]
	if len(remaining) == 0 {
		kvDelete(keyPendingList)
	} else {
		kvSet(keyPendingList, remaining)
	}
}

func settleBet(betID uint64, seed []byte) {
	state := kvGetBytes(betKey(betID))
	if state == nil || len(state) < 25 {
		return
	}

	storedBetID := binary.LittleEndian.Uint64(state[0:8])
	stake := binary.LittleEndian.Uint64(state[8:16])
	betMode := state[16]
	threshold := binary.LittleEndian.Uint64(state[17:25])

	chance := chanceBP(betMode, threshold)
	mult := fairMultBP(chance)
	effChance := chance * (10000 - houseEdgeBP) / 10000

	// Build the entropy buffer locally so we can emit its bytes for audit.
	// Touching the bytes via emit also defends against a TinyGo codegen
	// quirk that produced non-deterministic hashes for some (seed, bet_id)
	// pairs when the rng-derivation code stood alone.
	calcID := getCalcID()
	var ebuf [16]byte
	binary.BigEndian.PutUint64(ebuf[0:], storedBetID)
	binary.LittleEndian.PutUint64(ebuf[8:], calcID)
	entropy := make([]byte, len(seed)+16)
	copy(entropy, seed)
	copy(entropy[len(seed):], ebuf[:])
	sum := sha256sum(entropy)
	roll := binary.BigEndian.Uint64(sum[0:8]) % 10000
	win := isWin(betMode, roll, effChance)

	payout := uint64(0)
	settleKind := uint32(kindLoss)
	if win {
		payout = mulDiv(stake, mult, 10000)
		settleKind = uint32(kindWin)
	}

	addr := getBettorAddr(betID)
	host_settle(betID, payout, settleKind)

	// Clean up bet state.
	kvDelete(betKey(betID))

	resultStr := "loss"
	if win {
		resultStr = "win"
	}
	emitSettleJSON(storedBetID, roll, chance, effChance, mult, payout, stake, addr, resultStr)

	// roll_audit: per-bet RNG audit trail. Players can verify the chain
	// computed roll correctly by re-deriving sha256(seed||bet_id||calc_id)
	// and applying %10000. Wide field set also defeats the TinyGo codegen
	// quirk (touching the entropy bytes forces the compiler to keep them).
	emitJSON("roll_audit",
		"bet_id", storedBetID,
		"roll", roll,
		"chance_bp", chance,
		"e0", uint64(entropy[0]),
		"e1", uint64(entropy[1]),
		"e2", uint64(entropy[2]),
		"e30", uint64(entropy[30]),
		"e31", uint64(entropy[31]),
		"e32", uint64(entropy[32]),
		"e33", uint64(entropy[33]),
		"e46", uint64(entropy[46]),
		"e47", uint64(entropy[47]),
	)
}

// ---------------------------------------------------------------------------
// info
// ---------------------------------------------------------------------------

//export info
func info() *byte {
	data := []byte(`{
		"name":"Dice-firstexo",
		"engine":"dice",
		"mode":"v3",
		"house_edge_bp":100,
		"developer":"ExoHash",
		"description":"Provably fair dice — single bet, single outcome, next-block settlement",
		"errors":{
			"place_bet":{
				"1":"Invalid parameters — expected sender(20) + mode(1) + threshold(8)",
				"2":"Chance out of range — must be between 1% and 98%",
				"3":"Insufficient bankroll liquidity",
				"4":"Block full — max 100 bets per block, retry next block"
			}
		}
	}`)
	result := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(result[0:4], uint32(len(data)))
	copy(result[4:], data)
	return &result[0]
}

// ---------------------------------------------------------------------------
// Pending bet list helpers
// ---------------------------------------------------------------------------

func addPending(betID uint64) {
	existing := kvGetBytes(keyPendingList)
	entry := make([]byte, 8)
	binary.LittleEndian.PutUint64(entry, betID)
	newList := append(existing, entry...)
	kvSet(keyPendingList, newList)
}

// ---------------------------------------------------------------------------
// KV helpers
// ---------------------------------------------------------------------------

var betKeyBuf [9]byte

func betKey(betID uint64) []byte {
	betKeyBuf[0] = 'b'
	binary.LittleEndian.PutUint64(betKeyBuf[1:], betID)
	return betKeyBuf[:]
}

func kvSet(key, value []byte) {
	kv_set(
		uint32(uintptr(unsafe.Pointer(&key[0]))), uint32(len(key)),
		uint32(uintptr(unsafe.Pointer(&value[0]))), uint32(len(value)),
	)
}

func kvGetBytes(key []byte) []byte {
	packed := kv_get(uint32(uintptr(unsafe.Pointer(&key[0]))), uint32(len(key)))
	if packed == 0 {
		return nil
	}
	ptr := uint32(packed >> 32)
	length := uint32(packed & 0xFFFFFFFF)
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
}

func kvDelete(key []byte) {
	kv_delete(uint32(uintptr(unsafe.Pointer(&key[0]))), uint32(len(key)))
}

// ---------------------------------------------------------------------------
// Dice math
// ---------------------------------------------------------------------------

func chanceBP(mode byte, threshold uint64) uint64 {
	switch mode {
	case 1:
		return 10000 - threshold
	case 2:
		return threshold
	default:
		return 0
	}
}

func fairMultBP(chance uint64) uint64 {
	if chance == 0 {
		return 0
	}
	return (10000 * 10000) / chance
}

func isWin(mode byte, roll, effChance uint64) bool {
	switch mode {
	case 1:
		return roll >= (10000 - effChance)
	case 2:
		return roll < effChance
	default:
		return false
	}
}


// ---------------------------------------------------------------------------
// Event helpers
// ---------------------------------------------------------------------------

const maxAddrBuf = 64

func getBettorAddr(betID uint64) string {
	buf := make([]byte, maxAddrBuf)
	n := host_get_bettor(betID, uint32(uintptr(unsafe.Pointer(&buf[0]))))
	if n == 0 || n > maxAddrBuf {
		return ""
	}
	return string(buf[:n])
}

func emitJSON(topic string, pairs ...interface{}) {
	json := fmtJSON(pairs...)
	topicBytes := []byte(topic)
	jsonBytes := []byte(json)
	host_emit_event(
		uint32(uintptr(unsafe.Pointer(&topicBytes[0]))), uint32(len(topicBytes)),
		uint32(uintptr(unsafe.Pointer(&jsonBytes[0]))), uint32(len(jsonBytes)),
	)
}

func emitSettleJSON(betID, roll, chance, effChance, mult, payout, stake uint64, addr, result string) {
	json := fmtJSON("bet_id", betID, "roll", roll, "chance_bp", chance, "eff_chance_bp", effChance, "mult_bp", mult, "payout", payout, "result", result, "stake", stake, "addr", addr)
	topic := []byte("settle")
	jsonBytes := []byte(json)
	host_emit_event(
		uint32(uintptr(unsafe.Pointer(&topic[0]))), uint32(len(topic)),
		uint32(uintptr(unsafe.Pointer(&jsonBytes[0]))), uint32(len(jsonBytes)),
	)
}

func fmtJSON(pairs ...interface{}) string {
	buf := make([]byte, 0, 128)
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
// SHA-256 (no crypto/sha256 — FIPS panic in WASM)
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
	data = append(data, 0x80)
	for (len(data) % 64) != 56 {
		data = append(data, 0)
	}
	bits := uint64(msgLen) * 8
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], bits)
	data = append(data, lenBuf[:]...)

	for offset := 0; offset < len(data); offset += 64 {
		chunk := data[offset : offset+64]
		var w [64]uint32
		for i := 0; i < 16; i++ {
			w[i] = binary.BigEndian.Uint32(chunk[i*4:])
		}
		for i := 16; i < 64; i++ {
			s0 := rightRotate(w[i-15], 7) ^ rightRotate(w[i-15], 18) ^ (w[i-15] >> 3)
			s1 := rightRotate(w[i-2], 17) ^ rightRotate(w[i-2], 19) ^ (w[i-2] >> 10)
			w[i] = w[i-16] + s0 + w[i-7] + s1
		}
		a, b, c, d, e, f, g, h := h0, h1, h2, h3, h4, h5, h6, h7
		for i := 0; i < 64; i++ {
			S1 := rightRotate(e, 6) ^ rightRotate(e, 11) ^ rightRotate(e, 25)
			ch := (e & f) ^ (^e & g)
			temp1 := h + S1 + ch + sha256K[i] + w[i]
			S0 := rightRotate(a, 2) ^ rightRotate(a, 13) ^ rightRotate(a, 22)
			maj := (a & b) ^ (a & c) ^ (b & c)
			temp2 := S0 + maj
			h = g
			g = f
			f = e
			e = d + temp1
			d = c
			c = b
			b = a
			a = temp1 + temp2
		}
		h0 += a
		h1 += b
		h2 += c
		h3 += d
		h4 += e
		h5 += f
		h6 += g
		h7 += h
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

func rightRotate(x uint32, n uint) uint32 {
	return (x >> n) | (x << (32 - n))
}

// ---------------------------------------------------------------------------
// Safe 128-bit multiply-divide (matches mines/crash)
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

func main() {}
