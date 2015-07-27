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

package lease_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/GoogleCloudPlatform/gcsfuse/lease"
	. "github.com/jacobsa/oglematchers"
	. "github.com/jacobsa/ogletest"
)

func TestFileLeaser(t *testing.T) { RunTests(t) }

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

func panicIf(err *error) {
	if *err != nil {
		panic(*err)
	}
}

// Create a read/write lease and fill it in with data of the specified length.
// Panic on failure.
func newFileOfLength(
	fl lease.FileLeaser,
	length int) (rwl lease.ReadWriteLease) {
	var err error
	defer panicIf(&err)

	// Create the lease.
	rwl, err = fl.NewFile()
	if err != nil {
		err = fmt.Errorf("NewFile: %v", err)
		return
	}

	defer func() {
		if err != nil {
			rwl.Downgrade().Revoke()
			rwl = nil
		}
	}()

	// Write the contents.
	_, err = rwl.Write(bytes.Repeat([]byte("a"), length))
	if err != nil {
		err = fmt.Errorf("Write: %v", err)
		return
	}

	return
}

// Upgrade the supplied lease or panic.
func upgrade(rl lease.ReadLease) (rwl lease.ReadWriteLease) {
	var err error
	defer panicIf(&err)

	// Attempt to upgrade.
	rwl, err = rl.Upgrade()

	return
}

func growBy(w io.WriteSeeker, n int) {
	var err error
	defer panicIf(&err)

	// Seek to the end.
	_, err = w.Seek(0, 2)
	if err != nil {
		err = fmt.Errorf("Seek: %v", err)
		return
	}

	// Write.
	_, err = w.Write(bytes.Repeat([]byte("a"), n))
	if err != nil {
		err = fmt.Errorf("Write: %v", err)
		return
	}

	return
}

////////////////////////////////////////////////////////////////////////
// Boilerplate
////////////////////////////////////////////////////////////////////////

const limitNumFiles = 5
const limitBytes = 17

type FileLeaserTest struct {
	fl lease.FileLeaser
}

var _ SetUpInterface = &FileLeaserTest{}

func init() { RegisterTestSuite(&FileLeaserTest{}) }

func (t *FileLeaserTest) SetUp(ti *TestInfo) {
	t.fl = lease.NewFileLeaser("", limitNumFiles, limitBytes)
}

////////////////////////////////////////////////////////////////////////
// Tests
////////////////////////////////////////////////////////////////////////

func (t *FileLeaserTest) ReadWriteLeaseInitialState() {
	var n int
	var off int64
	var err error
	buf := make([]byte, 1024)

	// Create
	rwl, err := t.fl.NewFile()
	AssertEq(nil, err)
	defer func() { rwl.Downgrade().Revoke() }()

	// Size
	size, err := rwl.Size()
	AssertEq(nil, err)
	ExpectEq(0, size)

	// Seek
	off, err = rwl.Seek(0, 2)
	AssertEq(nil, err)
	ExpectEq(0, off)

	// Read
	n, err = rwl.Read(buf)
	ExpectEq(io.EOF, err)
	ExpectEq(0, n)

	// ReadAt
	n, err = rwl.ReadAt(buf, 0)
	ExpectEq(io.EOF, err)
	ExpectEq(0, n)
}

func (t *FileLeaserTest) ModifyThenObserveReadWriteLease() {
	var n int
	var off int64
	var size int64
	var err error
	buf := make([]byte, 1024)

	// Create
	rwl, err := t.fl.NewFile()
	AssertEq(nil, err)
	defer func() { rwl.Downgrade().Revoke() }()

	// Write, then check size and offset.
	n, err = rwl.Write([]byte("tacoburrito"))
	AssertEq(nil, err)
	ExpectEq(len("tacoburrito"), n)

	size, err = rwl.Size()
	AssertEq(nil, err)
	ExpectEq(len("tacoburrito"), size)

	off, err = rwl.Seek(0, 1)
	AssertEq(nil, err)
	ExpectEq(len("tacoburrito"), off)

	// Pwrite, then check size.
	n, err = rwl.WriteAt([]byte("enchilada"), 4)
	AssertEq(nil, err)
	ExpectEq(len("enchilada"), n)

	size, err = rwl.Size()
	AssertEq(nil, err)
	ExpectEq(len("tacoenchilada"), size)

	// Truncate downward, then check size.
	err = rwl.Truncate(4)
	AssertEq(nil, err)

	size, err = rwl.Size()
	AssertEq(nil, err)
	ExpectEq(len("taco"), size)

	// Seek, then read everything.
	off, err = rwl.Seek(0, 0)
	AssertEq(nil, err)
	ExpectEq(0, off)

	n, err = rwl.Read(buf)
	ExpectThat(err, AnyOf(nil, io.EOF))
	ExpectEq("taco", string(buf[0:n]))
}

func (t *FileLeaserTest) DowngradeThenObserve() {
	var n int
	var off int64
	var size int64
	var err error
	buf := make([]byte, 1024)

	// Create and write some data.
	rwl, err := t.fl.NewFile()
	AssertEq(nil, err)

	n, err = rwl.Write([]byte("taco"))
	AssertEq(nil, err)

	// Downgrade.
	rl := rwl.Downgrade()
	rwl = nil

	// Observing via the read lease should work fine.
	size = rl.Size()
	ExpectEq(len("taco"), size)

	off, err = rl.Seek(-4, 2)
	AssertEq(nil, err)
	ExpectEq(0, off)

	n, err = rl.Read(buf)
	ExpectThat(err, AnyOf(nil, io.EOF))
	ExpectEq("taco", string(buf[0:n]))

	n, err = rl.ReadAt(buf[0:2], 1)
	AssertEq(nil, err)
	ExpectEq("ac", string(buf[0:2]))
}

func (t *FileLeaserTest) DowngradeThenUpgradeThenObserve() {
	var n int
	var off int64
	var size int64
	var err error
	buf := make([]byte, 1024)

	// Create and write some data.
	rwl, err := t.fl.NewFile()
	AssertEq(nil, err)

	n, err = rwl.Write([]byte("taco"))
	AssertEq(nil, err)

	// Downgrade.
	rl := rwl.Downgrade()
	rwl = nil

	// Upgrade again.
	rwl, err = rl.Upgrade()
	AssertEq(nil, err)
	defer func() { rwl.Downgrade().Revoke() }()

	// Interacting with the read lease should no longer work.
	_, err = rl.Read(buf)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	_, err = rl.Seek(0, 0)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	_, err = rl.ReadAt(buf, 0)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	tmp, err := rl.Upgrade()
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))
	ExpectEq(nil, tmp)

	// Calling Revoke should cause nothing nasty to happen.
	rl.Revoke()

	// Observing via the new read/write lease should work fine.
	size, err = rwl.Size()
	AssertEq(nil, err)
	ExpectEq(len("taco"), size)

	off, err = rwl.Seek(-4, 2)
	AssertEq(nil, err)
	ExpectEq(0, off)

	n, err = rwl.Read(buf)
	ExpectThat(err, AnyOf(nil, io.EOF))
	ExpectEq("taco", string(buf[0:n]))

	n, err = rwl.ReadAt(buf[0:2], 1)
	AssertEq(nil, err)
	ExpectEq("ac", string(buf[0:2]))
}

func (t *FileLeaserTest) DowngradeFileWhoseSizeIsAboveLimit() {
	var err error
	buf := make([]byte, 1024)

	// Create and write data larger than the capacity.
	rwl, err := t.fl.NewFile()
	AssertEq(nil, err)

	_, err = rwl.Write(bytes.Repeat([]byte("a"), limitBytes+1))
	AssertEq(nil, err)

	// Downgrade.
	rl := rwl.Downgrade()
	rwl = nil

	// The read lease should be revoked on arrival.
	_, err = rl.Read(buf)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	_, err = rl.Seek(0, 0)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	_, err = rl.ReadAt(buf, 0)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	tmp, err := rl.Upgrade()
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))
	ExpectEq(nil, tmp)
}

func (t *FileLeaserTest) NewFileCausesEviction() {
	// Set up limitNumFiles read leases.
	var rls []lease.ReadLease
	for i := 0; i < limitNumFiles; i++ {
		rls = append(rls, newFileOfLength(t.fl, 0).Downgrade())
	}

	// All should still be good.
	for _, rl := range rls {
		AssertFalse(rl.Revoked())
	}

	// Creating two more write leases should cause two to be revoked.
	rwl0, err := t.fl.NewFile()
	AssertEq(nil, err)
	defer func() { rwl0.Downgrade().Revoke() }()

	rwl1, err := t.fl.NewFile()
	AssertEq(nil, err)
	defer func() { rwl1.Downgrade().Revoke() }()

	revoked := 0
	for _, rl := range rls {
		if rl.Revoked() {
			revoked++
		}
	}

	ExpectEq(2, revoked)
}

func (t *FileLeaserTest) WriteCausesEviction() {
	var err error

	// Set up a read lease whose size is right at the limit.
	rl := newFileOfLength(t.fl, limitBytes).Downgrade()
	AssertFalse(rl.Revoked())

	// Set up a new read/write lease. The read lease should still be unrevoked.
	rwl, err := t.fl.NewFile()
	AssertEq(nil, err)
	defer func() { rwl.Downgrade().Revoke() }()

	AssertFalse(rl.Revoked())

	// Writing zero bytes shouldn't cause trouble.
	_, err = rwl.Write([]byte(""))
	AssertEq(nil, err)

	AssertFalse(rl.Revoked())

	// But the next byte should.
	_, err = rwl.Write([]byte("a"))
	AssertEq(nil, err)

	ExpectTrue(rl.Revoked())
}

func (t *FileLeaserTest) WriteAtCausesEviction() {
	var err error
	AssertLt(3, limitBytes)

	// Set up a read lease whose size is three bytes below the limit.
	rl := newFileOfLength(t.fl, limitBytes-3).Downgrade()
	AssertFalse(rl.Revoked())

	// Set up a new read/write lease. The read lease should still be unrevoked.
	rwl, err := t.fl.NewFile()
	AssertEq(nil, err)
	defer func() { rwl.Downgrade().Revoke() }()

	AssertFalse(rl.Revoked())

	// Write in three bytes. Everything should be fine.
	_, err = rwl.Write([]byte("foo"))
	AssertEq(nil, err)

	// Overwriting a byte shouldn't cause trouble.
	_, err = rwl.WriteAt([]byte("p"), 0)
	AssertEq(nil, err)

	AssertFalse(rl.Revoked())

	// But extending the file by one byte should.
	_, err = rwl.WriteAt([]byte("taco"), 0)
	AssertEq(nil, err)

	ExpectTrue(rl.Revoked())
}

func (t *FileLeaserTest) TruncateCausesEviction() {
	var err error
	AssertLt(3, limitBytes)

	// Set up a read lease whose size is three bytes below the limit.
	rl := newFileOfLength(t.fl, limitBytes-3).Downgrade()
	AssertFalse(rl.Revoked())

	// Set up a new read/write lease. The read lease should still be unrevoked.
	rwl, err := t.fl.NewFile()
	AssertEq(nil, err)
	defer func() { rwl.Downgrade().Revoke() }()

	AssertFalse(rl.Revoked())

	// Truncate up to the limit. Nothing should happen.
	err = rwl.Truncate(3)
	AssertEq(nil, err)

	AssertFalse(rl.Revoked())

	// Truncate downward. Again, nothing should happen.
	err = rwl.Truncate(2)
	AssertEq(nil, err)

	AssertFalse(rl.Revoked())

	// But extending to four bytes should cause revocation.
	err = rwl.Truncate(4)
	AssertEq(nil, err)

	ExpectTrue(rl.Revoked())
}

func (t *FileLeaserTest) EvictionIsLRU() {
	AssertLt(4, limitBytes)

	// Arrange for four read leases, with a known order of recency of usage. Make
	// each the most recent in turn using different methods that we expect to
	// promote to most recent.
	rl0 := newFileOfLength(t.fl, 1).Downgrade()
	rl2 := newFileOfLength(t.fl, 1).Downgrade()
	rl3 := newFileOfLength(t.fl, 1).Downgrade()

	rl0.Read([]byte{})                          // Least recent
	rl1 := newFileOfLength(t.fl, 1).Downgrade() // Second least recent
	rl2.Read([]byte{})                          // Third least recent
	rl3.ReadAt([]byte{}, 0)                     // Fourth least recent

	// Fill up the remaining space. All read leases should still be valid.
	rwl := newFileOfLength(t.fl, limitBytes-4)

	AssertFalse(rl0.Revoked())
	AssertFalse(rl1.Revoked())
	AssertFalse(rl2.Revoked())
	AssertFalse(rl3.Revoked())

	// Use up one more byte. The least recently used lease should be revoked.
	growBy(rwl, 1)

	AssertTrue(rl0.Revoked())
	AssertFalse(rl1.Revoked())
	AssertFalse(rl2.Revoked())
	AssertFalse(rl3.Revoked())

	// Two more bytes. Now the next two should go.
	growBy(rwl, 2)

	AssertTrue(rl0.Revoked())
	AssertTrue(rl1.Revoked())
	AssertTrue(rl2.Revoked())
	AssertFalse(rl3.Revoked())

	// Downgrading and upgrading the read/write lease should change nothing.
	rwl = upgrade(rwl.Downgrade())
	AssertNe(nil, rwl)
	defer func() { rwl.Downgrade().Revoke() }()

	AssertTrue(rl0.Revoked())
	AssertTrue(rl1.Revoked())
	AssertTrue(rl2.Revoked())
	AssertFalse(rl3.Revoked())

	// But writing one more byte should boot the last one.
	growBy(rwl, 1)

	AssertTrue(rl0.Revoked())
	AssertTrue(rl1.Revoked())
	AssertTrue(rl2.Revoked())
	AssertTrue(rl3.Revoked())
}

func (t *FileLeaserTest) RevokeVoluntarily() {
	var err error
	buf := make([]byte, 1024)

	AssertLt(3, limitBytes)

	// Set up two read leases, together occupying all space, and an empty
	// read/write lease.
	rl0 := newFileOfLength(t.fl, 3).Downgrade()
	rl1 := newFileOfLength(t.fl, limitBytes-3).Downgrade()
	rwl := newFileOfLength(t.fl, 0)
	defer func() { rwl.Downgrade().Revoke() }()

	AssertFalse(rl0.Revoked())
	AssertFalse(rl1.Revoked())

	// Voluntarily revoke the first. Nothing should work anymore.
	rl0.Revoke()
	AssertTrue(rl0.Revoked())

	_, err = rl0.Read(buf)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	_, err = rl0.Seek(0, 0)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	_, err = rl0.ReadAt(buf, 0)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	// Calling Revoke more times should be harmless.
	rl0.Revoke()
	rl0.Revoke()
	rl0.Revoke()

	// The other lease should be fine.
	AssertFalse(rl1.Revoked())

	// The revocation should have freed up credit that can be used by the
	// read/write lease without booting the other read lease.
	growBy(rwl, 3)
	ExpectFalse(rl1.Revoked())

	// But one more byte should evict it, as usual.
	growBy(rwl, 1)
	ExpectTrue(rl1.Revoked())
}

func (t *FileLeaserTest) RevokeAllReadLeases() {
	var err error
	buf := make([]byte, 1024)

	AssertLt(3, limitBytes)

	// Set up two read leases, together occupying all space.
	rl0 := newFileOfLength(t.fl, 3).Downgrade()
	rl1 := newFileOfLength(t.fl, limitBytes-3).Downgrade()

	AssertFalse(rl0.Revoked())
	AssertFalse(rl1.Revoked())

	// Revoke all read leases. None of them should work anymore.
	t.fl.RevokeReadLeases()

	AssertTrue(rl0.Revoked())
	AssertTrue(rl1.Revoked())

	_, err = rl0.Read(buf)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	_, err = rl1.Read(buf)
	ExpectThat(err, HasSameTypeAs(&lease.RevokedError{}))

	// Calling Revoke more times should be harmless.
	rl0.Revoke()
	rl1.Revoke()
}
