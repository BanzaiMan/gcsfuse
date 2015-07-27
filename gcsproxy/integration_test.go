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

package gcsproxy_test

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/GoogleCloudPlatform/gcsfuse/gcsproxy"
	"github.com/GoogleCloudPlatform/gcsfuse/lease"
	"github.com/GoogleCloudPlatform/gcsfuse/mutable"
	"github.com/jacobsa/gcloud/gcs"
	"github.com/jacobsa/gcloud/gcs/gcsfake"
	"github.com/jacobsa/gcloud/gcs/gcsutil"
	. "github.com/jacobsa/oglematchers"
	. "github.com/jacobsa/ogletest"
	"github.com/jacobsa/timeutil"
)

func TestIntegration(t *testing.T) { RunTests(t) }

////////////////////////////////////////////////////////////////////////
// Boilerplate
////////////////////////////////////////////////////////////////////////

// Create random content of the given length, which must be a multiple of 4.
func randBytes(n int) (b []byte) {
	if n%4 != 0 {
		panic(fmt.Sprintf("Invalid n: %d", n))
	}

	b = make([]byte, n)
	for i := 0; i < n; i += 4 {
		w := rand.Uint32()
		b[i] = byte(w >> 24)
		b[i+1] = byte(w >> 16)
		b[i+2] = byte(w >> 8)
		b[i+3] = byte(w >> 0)
	}

	return
}

////////////////////////////////////////////////////////////////////////
// Boilerplate
////////////////////////////////////////////////////////////////////////

const chunkSize = 1<<18 + 3
const fileLeaserLimitNumFiles = math.MaxInt32
const fileLeaserLimitBytes = 1 << 21

type IntegrationTest struct {
	ctx    context.Context
	bucket gcs.Bucket
	leaser lease.FileLeaser
	clock  timeutil.SimulatedClock
	syncer gcsproxy.ObjectSyncer

	mc mutable.Content
}

var _ SetUpInterface = &IntegrationTest{}
var _ TearDownInterface = &IntegrationTest{}

func init() { RegisterTestSuite(&IntegrationTest{}) }

func (t *IntegrationTest) SetUp(ti *TestInfo) {
	t.ctx = ti.Ctx
	t.bucket = gcsfake.NewFakeBucket(&t.clock, "some_bucket")
	t.leaser = lease.NewFileLeaser(
		"",
		fileLeaserLimitNumFiles,
		fileLeaserLimitBytes)

	// Set up a fixed, non-zero time.
	t.clock.SetTime(time.Date(2012, 8, 15, 22, 56, 0, 0, time.Local))

	// Set up the object syncer.
	const appendThreshold = 0
	const tmpObjectPrefix = ".gcsfuse_tmp/"

	t.syncer = gcsproxy.NewObjectSyncer(
		appendThreshold,
		tmpObjectPrefix,
		t.bucket)
}

func (t *IntegrationTest) TearDown() {
	if t.mc != nil {
		t.mc.Destroy()
	}
}

func (t *IntegrationTest) create(o *gcs.Object) {
	// Set up the read proxy.
	rp := gcsproxy.NewReadProxy(
		o,
		nil,
		chunkSize,
		t.leaser,
		t.bucket)

	// Use it to create the mutable content.
	t.mc = mutable.NewContent(rp, &t.clock)
}

// Return the object generation, or -1 if non-existent. Panic on error.
func (t *IntegrationTest) objectGeneration(name string) (gen int64) {
	// Stat.
	req := &gcs.StatObjectRequest{Name: name}
	o, err := t.bucket.StatObject(t.ctx, req)

	if _, ok := err.(*gcs.NotFoundError); ok {
		gen = -1
		return
	}

	if err != nil {
		panic(err)
	}

	// Check the result.
	if o.Generation > math.MaxInt64 {
		panic(fmt.Sprintf("Out of range: %v", o.Generation))
	}

	gen = o.Generation
	return
}

func (t *IntegrationTest) sync(src *gcs.Object) (
	rl lease.ReadLease, o *gcs.Object, err error) {
	rl, o, err = t.syncer.SyncObject(t.ctx, src, t.mc)
	if err == nil && rl != nil {
		t.mc = nil
	}

	return
}

////////////////////////////////////////////////////////////////////////
// Tests
////////////////////////////////////////////////////////////////////////

func (t *IntegrationTest) ReadThenSync() {
	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Read the contents.
	buf := make([]byte, 1024)
	n, err := t.mc.ReadAt(t.ctx, buf, 0)

	AssertThat(err, AnyOf(io.EOF, nil))
	ExpectEq(len("taco"), n)
	ExpectEq("taco", string(buf[:n]))

	// Sync doesn't need to do anything.
	rl, newObj, err := t.sync(o)

	AssertEq(nil, err)
	ExpectEq(nil, rl)
	ExpectEq(nil, newObj)
}

func (t *IntegrationTest) WriteThenSync() {
	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Overwrite the first byte.
	n, err := t.mc.WriteAt(t.ctx, []byte("p"), 0)

	AssertEq(nil, err)
	ExpectEq(1, n)

	// Sync should save out the new generation.
	rl, newObj, err := t.sync(o)
	AssertEq(nil, err)

	ExpectNe(o.Generation, newObj.Generation)
	ExpectEq(t.objectGeneration("foo"), newObj.Generation)

	// Read via the bucket.
	contents, err := gcsutil.ReadObject(t.ctx, t.bucket, "foo")
	AssertEq(nil, err)
	ExpectEq("paco", string(contents))

	// Read via the lease.
	_, err = rl.Seek(0, 0)
	AssertEq(nil, err)

	contents, err = ioutil.ReadAll(rl)
	AssertEq(nil, err)
	ExpectEq("paco", string(contents))

	// There should be no junk left over in the bucket besides the object of
	// interest.
	objects, runs, err := gcsutil.ListAll(
		t.ctx,
		t.bucket,
		&gcs.ListObjectsRequest{})

	AssertEq(nil, err)
	AssertEq(1, len(objects))
	AssertEq(0, len(runs))

	ExpectEq("foo", objects[0].Name)
}

func (t *IntegrationTest) AppendThenSync() {
	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Append some data.
	n, err := t.mc.WriteAt(t.ctx, []byte("burrito"), 4)

	AssertEq(nil, err)
	ExpectEq(len("burrito"), n)

	// Sync should save out the new generation.
	rl, newObj, err := t.sync(o)
	AssertEq(nil, err)

	ExpectNe(o.Generation, newObj.Generation)
	ExpectEq(t.objectGeneration("foo"), newObj.Generation)

	// Read via the bucket.
	contents, err := gcsutil.ReadObject(t.ctx, t.bucket, "foo")
	AssertEq(nil, err)
	ExpectEq("tacoburrito", string(contents))

	// Read via the lease.
	_, err = rl.Seek(0, 0)
	AssertEq(nil, err)

	contents, err = ioutil.ReadAll(rl)
	AssertEq(nil, err)
	ExpectEq("tacoburrito", string(contents))

	// There should be no junk left over in the bucket besides the object of
	// interest.
	objects, runs, err := gcsutil.ListAll(
		t.ctx,
		t.bucket,
		&gcs.ListObjectsRequest{})

	AssertEq(nil, err)
	AssertEq(1, len(objects))
	AssertEq(0, len(runs))

	ExpectEq("foo", objects[0].Name)
}

func (t *IntegrationTest) TruncateThenSync() {
	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Truncate.
	err = t.mc.Truncate(t.ctx, 2)
	AssertEq(nil, err)

	// Sync should save out the new generation.
	rl, newObj, err := t.sync(o)
	AssertEq(nil, err)

	ExpectNe(o.Generation, newObj.Generation)
	ExpectEq(t.objectGeneration("foo"), newObj.Generation)

	contents, err := gcsutil.ReadObject(t.ctx, t.bucket, "foo")
	AssertEq(nil, err)
	ExpectEq("ta", string(contents))

	// Read via the lease.
	_, err = rl.Seek(0, 0)
	AssertEq(nil, err)

	contents, err = ioutil.ReadAll(rl)
	AssertEq(nil, err)
	ExpectEq("ta", string(contents))
}

func (t *IntegrationTest) Stat_InitialState() {
	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Stat.
	sr, err := t.mc.Stat(t.ctx)
	AssertEq(nil, err)

	ExpectEq(o.Size, sr.Size)
	ExpectEq(o.Size, sr.DirtyThreshold)
	ExpectEq(nil, sr.Mtime)
}

func (t *IntegrationTest) Stat_Dirty() {
	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Dirty.
	t.clock.AdvanceTime(time.Second)
	truncateTime := t.clock.Now()

	err = t.mc.Truncate(t.ctx, 2)
	AssertEq(nil, err)

	t.clock.AdvanceTime(time.Second)

	// Stat.
	sr, err := t.mc.Stat(t.ctx)
	AssertEq(nil, err)

	ExpectEq(2, sr.Size)
	ExpectEq(2, sr.DirtyThreshold)
	ExpectThat(sr.Mtime, Pointee(timeutil.TimeEq(truncateTime)))
}

func (t *IntegrationTest) WithinLeaserLimit() {
	AssertLt(len("taco"), fileLeaserLimitBytes)

	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Extend to be up against the leaser limit, then write out to GCS, which
	// should downgrade to a read lease.
	err = t.mc.Truncate(t.ctx, fileLeaserLimitBytes)
	AssertEq(nil, err)

	rl, _, err := t.sync(o)
	AssertEq(nil, err)

	// The backing object should be present and contain the correct contents.
	contents, err := gcsutil.ReadObject(t.ctx, t.bucket, o.Name)
	AssertEq(nil, err)
	ExpectEq(fileLeaserLimitBytes, len(contents))

	// Delete the backing object.
	err = t.bucket.DeleteObject(t.ctx, &gcs.DeleteObjectRequest{Name: o.Name})
	AssertEq(nil, err)

	// We should still be able to read the contents, because the read lease
	// should still be valid.
	buf := make([]byte, 4)
	n, err := rl.ReadAt(buf, 0)

	AssertEq(nil, err)
	ExpectEq("taco", string(buf[0:n]))
}

func (t *IntegrationTest) LargerThanLeaserLimit() {
	AssertLt(len("taco"), fileLeaserLimitBytes)

	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Extend to be past the leaser limit, then write out to GCS, which should
	// downgrade to a read lease.
	err = t.mc.Truncate(t.ctx, fileLeaserLimitBytes+1)
	AssertEq(nil, err)

	rl, _, err := t.sync(o)
	AssertEq(nil, err)

	// The backing object should be present and contain the correct contents.
	contents, err := gcsutil.ReadObject(t.ctx, t.bucket, o.Name)
	AssertEq(nil, err)
	ExpectEq(fileLeaserLimitBytes+1, len(contents))

	// Delete the backing object.
	err = t.bucket.DeleteObject(t.ctx, &gcs.DeleteObjectRequest{Name: o.Name})
	AssertEq(nil, err)

	// The contents should be lost, because the leaser should have revoked the
	// read lease.
	_, err = rl.ReadAt(make([]byte, len(contents)), 0)
	ExpectThat(err, Error(HasSubstr("revoked")))
}

func (t *IntegrationTest) BackingObjectHasBeenDeleted_BeforeReading() {
	// Create an object to obtain a record, then delete it.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	err = t.bucket.DeleteObject(t.ctx, &gcs.DeleteObjectRequest{Name: o.Name})
	AssertEq(nil, err)

	// Create a mutable object around it.
	t.create(o)

	// Sync doesn't need to do anything.
	rl, newObj, err := t.sync(o)

	AssertEq(nil, err)
	ExpectEq(nil, rl)
	ExpectEq(nil, newObj)

	// Anything that needs to fault in the contents should fail.
	_, err = t.mc.ReadAt(t.ctx, []byte{}, 0)
	ExpectThat(err, Error(HasSubstr("not found")))

	err = t.mc.Truncate(t.ctx, 10)
	ExpectThat(err, Error(HasSubstr("not found")))

	_, err = t.mc.WriteAt(t.ctx, []byte{}, 0)
	ExpectThat(err, Error(HasSubstr("not found")))
}

func (t *IntegrationTest) BackingObjectHasBeenDeleted_AfterReading() {
	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Fault in the contents.
	_, err = t.mc.ReadAt(t.ctx, []byte{}, 0)
	AssertEq(nil, err)

	// Delete the backing object.
	err = t.bucket.DeleteObject(t.ctx, &gcs.DeleteObjectRequest{Name: o.Name})
	AssertEq(nil, err)

	// Reading and modications should still work.
	_, err = t.mc.ReadAt(t.ctx, []byte{}, 0)
	AssertEq(nil, err)

	_, err = t.mc.WriteAt(t.ctx, []byte("a"), 0)
	AssertEq(nil, err)

	truncateTime := t.clock.Now()
	err = t.mc.Truncate(t.ctx, 1)
	AssertEq(nil, err)
	t.clock.AdvanceTime(time.Second)

	// Stat should see the current state.
	sr, err := t.mc.Stat(t.ctx)
	AssertEq(nil, err)

	ExpectEq(1, sr.Size)
	ExpectEq(0, sr.DirtyThreshold)
	ExpectThat(sr.Mtime, Pointee(timeutil.TimeEq(truncateTime)))

	// Sync should fail with a precondition error.
	_, _, err = t.sync(o)
	ExpectThat(err, HasSameTypeAs(&gcs.PreconditionError{}))

	// Nothing should have been created.
	_, err = gcsutil.ReadObject(t.ctx, t.bucket, o.Name)
	ExpectThat(err, HasSameTypeAs(&gcs.NotFoundError{}))
}

func (t *IntegrationTest) BackingObjectHasBeenOverwritten_BeforeReading() {
	// Create an object, then create the mutable object wrapper around it.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Overwrite the GCS object.
	_, err = gcsutil.CreateObject(t.ctx, t.bucket, "foo", "burrito")
	AssertEq(nil, err)

	// Sync doesn't need to do anything.
	rl, newObj, err := t.sync(o)

	AssertEq(nil, err)
	ExpectEq(nil, rl)
	ExpectEq(nil, newObj)

	// Anything that needs to fault in the contents should fail.
	_, err = t.mc.ReadAt(t.ctx, []byte{}, 0)
	ExpectThat(err, Error(HasSubstr("not found")))

	err = t.mc.Truncate(t.ctx, 10)
	ExpectThat(err, Error(HasSubstr("not found")))

	_, err = t.mc.WriteAt(t.ctx, []byte{}, 0)
	ExpectThat(err, Error(HasSubstr("not found")))
}

func (t *IntegrationTest) BackingObjectHasBeenOverwritten_AfterReading() {
	// Create.
	o, err := gcsutil.CreateObject(t.ctx, t.bucket, "foo", "taco")
	AssertEq(nil, err)

	t.create(o)

	// Fault in the contents.
	_, err = t.mc.ReadAt(t.ctx, []byte{}, 0)
	AssertEq(nil, err)

	// Overwrite the backing object.
	_, err = gcsutil.CreateObject(t.ctx, t.bucket, "foo", "burrito")
	AssertEq(nil, err)

	// Reading and modications should still work.
	_, err = t.mc.ReadAt(t.ctx, []byte{}, 0)
	AssertEq(nil, err)

	_, err = t.mc.WriteAt(t.ctx, []byte("a"), 0)
	AssertEq(nil, err)

	truncateTime := t.clock.Now()
	err = t.mc.Truncate(t.ctx, 3)
	AssertEq(nil, err)
	t.clock.AdvanceTime(time.Second)

	// Stat should see the current state.
	sr, err := t.mc.Stat(t.ctx)
	AssertEq(nil, err)

	ExpectEq(3, sr.Size)
	ExpectEq(0, sr.DirtyThreshold)
	ExpectThat(sr.Mtime, Pointee(timeutil.TimeEq(truncateTime)))

	// Sync should fail with a precondition error.
	_, _, err = t.sync(o)
	ExpectThat(err, HasSameTypeAs(&gcs.PreconditionError{}))

	// The newer version should still be present.
	contents, err := gcsutil.ReadObject(t.ctx, t.bucket, o.Name)
	AssertEq(nil, err)
	ExpectEq("burrito", string(contents))
}

func (t *IntegrationTest) MultipleInteractions() {
	// We will run through the script below for multiple interesting object
	// sizes.
	sizes := []int{
		0,
		1,
		chunkSize - 1,
		chunkSize,
		chunkSize + 1,
		3*chunkSize - 1,
		3 * chunkSize,
		3*chunkSize + 1,
		fileLeaserLimitBytes - 1,
		fileLeaserLimitBytes,
		fileLeaserLimitBytes + 1,
		((fileLeaserLimitBytes / chunkSize) - 1) * chunkSize,
		(fileLeaserLimitBytes / chunkSize) * chunkSize,
		((fileLeaserLimitBytes / chunkSize) + 1) * chunkSize,
	}

	// Generate random contents for the maximum size.
	var maxSize int
	for _, size := range sizes {
		if size > maxSize {
			maxSize = size
		}
	}

	randData := randBytes(maxSize)

	// Transition the mutable object in and out of the dirty state. Make sure
	// everything stays consistent.
	for i, size := range sizes {
		desc := fmt.Sprintf("test case %d (size %d)", i, size)
		name := fmt.Sprintf("obj_%d", i)
		buf := make([]byte, size)

		// Create the backing object with random initial contents.
		expectedContents := make([]byte, size)
		copy(expectedContents, randData)

		o, err := gcsutil.CreateObject(
			t.ctx,
			t.bucket,
			name,
			string(expectedContents))

		AssertEq(nil, err)

		// Create a mutable object around it.
		t.create(o)

		// Read the contents of the mutable object.
		_, err = t.mc.ReadAt(t.ctx, buf, 0)

		AssertThat(err, AnyOf(nil, io.EOF))
		if !bytes.Equal(buf, expectedContents) {
			AddFailure("Contents mismatch for %s", desc)
			AbortTest()
		}

		// Modify some bytes.
		if size > 0 {
			expectedContents[0] = 17
			expectedContents[size/2] = 19
			expectedContents[size-1] = 23

			_, err = t.mc.WriteAt(t.ctx, []byte{17}, 0)
			AssertEq(nil, err)

			_, err = t.mc.WriteAt(t.ctx, []byte{19}, int64(size/2))
			AssertEq(nil, err)

			_, err = t.mc.WriteAt(t.ctx, []byte{23}, int64(size-1))
			AssertEq(nil, err)
		}

		// Compare contents again.
		_, err = t.mc.ReadAt(t.ctx, buf, 0)

		AssertThat(err, AnyOf(nil, io.EOF))
		if !bytes.Equal(buf, expectedContents) {
			AddFailure("Contents mismatch for %s", desc)
			AbortTest()
		}

		// Sync and recreate if necessary.
		_, newObj, err := t.sync(o)
		AssertEq(nil, err)

		if newObj != nil {
			t.create(newObj)
		}

		// Check the new backing object's contents.
		objContents, err := gcsutil.ReadObject(t.ctx, t.bucket, name)
		AssertEq(nil, err)
		if !bytes.Equal(objContents, expectedContents) {
			AddFailure("Contents mismatch for %s", desc)
			AbortTest()
		}

		// Compare contents again.
		_, err = t.mc.ReadAt(t.ctx, buf, 0)

		AssertThat(err, AnyOf(nil, io.EOF))
		if !bytes.Equal(buf, expectedContents) {
			AddFailure("Contents mismatch for %s", desc)
			AbortTest()
		}

		// Dirty again.
		if size > 0 {
			expectedContents[0] = 29

			_, err = t.mc.WriteAt(t.ctx, []byte{29}, 0)
			AssertEq(nil, err)
		}

		// Compare contents again.
		_, err = t.mc.ReadAt(t.ctx, buf, 0)

		AssertThat(err, AnyOf(nil, io.EOF))
		if !bytes.Equal(buf, expectedContents) {
			AddFailure("Contents mismatch for %s", desc)
			AbortTest()
		}
	}
}
