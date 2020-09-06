// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Cond 的主要作用就是获取锁之后，wait() 方法会等待一个通知，来进行下一步锁释放等操作，以此控制锁的合适释放、释放的频率。适用于并发环境下 goroutine 的等待和通知
// Cond implements a condition variable, a rendezvous point
// for goroutines waiting for or announcing the occurrence
// of an event.
//
// Each Cond has an associated Locker L (often a *Mutex or *RWMutex),
// which must be held when changing the condition and
// when calling the Wait method.
//
// A Cond must not be copied after first use.
type Cond struct {
	noCopy noCopy	// noCopy 可以嵌入到结构中， 在第一次适用后不可复制，使用 go vet 作为检测使用 TODO 这里不是很明白

	// L is held while observing or changing the condition
	// 根据需求初始化不同的锁，例如：Mutex 和 RWMutex
	L Locker

	notify  notifyList 	// 通知列表，调用 Wait() 方法的goroutine 会被放入 list 中，每次唤醒后，从这里取出
	checker copyChecker	// 复制检查，检查 cond 实例是否被复制
}

// NewCond returns a new Cond with Locker l.
func NewCond(l Locker) *Cond {
	return &Cond{L: l}
}

// Wait atomically unlocks c.L and suspends execution
// of the calling goroutine. After later resuming execution,
// Wait locks c.L before returning. Unlike in other systems,
// Wait cannot return unless awoken by Broadcast or Signal.
//
// Because c.L is not locked when Wait first resumes, the caller
// typically cannot assume that the condition is true when
// Wait returns. Instead, the caller should Wait in a loop:
//
//    c.L.Lock()
//    for !condition() {
//        c.Wait()
//    }
//    ... make use of condition ...
//    c.L.Unlock()
//
func (c *Cond) Wait() {
	// 检查 c 是否被复制的，如果是就 panic
	c.checker.check()
	// 将当前 goroutine 加入等待队列
	t := runtime_notifyListAdd(&c.notify)
	c.L.Unlock()
	// 等待队列中的所有的goroutine执行等待唤醒操作
	runtime_notifyListWait(&c.notify, t)
	c.L.Lock()
}

// Signal wakes one goroutine waiting on c, if there is any.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Signal() {
	c.checker.check()
	runtime_notifyListNotifyOne(&c.notify)
}

// Broadcast wakes all goroutines waiting on c.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Broadcast() {
	c.checker.check()
	runtime_notifyListNotifyAll(&c.notify)
}

// copyChecker holds back pointer to itself to detect object copying.
type copyChecker uintptr

func (c *copyChecker) check() {
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
		uintptr(*c) != uintptr(unsafe.Pointer(c)) {
		panic("sync.Cond is copied")
	}
}

// noCopy may be embedded into structs which must not be copied
// after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
