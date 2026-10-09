package udev

import "encoding/binary"

// murmurHash2 is MurmurHash2 (32-bit, Austin Appleby), the hash libudev
// uses for the subsystem and devtype filters in its monitor header
// (systemd's string_hash32, seed 0). Receivers that subscribed to one
// subsystem compare these words in a BPF filter before they even read the
// message, so they must match bit for bit.
func murmurHash2(data []byte, seed uint32) uint32 {
	const m, r = 0x5bd1e995, 24
	h := seed ^ uint32(len(data))
	for len(data) >= 4 {
		k := binary.LittleEndian.Uint32(data)
		k *= m
		k ^= k >> r
		k *= m
		h *= m
		h ^= k
		data = data[4:]
	}
	switch len(data) {
	case 3:
		h ^= uint32(data[2]) << 16
		fallthrough
	case 2:
		h ^= uint32(data[1]) << 8
		fallthrough
	case 1:
		h ^= uint32(data[0])
		h *= m
	}
	h ^= h >> 13
	h *= m
	h ^= h >> 15
	return h
}
