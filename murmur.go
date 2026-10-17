package billdogeng

// Murmurhash3 is the 32-bit (x86, seed-able) MurmurHash3, ported verbatim from
// the canonical web reference packages/ab-testing/src/ABTest.ts.
//
// This is the cross-platform bucketing primitive for local feature-flag
// evaluation. The exact same algorithm runs on web / iOS / Android / every
// server SDK so that Murmurhash3("{key}.{distinctId}") % 100 produces an
// identical bucket everywhere.
//
// Implementation notes (must not change without re-pinning the vectors):
//   - UTF-8 bytes (Go strings are already UTF-8)
//   - 32-bit multiply wraps naturally on uint32
//   - JS `<<` operates on signed int32; bytes here are masked to stay 0..255 so
//     `keyBytes[i*4+3] << 24` matches the JS sign-extension exactly because the
//     subsequent operations all reduce to the same uint32 bit pattern.
func Murmurhash3(key string) uint32 {
	return murmurhash3Seed(key, 0)
}

func murmurhash3Seed(key string, seed uint32) uint32 {
	keyBytes := []byte(key)
	h1 := seed
	const c1 uint32 = 0xcc9e2d51
	const c2 uint32 = 0x1b873593
	length := len(keyBytes)
	blocks := length / 4

	for i := 0; i < blocks; i++ {
		k1 := uint32(keyBytes[i*4]) |
			uint32(keyBytes[i*4+1])<<8 |
			uint32(keyBytes[i*4+2])<<16 |
			uint32(keyBytes[i*4+3])<<24
		k1 *= c1
		k1 = (k1 << 15) | (k1 >> 17)
		k1 *= c2
		h1 ^= k1
		h1 = (h1 << 13) | (h1 >> 19)
		h1 = h1*5 + 0xe6546b64
	}

	var k1 uint32
	remainder := length % 4
	offset := blocks * 4
	switch remainder {
	case 3:
		k1 ^= uint32(keyBytes[offset+2]) << 16
		fallthrough
	case 2:
		k1 ^= uint32(keyBytes[offset+1]) << 8
		fallthrough
	case 1:
		k1 ^= uint32(keyBytes[offset])
		k1 *= c1
		k1 = (k1 << 15) | (k1 >> 17)
		k1 *= c2
		h1 ^= k1
	}

	h1 ^= uint32(length)
	h1 ^= h1 >> 16
	h1 *= 0x85ebca6b
	h1 ^= h1 >> 13
	h1 *= 0xc2b2ae35
	h1 ^= h1 >> 16
	return h1
}

// BucketOf returns a deterministic 0..99 bucket for a "{key}.{distinctId}" seed.
func BucketOf(seed string) int {
	return int(Murmurhash3(seed) % 100)
}
