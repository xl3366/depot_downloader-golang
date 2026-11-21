package crypto

// Adler32Seed0 computes the Adler-32 checksum with seed 0 (s1=0, s2=0).
// Steam uses this variant everywhere depot data and chunks are verified.
func Adler32Seed0(data []byte) uint32 {
	const mod = 65521
	var s1 uint32 = 0
	var s2 uint32 = 0

	// Process in blocks to prevent uint32 overflow
	// 5552 is the max number of iterations before s2 could overflow uint32
	for len(data) > 0 {
		block := data
		if len(block) > 5552 {
			block = block[:5552]
		}
		data = data[len(block):]

		for _, b := range block {
			s1 += uint32(b)
			s2 += s1
		}
		s1 %= mod
		s2 %= mod
	}

	return (s2 << 16) | s1
}
