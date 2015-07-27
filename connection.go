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

package fuse

import (
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"runtime"
	"sync"
	"syscall"

	"golang.org/x/net/context"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/internal/buffer"
	"github.com/jacobsa/fuse/internal/fusekernel"
)

type contextKeyType uint64

const contextKey contextKeyType = 0

// Ask the Linux kernel for larger read requests.
//
// As of 2015-03-26, the behavior in the kernel is:
//
//  *  (http://goo.gl/bQ1f1i, http://goo.gl/HwBrR6) Set the local variable
//     ra_pages to be init_response->max_readahead divided by the page size.
//
//  *  (http://goo.gl/gcIsSh, http://goo.gl/LKV2vA) Set
//     backing_dev_info::ra_pages to the min of that value and what was sent
//     in the request's max_readahead field.
//
//  *  (http://goo.gl/u2SqzH) Use backing_dev_info::ra_pages when deciding
//     how much to read ahead.
//
//  *  (http://goo.gl/JnhbdL) Don't read ahead at all if that field is zero.
//
// Reading a page at a time is a drag. Ask for a larger size.
const maxReadahead = 1 << 20

// A connection to the fuse kernel process.
type Connection struct {
	debugLogger *log.Logger
	errorLogger *log.Logger

	// The device through which we're talking to the kernel, and the protocol
	// version that we're using to talk to it.
	dev      *os.File
	protocol fusekernel.Protocol

	// The context from which all op contexts inherit.
	parentCtx context.Context

	// For logging purposes only.
	nextOpID uint32

	mu sync.Mutex

	// A freelist of InMessage structs, the allocation of which can be a hot spot
	// for CPU usage. Each element is in an undefined state, and must be
	// re-initialized.
	//
	// GUARDED_BY(mu)
	messageFreelist []*buffer.InMessage

	// A map from fuse "unique" request ID (*not* the op ID for logging used
	// above) to a function that cancel's its associated context.
	//
	// GUARDED_BY(mu)
	cancelFuncs map[uint64]func()
}

// State that is maintained for each in-flight op. This is stuffed into the
// context that the user uses to reply to the op.
type opState struct {
	inMsg *buffer.InMessage
	op    fuseops.Op
	opID  uint64 // For logging
}

// Create a connection wrapping the supplied file descriptor connected to the
// kernel. You must eventually call c.close().
//
// The loggers may be nil.
func newConnection(
	parentCtx context.Context,
	debugLogger *log.Logger,
	errorLogger *log.Logger,
	dev *os.File) (c *Connection, err error) {
	c = &Connection{
		debugLogger: debugLogger,
		errorLogger: errorLogger,
		dev:         dev,
		parentCtx:   parentCtx,
		cancelFuncs: make(map[uint64]func()),
	}

	// Initialize.
	err = c.Init()
	if err != nil {
		c.close()
		err = fmt.Errorf("Init: %v", err)
		return
	}

	return
}

// Do the work necessary to cause the mount process to complete.
func (c *Connection) Init() (err error) {
	// Read the init op.
	ctx, op, err := c.ReadOp()
	if err != nil {
		err = fmt.Errorf("Reading init op: %v", err)
		return
	}

	initOp, ok := op.(*internalInitOp)
	if !ok {
		c.Reply(ctx, syscall.EPROTO)
		err = fmt.Errorf("Expected *internalInitOp, got %T", op)
		return
	}

	// Make sure the protocol version spoken by the kernel is new enough.
	min := fusekernel.Protocol{
		fusekernel.ProtoVersionMinMajor,
		fusekernel.ProtoVersionMinMinor,
	}

	if initOp.Kernel.LT(min) {
		c.Reply(ctx, syscall.EPROTO)
		err = fmt.Errorf("Version too old: %v", initOp.Kernel)
		return
	}

	// Downgrade our protocol if necessary.
	c.protocol = fusekernel.Protocol{
		fusekernel.ProtoVersionMaxMajor,
		fusekernel.ProtoVersionMaxMinor,
	}

	if initOp.Kernel.LT(c.protocol) {
		c.protocol = initOp.Kernel
	}

	// Respond to the init op.
	initOp.Library = c.protocol
	initOp.MaxReadahead = maxReadahead
	initOp.MaxWrite = buffer.MaxWriteSize
	initOp.Flags = fusekernel.InitBigWrites

	c.Reply(ctx, nil)
	return
}

// Log information for an operation with the given ID. calldepth is the depth
// to use when recovering file:line information with runtime.Caller.
func (c *Connection) debugLog(
	opID uint32,
	calldepth int,
	format string,
	v ...interface{}) {
	if c.debugLogger == nil {
		return
	}

	// Get file:line info.
	var file string
	var line int
	var ok bool

	_, file, line, ok = runtime.Caller(calldepth)
	if !ok {
		file = "???"
	}

	fileLine := fmt.Sprintf("%v:%v", path.Base(file), line)

	// Format the actual message to be printed.
	msg := fmt.Sprintf(
		"Op 0x%08x %24s] %v",
		opID,
		fileLine,
		fmt.Sprintf(format, v...))

	// Print it.
	c.debugLogger.Println(msg)
}

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) recordCancelFunc(
	fuseID uint64,
	f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.cancelFuncs[fuseID]; ok {
		panic(fmt.Sprintf("Already have cancel func for request %v", fuseID))
	}

	c.cancelFuncs[fuseID] = f
}

// Set up state for an op that is about to be returned to the user, given its
// underlying fuse opcode and request ID.
//
// Return a context that should be used for the op.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) beginOp(
	opCode uint32,
	fuseID uint64) (ctx context.Context) {
	// Start with the parent context.
	ctx = c.parentCtx

	// Set up a cancellation function.
	//
	// Special case: On Darwin, osxfuse aggressively reuses "unique" request IDs.
	// This matters for Forget requests, which have no reply associated and
	// therefore have IDs that are immediately eligible for reuse. For these, we
	// should not record any state keyed on their ID.
	//
	// Cf. https://github.com/osxfuse/osxfuse/issues/208
	if opCode != fusekernel.OpForget {
		var cancel func()
		ctx, cancel = context.WithCancel(ctx)
		c.recordCancelFunc(fuseID, cancel)
	}

	return
}

// Clean up all state associated with an op to which the user has responded,
// given its underlying fuse opcode and request ID. This must be called before
// a response is sent to the kernel, to avoid a race where the request's ID
// might be reused by osxfuse.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) finishOp(
	opCode uint32,
	fuseID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Even though the op is finished, context.WithCancel requires us to arrange
	// for the cancellation function to be invoked. We also must remove it from
	// our map.
	//
	// Special case: we don't do this for Forget requests. See the note in
	// beginOp above.
	if opCode != fusekernel.OpForget {
		cancel, ok := c.cancelFuncs[fuseID]
		if !ok {
			panic(fmt.Sprintf("Unknown request ID in finishOp: %v", fuseID))
		}

		cancel()
		delete(c.cancelFuncs, fuseID)
	}
}

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) handleInterrupt(fuseID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// NOTE(jacobsa): fuse.txt in the Linux kernel documentation
	// (https://goo.gl/H55Dnr) defines the kernel <-> userspace protocol for
	// interrupts.
	//
	// In particular, my reading of it is that an interrupt request cannot be
	// delivered to userspace before the original request. The part about the
	// race and EAGAIN appears to be aimed at userspace programs that
	// concurrently process requests (cf. http://goo.gl/BES2rs).
	//
	// So in this method if we can't find the ID to be interrupted, it means that
	// the request has already been replied to.
	//
	// Cf. https://github.com/osxfuse/osxfuse/issues/208
	// Cf. http://comments.gmane.org/gmane.comp.file-systems.fuse.devel/14675
	cancel, ok := c.cancelFuncs[fuseID]
	if !ok {
		return
	}

	cancel()
}

// m.Init must be called.
func (c *Connection) allocateInMessage() (m *buffer.InMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Can we pull from the freelist?
	l := len(c.messageFreelist)
	if l != 0 {
		m = c.messageFreelist[l-1]
		c.messageFreelist = c.messageFreelist[:l-1]
		return
	}

	// Otherwise, allocate a new one.
	m = new(buffer.InMessage)

	return
}

func (c *Connection) destroyInMessage(m *buffer.InMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Stick it on the freelist.
	c.messageFreelist = append(c.messageFreelist, m)
}

// Read the next message from the kernel. The message must later be destroyed
// using destroyInMessage.
func (c *Connection) readMessage() (m *buffer.InMessage, err error) {
	// Allocate a message.
	m = c.allocateInMessage()

	// Loop past transient errors.
	for {
		// Attempt a reaed.
		err = m.Init(c.dev)

		// Special cases:
		//
		//  *  ENODEV means fuse has hung up.
		//
		//  *  EINTR means we should try again. (This seems to happen often on
		//     OS X, cf. http://golang.org/issue/11180)
		//
		if pe, ok := err.(*os.PathError); ok {
			switch pe.Err {
			case syscall.ENODEV:
				err = io.EOF

			case syscall.EINTR:
				err = nil
				continue
			}
		}

		if err != nil {
			c.destroyInMessage(m)
			m = nil
			return
		}

		return
	}
}

// Write the supplied message to the kernel.
func (c *Connection) writeMessage(msg []byte) (err error) {
	// Avoid the retry loop in os.File.Write.
	n, err := syscall.Write(int(c.dev.Fd()), msg)
	if err != nil {
		return
	}

	if n != len(msg) {
		err = fmt.Errorf("Wrote %d bytes; expected %d", n, len(msg))
		return
	}

	return
}

// Read the next op from the kernel process, returning the op and a context
// that should be used for work related to the op. Return io.EOF if the kernel
// has closed the connection.
//
// If err != nil, the user is responsible for later calling c.Reply with the
// returned context.
//
// This function delivers ops in exactly the order they are received from
// /dev/fuse. It must not be called multiple times concurrently.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) ReadOp() (ctx context.Context, op fuseops.Op, err error) {
	// Keep going until we find a request we know how to convert.
	for {
		// Read the next message from the kernel.
		var m *buffer.InMessage
		m, err = c.readMessage()
		if err != nil {
			return
		}

		// Convert the message to an op.
		op, err := convertInMessage(m, c.protocol)
		if err != nil {
			err = fmt.Errorf("convertInMessage: %v", err)
			return
		}

		// Choose an ID for this operation for the purposes of logging, and log it.
		opID := c.nextOpID
		c.nextOpID++

		c.debugLog(opID, 1, "<- %#v", op)

		// Special case: handle interrupt requests inline.
		if interruptOp, ok := op.(*fuseops.InternalInterruptOp); ok {
			c.handleInterrupt(interruptOp.FuseID)
			continue
		}

		// Set up a context that remembers information about this op.
		ctx = c.beginOp(m.Header().Opcode, m.Header().Unique)
		ctx = context.WithValue(ctx, contextKey, opState{m, opID, op})

		// Special case: responding to statfs is required to make mounting work on
		// OS X. We don't currently expose the capability for the file system to
		// intercept this.
		if _, ok := op.(*internalStatFSOp); ok {
			c.Reply(ctx, nil)
			continue
		}

		// Return the op to the user.
		return
	}
}

// Close the connection. Must not be called until operations that were read
// from the connection have been responded to.
func (c *Connection) close() (err error) {
	// Posix doesn't say that close can be called concurrently with read or
	// write, but luckily we exclude the possibility of a race by requiring the
	// user to respond to all ops first.
	err = c.dev.Close()
	return
}
