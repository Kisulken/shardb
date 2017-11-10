package db

import (
	"encoding/json"
	"sync"
	"os"
	"strconv"
	"errors"
	"github.com/rs/xid"
	"math/rand"
)

var SHARD_COUNT = 32

// A "thread" safe map of type string:Anything.
// To avoid lock bottlenecks this map is dived to several (SHARD_COUNT) map shards.

type ConcurrentMap struct {
	Shared  []*ConcurrentMapShared
	counter uint64
	mx sync.Mutex
}

// A "thread" safe string to anything map.
type ConcurrentMapShared struct {
	id			 int
	Items        map[string]interface{}
	file		 *os.File
	sync.RWMutex // Read Write mutex, guards access to internal map.
}

type ShardOffset struct {
	start int64
	length int
}

//! Not intended to be used in production environment
func (shard *ConcurrentMapShared) GetRandomItem() (string, interface{}) {
	return shard.GetItemWithNumber(rand.Intn(len(shard.Items)))
}
//! Not intended to be used in production environment
func (shard *ConcurrentMapShared) GetItemWithNumber(n int) (string, interface{}) {
	i := 0
	for k, v := range shard.Items {
		if i >= n {
			return k, v
		}
		i++
	}
	return "", nil
}

func (shard *ConcurrentMapShared) GetItem(id string) interface{} {
	return shard.Items[id]
}

// Creates a new concurrent map.
func NewConcurrentMap(files []*os.File) *ConcurrentMap {
	m := &ConcurrentMap{make([]*ConcurrentMapShared, SHARD_COUNT), 0, sync.Mutex{}}
	for i := 0; i < SHARD_COUNT; i++ {
		m.Shared[i] = &ConcurrentMapShared{id: i, Items: make(map[string]interface{}), file: files[i]}
	}
	return m
}

// Returns shard under given key
func (m *ConcurrentMap) GetShard(key string) *ConcurrentMapShared {
	return m.Shared[uint(fnv32(key))%uint(SHARD_COUNT)]
}

func (m *ConcurrentMap) GetNextShard() *ConcurrentMapShared {
	m.mx.Lock()
	defer m.mx.Unlock()

	m.counter++
	if m.counter >= 32 {
		m.counter = 0
	}
	return m.Shared[m.counter]
}

func (m *ConcurrentMap) MSet(data map[string]interface{}) {
	for key, value := range data {
		shard := m.GetShard(key)
		shard.Lock()
		shard.Items[key] = value
		shard.Unlock()
	}
}

func (m *ConcurrentMap) FindById(shard *ConcurrentMapShared, key string, id string) ([]byte, error) {
	shard.RLock()
	defer shard.RUnlock()

	if item, ok := shard.Items[key + ":id:" + id]; ok {
		offset := item.(ShardOffset)
		data := make([]byte, offset.length)
		_, err := shard.file.ReadAt(data, offset.start)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, errors.New("not found")
}

// Sets the given value under the specified key.
// return shard id, object id
func (m *ConcurrentMap) Set(key string, indexData []IndexData, value interface{}) (int, string, error) {
	// Get map shard.
	shard := m.GetNextShard()
	shard.Lock()
	defer shard.Unlock()

	idStr := xid.New().String()
	// Marshal the payload
	data, err := json.Marshal(Element{idStr, value})
	if err != nil {
		return -1, "", err
	}

	// Write to the file
	ret, err := shard.file.Seek(0, 2)
	if err != nil {
		return -1, "", err
	}

	n, err := shard.file.WriteString(string(data) + "\n")
	if err != nil {
		return -1, "", err
	}

	// Write Id index if other indexes were not provided
	offset := ShardOffset{ret, n}
	if indexData == nil {
		shard.Items[key + ":id:" + idStr] = offset
	} else {
		// Or walk through the provided indexes otherwise
		for _, ix := range indexData {
			fullKey := key + ":" + ix.Field + ":" + ix.Data
			index := 0
			lastAvailable := ""
			for ; ; {
				lastAvailable = fullKey + "_" + strconv.Itoa(index)
				if _, ok := shard.Items[lastAvailable]; ok {
					index++
				} else {
					break
				}
			}
			shard.Items[lastAvailable] = offset
		}
	}

	return shard.id, idStr, nil
}

// Callback to return new element to be inserted into the map
// It is called while lock is held, therefore it MUST NOT
// try to access other keys in same map, as it can lead to deadlock since
// Go sync.RWLock is not reentrant
type UpsertCb func(exist bool, valueInMap interface{}, newValue interface{}) interface{}

// Insert or Update - updates existing element or inserts a new one using UpsertCb
func (m *ConcurrentMap) Upsert(key string, value interface{}, cb UpsertCb) (res interface{}) {
	shard := m.GetShard(key)
	shard.Lock()
	v, ok := shard.Items[key]
	res = cb(ok, v, value)
	shard.Items[key] = res
	shard.Unlock()
	return res
}

// Sets the given value under the specified key if no value was associated with it.
func (m *ConcurrentMap) SetIfAbsent(key string, value interface{}) bool {
	// Get map shard.
	shard := m.GetShard(key)
	shard.Lock()
	_, ok := shard.Items[key]
	if !ok {
		shard.Items[key] = value
	}
	shard.Unlock()
	return !ok
}

// Retrieves an element from map under given key.
func (m *ConcurrentMap) Get(key string) (interface{}, bool) {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// Get item from shard.
	val, ok := shard.Items[key]
	shard.RUnlock()
	return val, ok
}

// Returns the number of elements within the map.
func (m *ConcurrentMap) Count() int {
	count := 0
	for i := 0; i < SHARD_COUNT; i++ {
		shard := m.Shared[i]
		shard.RLock()
		count += len(shard.Items)
		shard.RUnlock()
	}
	return count
}

// Looks up an item under specified key
func (m *ConcurrentMap) Has(key string) bool {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// See if element is within shard.
	_, ok := shard.Items[key]
	shard.RUnlock()
	return ok
}

// Removes an element from the map.
func (m *ConcurrentMap) Remove(key string) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	delete(shard.Items, key)
	shard.Unlock()
}

// RemoveCb is a callback executed in a map.RemoveCb() call, while Lock is held
// If returns true, the element will be removed from the map
type RemoveCb func(key string, v interface{}, exists bool) bool

// RemoveCb locks the shard containing the key, retrieves its current value and calls the callback with those params
// If callback returns true and element exists, it will remove it from the map
// Returns the value returned by the callback (even if element was not present in the map)
func (m *ConcurrentMap) RemoveCb(key string, cb RemoveCb) bool {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	v, ok := shard.Items[key]
	remove := cb(key, v, ok)
	if remove && ok {
		delete(shard.Items, key)
	}
	shard.Unlock()
	return remove
}

// Removes an element from the map and returns it
func (m *ConcurrentMap) Pop(key string) (v interface{}, exists bool) {
	// Try to get shard.
	shard := m.GetShard(key)
	shard.Lock()
	v, exists = shard.Items[key]
	delete(shard.Items, key)
	shard.Unlock()
	return v, exists
}

// Checks if map is empty.
func (m *ConcurrentMap) IsEmpty() bool {
	return m.Count() == 0
}

// Used by the Iter & IterBuffered functions to wrap two variables together over a channel,
type Tuple struct {
	Key string
	Val interface{}
}

// Returns an iterator which could be used in a for range loop.
//
// Deprecated: using IterBuffered() will get a better performence
func (m *ConcurrentMap) Iter() <-chan Tuple {
	chans := snapshot(m)
	ch := make(chan Tuple)
	go fanIn(chans, ch)
	return ch
}

// Returns a buffered iterator which could be used in a for range loop.
func (m *ConcurrentMap) IterBuffered() <-chan Tuple {
	chans := snapshot(m)
	total := 0
	for _, c := range chans {
		total += cap(c)
	}
	ch := make(chan Tuple, total)
	go fanIn(chans, ch)
	return ch
}

// Returns a array of channels that contains elements in each shard,
// which likely takes a snapshot of `m`.
// It returns once the size of each buffered channel is determined,
// before all the channels are populated using goroutines.
func snapshot(m *ConcurrentMap) (chans []chan Tuple) {
	chans = make([]chan Tuple, SHARD_COUNT)
	wg := sync.WaitGroup{}
	wg.Add(SHARD_COUNT)
	// Foreach shard.
	for index, shard := range m.Shared {
		go func(index int, shard *ConcurrentMapShared) {
			// Foreach key, value pair.
			shard.RLock()
			chans[index] = make(chan Tuple, len(shard.Items))
			wg.Done()
			for key, val := range shard.Items {
				chans[index] <- Tuple{key, val}
			}
			shard.RUnlock()
			close(chans[index])
		}(index, shard)
	}
	wg.Wait()
	return chans
}

// fanIn reads elements from channels `chans` into channel `out`
func fanIn(chans []chan Tuple, out chan Tuple) {
	wg := sync.WaitGroup{}
	wg.Add(len(chans))
	for _, ch := range chans {
		go func(ch chan Tuple) {
			for t := range ch {
				out <- t
			}
			wg.Done()
		}(ch)
	}
	wg.Wait()
	close(out)
}

// Returns all items as map[string]interface{}
func (m *ConcurrentMap) Items() map[string]interface{} {
	tmp := make(map[string]interface{})

	// Insert items to temporary map.
	for item := range m.IterBuffered() {
		tmp[item.Key] = item.Val
	}

	return tmp
}

// Iterator callback,called for every key,value found in
// maps. RLock is held for all calls for a given shard
// therefore callback sess consistent view of a shard,
// but not across the shards
type IterCb func(key string, v interface{})

// Callback based iterator, cheapest way to read
// all elements in a map.
func (m *ConcurrentMap) IterCb(fn IterCb) {
	for idx := range m.Shared {
		shard := (m.Shared)[idx]
		shard.RLock()
		for key, value := range shard.Items {
			fn(key, value)
		}
		shard.RUnlock()
	}
}

// Return all keys as []string
func (m *ConcurrentMap) Keys() []string {
	count := m.Count()
	ch := make(chan string, count)
	go func() {
		// Foreach shard.
		wg := sync.WaitGroup{}
		wg.Add(SHARD_COUNT)
		for _, shard := range m.Shared {
			go func(shard *ConcurrentMapShared) {
				// Foreach key, value pair.
				shard.RLock()
				for key := range shard.Items {
					ch <- key
				}
				shard.RUnlock()
				wg.Done()
			}(shard)
		}
		wg.Wait()
		close(ch)
	}()

	// Generate keys
	keys := make([]string, 0, count)
	for k := range ch {
		keys = append(keys, k)
	}
	return keys
}

//Reviles ConcurrentMap "private" variables to json marshal.
/*func (m *ConcurrentMap) MarshalJSON() ([]byte, error) {
	// Create a temporary map, which will hold all item spread across shards.
	tmp := make(map[string]interface{})

	// Insert items to temporary map.
	for item := range m.IterBuffered() {
		tmp[item.Key] = item.Val
	}
	return json.Marshal(tmp)
}*/

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < len(key); i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
}
