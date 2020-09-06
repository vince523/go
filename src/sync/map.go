// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)


/*
sync.Map 原理：大致是使用空间换时间的策略，通过冗余两个数据结构（read、dirty）实现加锁对性能的影响。
通过引入两个 map, read 和 dirty, (注意 read 最终其实也是一个 map ,readOnly 结构体里包含一个map[interface{}]*entry),
将读写分离到不同的map, 其中 read map 提供并发的读和已存的元素的写操作，而dirty map 则负责读写。这样read map 就可以在不加锁的情况下
进行并发读取，当 read map 中没有读取到值时，再加锁进行后续的读取，并累加未命中的数，当未命中数大于等于 dirty map 长度， 将dirty map 上升
为 read map。虽然引入两个 map 但是底层数据存储的是指针，指向同一份值。
 */


// Map is like a Go map[interface{}]interface{} but is safe for concurrent use
// by multiple goroutines without additional locking or coordination.
// Loads, stores, and deletes run in amortized constant time.
//
// The Map type is specialized. Most code should use a plain Go map instead,
// with separate locking or coordination, for better type safety and to make it
// easier to maintain other invariants along with the map content.
//
// The Map type is optimized for two common use cases: (1) when the entry for a given
// key is only ever written once but read many times, as in caches that only grow,
// or (2) when multiple goroutines read, write, and overwrite entries for disjoint
// sets of keys. In these two cases, use of a Map may significantly reduce lock
// contention compared to a Go map paired with a separate Mutex or RWMutex.
//
// The zero Map is empty and ready for use. A Map must not be copied after first use.
type Map struct {
	mu Mutex	// 互斥锁， 用于锁定 dirty map

	// read contains the portion of the map's contents that are safe for
	// concurrent access (with or without mu held).
	//
	// The read field itself is always safe to load, but must only be stored with
	// mu held.
	//
	// Entries stored in read may be updated concurrently without mu, but updating
	// a previously-expunged entry requires that the entry be copied to the dirty
	// map and unexpunged with mu held.
	// 支持原子操作
	read atomic.Value // readOnly 注意这个 readOnly 指的是下面 readOnly 结构体

	// dirty contains the portion of the map's contents that require mu to be
	// held. To ensure that the dirty map can be promoted to the read map quickly,
	// it also includes all of the non-expunged entries in the read map.
	//
	// Expunged entries are not stored in the dirty map. An expunged entry in the
	// clean map must be unexpunged and added to the dirty map before a new value
	// can be stored to it.
	//
	// If the dirty map is nil, the next write to the map will initialize it by
	// making a shallow copy of the clean map, omitting stale entries.
	dirty map[interface{}]*entry // dirty 是一个当前最新的map, 允许读写

	// misses counts the number of loads since the read map was last updated that
	// needed to lock mu to determine whether the key was present.
	//
	// Once enough misses have occurred to cover the cost of copying the dirty
	// map, the dirty map will be promoted to the read map (in the unamended
	// state) and the next store to the map will make a new dirty copy.
	misses int // 主要记录 read 读取不到数据加锁读取 read map 以及 dirty map 的次数，当misses 等于dirty 的长度时，将dirty 复制到read
}

// readOnly is an immutable struct stored atomically in the Map.read field.
// readOnly 是一个不可变的结构体，通过原子操作存储在 Map.read 字段中
type readOnly struct {
	m       map[interface{}]*entry
	amended bool // true if the dirty map contains some key not in m.
}

// expunged is an arbitrary pointer that marks entries which have been deleted
// from the dirty map.
var expunged = unsafe.Pointer(new(interface{}))

// An entry is a slot in the map corresponding to a particular key.
type entry struct {
	// p points to the interface{} value stored for the entry.
	//
	// If p == nil, the entry has been deleted and m.dirty == nil.
	//
	// If p == expunged, the entry has been deleted, m.dirty != nil, and the entry
	// is missing from m.dirty.
	//
	// Otherwise, the entry is valid and recorded in m.read.m[key] and, if m.dirty
	// != nil, in m.dirty[key].
	//
	// An entry can be deleted by atomic replacement with nil: when m.dirty is
	// next created, it will atomically replace nil with expunged and leave
	// m.dirty[key] unset.
	//
	// An entry's associated value can be updated by atomic replacement, provided
	// p != expunged. If p == expunged, an entry's associated value can be updated
	// only after first setting m.dirty[key] = e so that lookups using the dirty
	// map find the entry.

	//p == nil的时候： 表示为被删除 m.dirty == nil
	// p == expunged 时：也是被删除，但 m.dirty != nil
	// 其他情况：表示存储真正的数据
	p unsafe.Pointer // *interface{}
}

func newEntry(i interface{}) *entry {
	return &entry{p: unsafe.Pointer(&i)}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	// 检测元素是否存在read map中
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	// 不存在的情况
	if !ok && read.amended {
		// 给 dirty map 加锁
		m.mu.Lock()
		// Avoid reporting a spurious miss if m.dirty got promoted while we were
		// blocked on m.mu. (If further loads of the same key will not miss, it's
		// not worth copying the dirty map for this key.)
		// 再次检测元素是否存在于read map， 主要是防止加锁的时候，dirty map 晋升为 read map 了
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		// 还不存在
		if !ok && read.amended {
			// 从 dirty map 中获取对应的元素
			e, ok = m.dirty[key]
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			// 这里没有判断 ok 值，是因为不管元素存不存在，都要记录 misses 次数
			m.missLocked()
		}
		// 释放锁
		m.mu.Unlock()
	}

	// 这里再判断ok ，如果元素不存在，就直接返回
	if !ok {
		return nil, false
	}
	// 元素取值
	return e.load()
}

func (e *entry) load() (value interface{}, ok bool) {
	// 原子操作读取
	p := atomic.LoadPointer(&e.p)
	// 如果元素不存在或者是被删除了, 直接返回
	if p == nil || p == expunged {
		return nil, false
	}
	return *(*interface{})(p), true
}

// Store sets the value for a key.
func (m *Map) Store(key, value interface{}) {

	read, _ := m.read.Load().(readOnly)
	//  先检测是否存在, 如果存在这个 key ，并且尝试写入，如果写入成功，则结束，直接返回
	if e, ok := read.m[key]; ok && e.tryStore(&value) {
		return
	}

	// dirty map 加锁
	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	// 再次检测
	if e, ok := read.m[key]; ok {
		// unexpungeLocked 方法是判断元素是否被标识为删除
		if e.unexpungeLocked() {
			// 该元素之前被删除了，就是说dirty map != nil 并且 dirty map 中又不包含这个元素，因此需要把该元素加入到 dirty map 中
			// The entry was previously expunged, which implies that there is a
			// non-nil dirty map and this entry is not in it.
			m.dirty[key] = e
		}
		// 更新 read map
		e.storeLocked(&value)
	} else if e, ok := m.dirty[key]; ok {
		// 这是 read map 中不存在该元素，但是dirty map 中存在
		// 更新 read map
		e.storeLocked(&value)
	} else {
		// read.amended == false 意味着 dirty map 是空（dirty map == nil）, 需要拷贝一份read map 数据到 dirty map
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			// 拷贝 read map 数据写入到 dirty map
			m.dirtyLocked()
			// 并且把 readOnly 中的 amended 字段重置 为true (说明 dirty map != nil 了)
			m.read.Store(readOnly{m: read.m, amended: true})
		}
		// 最后把需要设置的新元素，写入到 dirty map 中
		m.dirty[key] = newEntry(value)
	}
	// 解锁
	m.mu.Unlock()
}

// tryStore stores a value if the entry has not been expunged.
//
// If the entry is expunged, tryStore returns false and leaves the entry
// unchanged.
// 尝试写入元素
func (e *entry) tryStore(i *interface{}) bool {
	for {
		p := atomic.LoadPointer(&e.p)
		// 判断是否被标识为删除了
		if p == expunged {
			return false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
			return true
		}
	}
}

// unexpungeLocked ensures that the entry is not marked as expunged.
// unexpungeLocked 确保元素没有被标记为删除
// If the entry was previously expunged, it must be added to the dirty map
// before m.mu is unlocked.
// 如果这个元素之前被删除了，它必须在解锁前被加入到 dirty map 中。这就是👆(m.dirtyLocked()的操作)

func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return atomic.CompareAndSwapPointer(&e.p, expunged, nil)
}

// storeLocked unconditionally stores a value to the entry.
//
// The entry must be known not to be expunged.
func (e *entry) storeLocked(i *interface{}) {
	atomic.StorePointer(&e.p, unsafe.Pointer(i))
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.

func (m *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	// Avoid locking if it's a clean hit.
	read, _ := m.read.Load().(readOnly)
	// 先检测是否存在
	if e, ok := read.m[key]; ok {
		// 如果存在，则尝试获取该元素值或者保存该值
		actual, loaded, ok := e.tryLoadOrStore(value)
		if ok {
			return actual, loaded
		}
	}
	// 下面逻辑和 Store 方法类同
	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		if e.unexpungeLocked() {
			m.dirty[key] = e
		}
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok {
		actual, loaded, _ = e.tryLoadOrStore(value)
		m.missLocked()
	} else {
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// tryLoadOrStore atomically loads or stores a value if the entry is not
// expunged.
//
// If the entry is expunged, tryLoadOrStore leaves the entry unchanged and
// returns with ok==false.
func (e *entry) tryLoadOrStore(i interface{}) (actual interface{}, loaded, ok bool) {
	p := atomic.LoadPointer(&e.p)
	// 元素被标记为删除，直接返回
	if p == expunged {
		return nil, false, false
	}
	// 改元素存在真实值，返回原来值
	if p != nil {
		return *(*interface{})(p), true, true
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if we hit the "load" path or the entry is expunged, we
	// shouldn't bother heap-allocating.
	// p == nil 的情况：
	// 这里 p == nil 并不是原元素的值为nil, 而是atomic.LoadPointer(&e.p) 的值为nil, (元素的nil在unsafe.Pointer是有值的), 所以更新改元素值
	ic := i
	for {
		if atomic.CompareAndSwapPointer(&e.p, nil, unsafe.Pointer(&ic)) {
			return i, false, true
		}
		p = atomic.LoadPointer(&e.p)
		if p == expunged {
			return nil, false, false
		}
		if p != nil {
			return *(*interface{})(p), true, true
		}
	}
}

// LoadAndDelete deletes the value for a key, returning the previous value if any.
// The loaded result reports whether the key was present.
func (m *Map) LoadAndDelete(key interface{}) (value interface{}, loaded bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		// read map 中不存在， 并且 read.amended == true 说明 dirty map 不为空
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// 这里没有对ok 值判断，说明无论 dirty map 中是否存在，都执行删除操作
			delete(m.dirty, key)
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if ok {
		// 在read map 中命中，则直接将其删除（nil）
		return e.delete()
	}
	return nil, false
}

// Delete deletes the value for a key.
func (m *Map) Delete(key interface{}) {
	m.LoadAndDelete(key)
}

// 将元素置为nil
func (e *entry) delete() (value interface{}, ok bool) {
	for {
		p := atomic.LoadPointer(&e.p)
		// 已被标记为删除
		if p == nil || p == expunged {
			return nil, false
		}
		// 置为nil
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return *(*interface{})(p), true
		}
	}
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, Range may reflect any mapping for that key
// from any point during the Range call.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
func (m *Map) Range(f func(key, value interface{}) bool) {
	// We need to be able to iterate over all of the keys that were already
	// present at the start of the call to Range.
	// If read.amended is false, then read.m satisfies that property without
	// requiring us to hold m.mu for a long time.
	read, _ := m.read.Load().(readOnly)
	// dirty map 中有数据
	if read.amended {
		// m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
		// (assuming the caller does not break out early), so a call to Range
		// amortizes an entire copy of the map: we can promote the dirty copy
		// immediately!
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		if read.amended {
			// 把 dirty map 晋升为 read map
			read = readOnly{m: m.dirty}
			m.read.Store(read)
			// dirty map置空
			m.dirty = nil
			// 计数清0
			m.misses = 0
		}
		m.mu.Unlock()
	}

	// 遍历 read map
	for k, e := range read.m {
		v, ok := e.load()
		// 忽略被删除的
		if !ok {
			continue
		}
		// 返回返回 false, 就终止
		if !f(k, v) {
			break
		}
	}
}

func (m *Map) missLocked() {
	// 每次调用自增 +1
	m.misses++
	// 判断 misses 是否超过 dirty map 长度，超过就晋升为 read map， 没有就直接返回
	if m.misses < len(m.dirty) {
		return
	}
	// dirty map 晋升为 read map
	m.read.Store(readOnly{m: m.dirty})
	// 清空 dirty map
	m.dirty = nil
	// 重置 misses 计数
	m.misses = 0
}

// 把 read map 复制到 dirty map 中
func (m *Map) dirtyLocked() {
	if m.dirty != nil {
		return
	}

	read, _ := m.read.Load().(readOnly)
	m.dirty = make(map[interface{}]*entry, len(read.m))
	for k, e := range read.m {
		if !e.tryExpungeLocked() {
			m.dirty[k] = e
		}
	}
}

func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := atomic.LoadPointer(&e.p)
	for p == nil {
		if atomic.CompareAndSwapPointer(&e.p, nil, expunged) {
			return true
		}
		p = atomic.LoadPointer(&e.p)
	}
	return p == expunged
}
