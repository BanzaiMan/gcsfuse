// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lease

import (
	"fmt"
	"io"
	"sort"

	"golang.org/x/net/context"
)

// Create a read proxy consisting of the contents defined by the supplied
// refreshers concatenated. See NewReadProxy for more.
//
// If rl is non-nil, it will be used as the first temporary copy of the
// contents, and must match the concatenation of the content returned by the
// refreshers.
func NewMultiReadProxy(
	fl FileLeaser,
	refreshers []Refresher,
	rl ReadLease) (rp ReadProxy) {
	// Create one wrapped read proxy per refresher.
	var wrappedProxies []readProxyAndOffset
	var size int64

	for _, r := range refreshers {
		wrapped := NewReadProxy(fl, r, nil)
		wrappedProxies = append(wrappedProxies, readProxyAndOffset{size, wrapped})
		size += wrapped.Size()
	}

	// Check that the lease the user gave us, if any, is consistent.
	if rl != nil && rl.Size() != size {
		panic(fmt.Sprintf(
			"Provided read lease of size %d bytes doesn't match combined size "+
				"%d bytes for %d refreshers",
			rl.Size(),
			size,
			len(refreshers)))
	}

	// Create the multi-read proxy.
	rp = &multiReadProxy{
		size:   size,
		leaser: fl,
		rps:    wrappedProxies,
		lease:  rl,
	}

	return
}

////////////////////////////////////////////////////////////////////////
// Implementation
////////////////////////////////////////////////////////////////////////

type multiReadProxy struct {
	/////////////////////////
	// Constant data
	/////////////////////////

	// The size of the proxied content.
	size int64

	/////////////////////////
	// Dependencies
	/////////////////////////

	leaser FileLeaser

	// The wrapped read proxies, indexed by their logical starting offset.
	//
	// INVARIANT: If len(rps) != 0, rps[0].off == 0
	// INVARIANT: For each x, x.rp.Size() >= 0
	// INVARIANT: For each i>0, rps[i].off == rps[i-i].off + rps[i-i].rp.Size()
	// INVARIANT: size is the sum over the wrapped proxy sizes.
	rps []readProxyAndOffset

	/////////////////////////
	// Mutable state
	/////////////////////////

	// A read lease for the entire contents. May be nil.
	//
	// INVARIANT: If lease != nil, size == lease.Size()
	lease ReadLease

	destroyed bool
}

func (mrp *multiReadProxy) Size() (size int64) {
	size = mrp.size
	return
}

func (mrp *multiReadProxy) ReadAt(
	ctx context.Context,
	p []byte,
	off int64) (n int, err error) {
	// Special case: can we read directly from our initial read lease?
	if mrp.lease != nil {
		n, err = mrp.lease.ReadAt(p, off)

		// Successful?
		if err == nil {
			return
		}

		// Revoked?
		if _, ok := err.(*RevokedError); ok {
			mrp.lease = nil
			err = nil
		} else {
			// Propagate other errors
			return
		}
	}

	// Special case: we don't support negative offsets, silly user.
	if off < 0 {
		err = fmt.Errorf("Invalid offset: %v", off)
		return
	}

	// Special case: offsets at or beyond the end of our content can never yield
	// any content, and the io.ReaderAt spec allows us to return EOF. Knock them
	// out here so we know off is in range when we start below.
	if off >= mrp.Size() {
		err = io.EOF
		return
	}

	// The read proxy that contains off is the *last* read proxy whose start
	// offset is less than or equal to off. Find the first that is greater and
	// move back one.
	//
	// Because we handled the special cases above, this must be in range.
	wrappedIndex := mrp.upperBound(off) - 1

	if wrappedIndex < 0 || wrappedIndex >= len(mrp.rps) {
		panic(fmt.Sprintf("Unexpected index: %v", wrappedIndex))
	}

	// Keep going until we've got nothing left to do.
	for len(p) > 0 {
		// Have we run out of wrapped read proxies?
		if wrappedIndex == len(mrp.rps) {
			err = io.EOF
			return
		}

		// Read from the wrapped proxy, accumulating into our total before checking
		// for a read error.
		wrappedN, wrappedErr := mrp.readFromOne(ctx, wrappedIndex, p, off)
		n += wrappedN
		if wrappedErr != nil {
			err = wrappedErr
			return
		}

		// readFromOne guarantees to either fill our buffer or exhaust the wrapped
		// proxy. So advance the buffer, the offset, and the wrapped proxy index
		// and go again.
		p = p[wrappedN:]
		off += int64(wrappedN)
		wrappedIndex++
	}

	return
}

func (mrp *multiReadProxy) Upgrade(
	ctx context.Context) (rwl ReadWriteLease, err error) {
	// This function is destructive; the user is not allowed to call us again.
	mrp.destroyed = true

	// Special case: can we upgrade directly from our initial read lease?
	if mrp.lease != nil {
		rwl, err = mrp.lease.Upgrade()

		// Successful?
		if err == nil {
			return
		}

		// Revoked?
		if _, ok := err.(*RevokedError); ok {
			mrp.lease = nil
			err = nil
		} else {
			// Propagate other errors
			return
		}
	}

	// Create a new read/write lease to return to the user. Ensure that it is
	// destroyed if we return in error.
	rwl, err = mrp.leaser.NewFile()
	if err != nil {
		err = fmt.Errorf("NewFile: %v", err)
		return
	}

	defer func() {
		if err != nil {
			rwl.Downgrade().Revoke()
		}
	}()

	// Accumulate each wrapped read proxy in turn.
	for i, entry := range mrp.rps {
		err = mrp.upgradeOne(ctx, rwl, entry.rp)
		if err != nil {
			err = fmt.Errorf("upgradeOne(%d): %v", i, err)
			return
		}
	}

	return
}

func (mrp *multiReadProxy) Destroy() {
	// Destroy all of the wrapped proxies.
	for _, entry := range mrp.rps {
		entry.rp.Destroy()
	}

	// Destroy the lease for the entire contents, if any.
	if mrp.lease != nil {
		mrp.lease.Revoke()
	}

	// Crash early if called again.
	mrp.rps = nil
	mrp.lease = nil
	mrp.destroyed = true
}

func (mrp *multiReadProxy) CheckInvariants() {
	if mrp.destroyed {
		panic("Use after destroyed")
	}

	// INVARIANT: If len(rps) != 0, rps[0].off == 0
	if len(mrp.rps) != 0 && mrp.rps[0].off != 0 {
		panic(fmt.Sprintf("Unexpected starting point: %v", mrp.rps[0].off))
	}

	// INVARIANT: For each x, x.rp.Size() >= 0
	for _, x := range mrp.rps {
		if x.rp.Size() < 0 {
			panic(fmt.Sprintf("Negative size: %v", x.rp.Size()))
		}
	}

	// INVARIANT: For each i>0, rps[i].off == rps[i-i].off + rps[i-i].rp.Size()
	for i := range mrp.rps {
		if i > 0 && !(mrp.rps[i].off == mrp.rps[i-1].off+mrp.rps[i-1].rp.Size()) {
			panic("Offsets are not indexed correctly.")
		}
	}

	// INVARIANT: size is the sum over the wrapped proxy sizes.
	var sum int64
	for _, wrapped := range mrp.rps {
		sum += wrapped.rp.Size()
	}

	if sum != mrp.size {
		panic(fmt.Sprintf("Size mismatch: %v vs. %v", sum, mrp.size))
	}

	// INVARIANT: If lease != nil, size == lease.Size()
	if mrp.lease != nil && mrp.size != mrp.lease.Size() {
		panic(fmt.Sprintf("Size mismatch: %v vs. %v", mrp.size, mrp.lease.Size()))
	}
}

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

type readProxyAndOffset struct {
	off int64
	rp  ReadProxy
}

// Return the index within mrp.rps of the first read proxy whose logical offset
// is greater than off. If there is none, return len(mrp.rps).
func (mrp *multiReadProxy) upperBound(off int64) (index int) {
	pred := func(i int) bool {
		return mrp.rps[i].off > off
	}

	return sort.Search(len(mrp.rps), pred)
}

// Serve a read from the wrapped proxy at the given index within our array of
// wrapped proxies. The offset is relative to the start of the multiReadProxy,
// not the wrapped proxy.
//
// Guarantees, letting wrapped be mrp.rps[i].rp and wrappedStart be
// mrp.rps[i].off:
//
//  *  If err == nil, n == len(p) || off + n == wrappedStart + wrapped.Size().
//  *  Never returns err == io.EOF.
//
// REQUIRES: index < len(mrp.rps)
// REQUIRES: mrp.rps[index].off <= off < mrp.rps[index].off + wrapped.Size()
func (mrp *multiReadProxy) readFromOne(
	ctx context.Context,
	index int,
	p []byte,
	off int64) (n int, err error) {
	// Check input requirements.
	if !(index < len(mrp.rps)) {
		panic(fmt.Sprintf("Out of range wrapped index: %v", index))
	}

	wrapped := mrp.rps[index].rp
	wrappedStart := mrp.rps[index].off
	wrappedSize := wrapped.Size()

	if !(wrappedStart <= off && off < wrappedStart+wrappedSize) {
		panic(fmt.Sprintf(
			"Offset %v not in range [%v, %v)",
			off,
			wrappedStart,
			wrappedStart+wrappedSize))
	}

	// Check guarantees on return.
	defer func() {
		if err == nil &&
			!(n == len(p) || off+int64(n) == wrappedStart+wrappedSize) {
			panic(fmt.Sprintf(
				"Failed to serve full read. "+
					"off: %d n: %d, len(p): %d, wrapped start: %d, wrapped size: %d",
				off,
				n,
				len(p),
				wrappedStart,
				wrappedSize))

			return
		}

		if err == io.EOF {
			panic("Unexpected EOF.")
		}
	}()

	// Read from the wrapped reader, translating the offset. We rely on the
	// wrapped reader to properly implement ReadAt, not returning a short read.
	wrappedOff := off - wrappedStart
	n, err = wrapped.ReadAt(ctx, p, wrappedOff)

	// Sanity check: the wrapped read proxy is supposed to return err == nil only
	// if the entire read was satisfied.
	if err == nil && n != len(p) {
		err = fmt.Errorf(
			"Wrapped proxy %d returned only %d bytes for a %d-byte read "+
				"starting at wrapped offset %d",
			index,
			n,
			len(p),
			wrappedOff)

		return
	}

	// Don't return io.EOF, as guaranteed.
	if err == io.EOF {
		// Sanity check: if we hit EOF, that should mean that we read up to the end
		// of the wrapped range.
		if int64(n) != wrappedSize-wrappedOff {
			err = fmt.Errorf(
				"Wrapped proxy %d returned unexpected EOF. n: %d, wrapped size: %d, "+
					"wrapped offset: %d",
				index,
				n,
				wrappedSize,
				wrappedOff)

			return
		}

		err = nil
	}

	return
}

// Upgrade the read proxy and copy its contents into the supplied read/write
// lease, then destroy it.
func (mrp *multiReadProxy) upgradeOne(
	ctx context.Context,
	dst ReadWriteLease,
	rp ReadProxy) (err error) {
	// Upgrade.
	src, err := rp.Upgrade(ctx)
	if err != nil {
		err = fmt.Errorf("Upgrade: %v", err)
		return
	}

	defer func() {
		src.Downgrade().Revoke()
	}()

	// Seek to the start and copy.
	_, err = src.Seek(0, 0)
	if err != nil {
		err = fmt.Errorf("Seek: %v", err)
		return
	}

	_, err = io.Copy(dst, src)
	if err != nil {
		err = fmt.Errorf("Copy: %v", err)
		return
	}

	return
}
