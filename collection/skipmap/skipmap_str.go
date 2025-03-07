// Copyright 2021 ByteDance Inc.
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

package skipmap

import (
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/bytedance/gopkg/internal/wyhash"
)

func hash(s string) uint64 {
	return wyhash.Sum64String(s)
}

// StringMap represents a map based on skip list in ascending order.
type StringMap struct {
	header *stringNode
	length int64
}

type stringNode struct {
	key   string
	score uint64
	value unsafe.Pointer
	next  []*stringNode
	mu    sync.Mutex
	flags bitflag
}

func newStringNode(key string, value interface{}, level int) *stringNode {
	n := &stringNode{
		key:   key,
		score: hash(key),
		next:  make([]*stringNode, level),
	}
	n.storeVal(value)
	return n
}

func (n *stringNode) storeVal(value interface{}) {
	atomic.StorePointer(&n.value, unsafe.Pointer(&value))
}

func (n *stringNode) loadVal() interface{} {
	return *(*interface{})(atomic.LoadPointer(&n.value))
}

// cmp return 1 if n is bigger, 0 if equal, else -1.
func (n *stringNode) cmp(score uint64, key string) int {
	if n.score > score {
		return 1
	} else if n.score == score {
		return cmpstring(n.key, key)
	}
	return -1
}

// loadNext return `n.next[i]`(atomic)
func (n *stringNode) loadNext(i int) *stringNode {
	return (*stringNode)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&n.next[i]))))
}

// storeNext same with `n.next[i] = value`(atomic)
func (n *stringNode) storeNext(i int, value *stringNode) {
	atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&n.next[i])), unsafe.Pointer(value))
}

// NewString return an empty int64 skipmap.
func NewString() *StringMap {
	h := newStringNode("", "", maxLevel)
	h.flags.SetTrue(fullyLinked)
	return &StringMap{
		header: h,
	}
}

// findNode takes a score and two maximal-height arrays then searches exactly as in a sequential skipmap.
// The returned preds and succs always satisfy preds[i] > score > succs[i].
// (without fullpath, if find the node will return immediately)
func (s *StringMap) findNode(key string, preds *[maxLevel]*stringNode, succs *[maxLevel]*stringNode) *stringNode {
	score := hash(key)
	x := s.header
	for i := maxLevel - 1; i >= 0; i-- {
		succ := x.loadNext(i)
		for succ != nil && succ.cmp(score, key) < 0 {
			x = succ
			succ = x.loadNext(i)
		}
		preds[i] = x
		succs[i] = succ

		// Check if the score already in the skipmap.
		if succ != nil && succ.cmp(score, key) == 0 {
			return succ
		}
	}
	return nil
}

// findNodeDelete takes a score and two maximal-height arrays then searches exactly as in a sequential skip-list.
// The returned preds and succs always satisfy preds[i] > score >= succs[i].
func (s *StringMap) findNodeDelete(key string, preds *[maxLevel]*stringNode, succs *[maxLevel]*stringNode) int {
	// lFound represents the index of the first layer at which it found a node.
	score := hash(key)
	lFound, x := -1, s.header
	for i := maxLevel - 1; i >= 0; i-- {
		succ := x.loadNext(i)
		for succ != nil && succ.cmp(score, key) < 0 {
			x = succ
			succ = x.loadNext(i)
		}
		preds[i] = x
		succs[i] = succ

		// Check if the score already in the skip list.
		if lFound == -1 && succ != nil && succ.cmp(score, key) == 0 {
			lFound = i
		}
	}
	return lFound
}

func unlockString(preds [maxLevel]*stringNode, highestLevel int) {
	var prevPred *stringNode
	for i := highestLevel; i >= 0; i-- {
		if preds[i] != prevPred { // the node could be unlocked by previous loop
			preds[i].mu.Unlock()
			prevPred = preds[i]
		}
	}
}

// Store sets the value for a key.
func (s *StringMap) Store(key string, value interface{}) {
	level := randomLevel()
	var preds, succs [maxLevel]*stringNode
	for {
		nodeFound := s.findNode(key, &preds, &succs)
		if nodeFound != nil { // indicating the score is already in the skip-list
			if !nodeFound.flags.Get(marked) {
				// We don't need to care about whether or not the node is fully linked,
				// just replace the value.
				// fmt.Printf("%v\n", nodeFound.loadVal())
				nodeFound.storeVal(value)
				return
			}
			// If the node is marked, represents some other goroutines is in the process of deleting this node,
			// we need to add this node in next loop.
			continue
		}

		// Add this node into skip list.
		var (
			highestLocked        = -1 // the highest level being locked by this process
			valid                = true
			pred, succ, prevPred *stringNode
		)
		for layer := 0; valid && layer < level; layer++ {
			pred = preds[layer]   // target node's previous node
			succ = succs[layer]   // target node's next node
			if pred != prevPred { // the node in this layer could be locked by previous loop
				pred.mu.Lock()
				highestLocked = layer
				prevPred = pred
			}
			// valid check if there is another node has inserted into the skip list in this layer during this process.
			// It is valid if:
			// 1. The previous node and next node both are not marked.
			// 2. The previous node's next node is succ in this layer.
			valid = !pred.flags.Get(marked) && (succ == nil || !succ.flags.Get(marked)) && pred.loadNext(layer) == succ
		}
		if !valid {
			unlockString(preds, highestLocked)
			continue
		}

		nn := newStringNode(key, value, level)
		for layer := 0; layer < level; layer++ {
			nn.next[layer] = succs[layer]
			preds[layer].storeNext(layer, nn)
		}
		nn.flags.SetTrue(fullyLinked)
		unlockString(preds, highestLocked)
		atomic.AddInt64(&s.length, 1)
	}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
func (s *StringMap) Load(key string) (value interface{}, ok bool) {
	score := hash(key)
	x := s.header
	for i := maxLevel - 1; i >= 0; i-- {
		nex := x.loadNext(i)
		for nex != nil && nex.cmp(score, key) < 0 {
			x = nex
			nex = x.loadNext(i)
		}

		// Check if the score already in the skip list.
		if nex != nil && nex.cmp(score, key) == 0 {
			if nex.flags.MGet(fullyLinked|marked, fullyLinked) {
				return nex.loadVal(), true
			}
			return nil, false
		}
	}
	return nil, false
}

// LoadAndDelete deletes the value for a key, returning the previous value if any.
// The loaded result reports whether the key was present.
func (s *StringMap) LoadAndDelete(key string) (value interface{}, loaded bool) {
	var (
		nodeToDelete *stringNode
		isMarked     bool // represents if this operation mark the node
		topLayer     = -1
		preds, succs [maxLevel]*stringNode
	)
	for {
		lFound := s.findNodeDelete(key, &preds, &succs)
		if isMarked || // this process mark this node or we can find this node in the skip list
			lFound != -1 && succs[lFound].flags.MGet(fullyLinked|marked, fullyLinked) && (len(succs[lFound].next)-1) == lFound {
			if !isMarked { // we don't mark this node for now
				nodeToDelete = succs[lFound]
				topLayer = lFound
				nodeToDelete.mu.Lock()
				if nodeToDelete.flags.Get(marked) {
					// The node is marked by another process,
					// the physical deletion will be accomplished by another process.
					nodeToDelete.mu.Unlock()
					return nil, false
				}
				nodeToDelete.flags.SetTrue(marked)
				isMarked = true
			}
			// Accomplish the physical deletion.
			var (
				highestLocked        = -1 // the highest level being locked by this process
				valid                = true
				pred, succ, prevPred *stringNode
			)
			for layer := 0; valid && (layer <= topLayer); layer++ {
				pred, succ = preds[layer], succs[layer]
				if pred != prevPred { // the node in this layer could be locked by previous loop
					pred.mu.Lock()
					highestLocked = layer
					prevPred = pred
				}
				// valid check if there is another node has inserted into the skip list in this layer
				// during this process, or the previous is deleted by another process.
				// It is valid if:
				// 1. the previous node exists.
				// 2. no another node has inserted into the skip list in this layer.
				valid = !pred.flags.Get(marked) && pred.loadNext(layer) == succ
			}
			if !valid {
				unlockString(preds, highestLocked)
				continue
			}
			for i := topLayer; i >= 0; i-- {
				// Now we own the `nodeToDelete`, no other goroutine will modify it.
				// So we don't need `nodeToDelete.loadNext`
				preds[i].storeNext(i, nodeToDelete.next[i])
			}
			nodeToDelete.mu.Unlock()
			unlockString(preds, highestLocked)
			atomic.AddInt64(&s.length, -1)
			return nodeToDelete.loadVal(), true
		}
		return nil, false
	}
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
func (s *StringMap) LoadOrStore(key string, value interface{}) (actual interface{}, loaded bool) {
	loadedval, ok := s.Load(key)
	if !ok {
		s.Store(key, value)
		return nil, false
	}
	return loadedval, true
}

// Delete deletes the value for a key.
func (s *StringMap) Delete(key string) {
	var (
		nodeToDelete *stringNode
		isMarked     bool // represents if this operation mark the node
		topLayer     = -1
		preds, succs [maxLevel]*stringNode
	)
	for {
		lFound := s.findNodeDelete(key, &preds, &succs)
		if isMarked || // this process mark this node or we can find this node in the skip list
			lFound != -1 && succs[lFound].flags.MGet(fullyLinked|marked, fullyLinked) && (len(succs[lFound].next)-1) == lFound {
			if !isMarked { // we don't mark this node for now
				nodeToDelete = succs[lFound]
				topLayer = lFound
				nodeToDelete.mu.Lock()
				if nodeToDelete.flags.Get(marked) {
					// The node is marked by another process,
					// the physical deletion will be accomplished by another process.
					nodeToDelete.mu.Unlock()
					return // false
				}
				nodeToDelete.flags.SetTrue(marked)
				isMarked = true
			}
			// Accomplish the physical deletion.
			var (
				highestLocked        = -1 // the highest level being locked by this process
				valid                = true
				pred, succ, prevPred *stringNode
			)
			for layer := 0; valid && (layer <= topLayer); layer++ {
				pred, succ = preds[layer], succs[layer]
				if pred != prevPred { // the node in this layer could be locked by previous loop
					pred.mu.Lock()
					highestLocked = layer
					prevPred = pred
				}
				// valid check if there is another node has inserted into the skip list in this layer
				// during this process, or the previous is deleted by another process.
				// It is valid if:
				// 1. the previous node exists.
				// 2. no another node has inserted into the skip list in this layer.
				valid = !pred.flags.Get(marked) && pred.loadNext(layer) == succ
			}
			if !valid {
				unlockString(preds, highestLocked)
				continue
			}
			for i := topLayer; i >= 0; i-- {
				// Now we own the `nodeToDelete`, no other goroutine will modify it.
				// So we don't need `nodeToDelete.loadNext`
				preds[i].storeNext(i, nodeToDelete.next[i])
			}
			nodeToDelete.mu.Unlock()
			unlockString(preds, highestLocked)
			atomic.AddInt64(&s.length, -1)
			return // true
		}
		return // false
	}
}

// Range calls f sequentially for each key and value present in the skipmap.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, Range may reflect any mapping for that key
// from any point during the Range call.
func (s *StringMap) Range(f func(key string, value interface{}) bool) {
	x := s.header.loadNext(0)
	for x != nil {
		if !x.flags.MGet(fullyLinked|marked, fullyLinked) {
			x = x.loadNext(0)
			continue
		}
		if !f(x.key, x.loadVal()) {
			break
		}
		x = x.loadNext(0)
	}
}

// Len return the length of this skipmap.
// Keep in sync with types_gen.go:lengthFunction
// Special case for code generation, Must in the tail of skipmap.go.
func (s *StringMap) Len() int {
	return int(atomic.LoadInt64(&s.length))
}
