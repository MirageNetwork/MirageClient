// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package hashx

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math/rand"
	"testing"

	qt "github.com/frankban/quicktest"
	"tailscale.com/util/must"
)

// naiveHash is an obviously correct implementation of Hash.
type naiveHash struct {
	hash.Hash
	scratch [256]byte
}

func newNaive() *naiveHash               { return &naiveHash{Hash: sha256.New()} }
func (h *naiveHash) HashUint8(n uint8)   { h.Write(append(h.scratch[:0], n)) }
func (h *naiveHash) HashUint16(n uint16) { h.Write(binary.LittleEndian.AppendUint16(h.scratch[:0], n)) }
func (h *naiveHash) HashUint32(n uint32) { h.Write(binary.LittleEndian.AppendUint32(h.scratch[:0], n)) }
func (h *naiveHash) HashUint64(n uint64) { h.Write(binary.LittleEndian.AppendUint64(h.scratch[:0], n)) }
func (h *naiveHash) HashBytes(b []byte)  { h.Write(b) }
func (h *naiveHash) HashString(s string) { h.Write(append(h.scratch[:0], s...)) }

var bytes = func() (out []byte) {
	out = make([]byte, 130)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}()

type hasher interface {
	HashUint8(uint8)
	HashUint16(uint16)
	HashUint32(uint32)
	HashUint64(uint64)
	HashBytes([]byte)
	HashString(string)
}

func hashSuite(h hasher) {
	for i := 0; i < 10; i++ {
		for j := 0; j < 10; j++ {
			h.HashUint8(0x01)
			h.HashUint8(0x23)
			h.HashUint32(0x456789ab)
			h.HashUint8(0xcd)
			h.HashUint8(0xef)
			h.HashUint16(0x0123)
			h.HashUint32(0x456789ab)
			h.HashUint16(0xcdef)
			h.HashUint8(0x01)
			h.HashUint64(0x23456789abcdef01)
			h.HashUint16(0x2345)
			h.HashUint8(0x67)
			h.HashUint16(0x89ab)
			h.HashUint8(0xcd)
		}
		b := bytes[:(i+1)*13]
		if i%2 == 0 {
			h.HashBytes(b)
		} else {
			h.HashString(string(b))
		}
	}
}

func Test(t *testing.T) {
	c := qt.New(t)
	h1 := must.Get(New512(sha256.New()))
	h2 := newNaive()
	hashSuite(h1)
	hashSuite(h2)
	c.Assert(h1.Sum(nil), qt.DeepEquals, h2.Sum(nil))
}

func TestAllocations(t *testing.T) {
	c := qt.New(t)
	c.Run("Sum", func(c *qt.C) {
		h := must.Get(New512(sha256.New()))
		c.Assert(testing.AllocsPerRun(100, func() {
			var a [sha256.Size]byte
			h.Sum(a[:0])
		}), qt.Equals, 0.0)
	})
	c.Run("HashUint8", func(c *qt.C) {
		h := must.Get(New512(sha256.New()))
		c.Assert(testing.AllocsPerRun(100, func() {
			h.HashUint8(0x01)
		}), qt.Equals, 0.0)
	})
	c.Run("HashUint16", func(c *qt.C) {
		h := must.Get(New512(sha256.New()))
		c.Assert(testing.AllocsPerRun(100, func() {
			h.HashUint16(0x0123)
		}), qt.Equals, 0.0)
	})
	c.Run("HashUint32", func(c *qt.C) {
		h := must.Get(New512(sha256.New()))
		c.Assert(testing.AllocsPerRun(100, func() {
			h.HashUint32(0x01234567)
		}), qt.Equals, 0.0)
	})
	c.Run("HashUint64", func(c *qt.C) {
		h := must.Get(New512(sha256.New()))
		c.Assert(testing.AllocsPerRun(100, func() {
			h.HashUint64(0x0123456789abcdef)
		}), qt.Equals, 0.0)
	})
	c.Run("HashBytes", func(c *qt.C) {
		h := must.Get(New512(sha256.New()))
		c.Assert(testing.AllocsPerRun(100, func() {
			h.HashBytes(bytes)
		}), qt.Equals, 0.0)
	})
	c.Run("HashString", func(c *qt.C) {
		h := must.Get(New512(sha256.New()))
		c.Assert(testing.AllocsPerRun(100, func() {
			h.HashString("abcdefghijklmnopqrstuvwxyz")
		}), qt.Equals, 0.0)
	})
}

func Fuzz(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64) {
		c := qt.New(t)

		execute := func(h hasher, r *rand.Rand) {
			for i := 0; i < r.Intn(256); i++ {
				switch r.Intn(5) {
				case 0:
					n := uint8(r.Uint64())
					h.HashUint8(n)
				case 1:
					n := uint16(r.Uint64())
					h.HashUint16(n)
				case 2:
					n := uint32(r.Uint64())
					h.HashUint32(n)
				case 3:
					n := uint64(r.Uint64())
					h.HashUint64(n)
				case 4:
					b := make([]byte, r.Intn(256))
					r.Read(b)
					h.HashBytes(b)
				}
			}
		}

		r1 := rand.New(rand.NewSource(seed))
		r2 := rand.New(rand.NewSource(seed))

		h1 := must.Get(New512(sha256.New()))
		h2 := newNaive()

		execute(h1, r1)
		execute(h2, r2)

		c.Assert(h1.Sum(nil), qt.DeepEquals, h2.Sum(nil))

		execute(h1, r1)
		execute(h2, r2)

		c.Assert(h1.Sum(nil), qt.DeepEquals, h2.Sum(nil))

		h1.Reset()
		h2.Reset()

		execute(h1, r1)
		execute(h2, r2)

		c.Assert(h1.Sum(nil), qt.DeepEquals, h2.Sum(nil))
	})
}

func Benchmark(b *testing.B) {
	var sum [sha256.Size]byte
	b.Run("Hash", func(b *testing.B) {
		b.ReportAllocs()
		h := must.Get(New512(sha256.New()))
		for i := 0; i < b.N; i++ {
			h.Reset()
			hashSuite(h)
			h.Sum(sum[:0])
		}
	})
	b.Run("Naive", func(b *testing.B) {
		b.ReportAllocs()
		h := newNaive()
		for i := 0; i < b.N; i++ {
			h.Reset()
			hashSuite(h)
			h.Sum(sum[:0])
		}
	})
}
