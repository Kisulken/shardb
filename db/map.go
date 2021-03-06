package db

// Original idea of the concurrent map was taken from https://github.com/orcaman/concurrent-map

import (
	"errors"
	"github.com/rs/xid"
	"io/ioutil"
	"math/rand"
	"os"
	"strconv"
	"sync"
)

// Every collection will be split along %SHARD_COUNT% files
var SHARD_COUNT = 32

// A "thread" safe map of type string:Anything.
// To avoid lock bottlenecks this map is dived to several (SHARD_COUNT) map shards.

type ConcurrentMap struct {
	Shared []*ConcurrentMapShared

	counter         uint64
	counterMx       sync.Mutex
	SyncDestination string
}

type ShardOffset struct {
	Start   int64 `json:"s"`
	Length  int   `json:"l"`
	Deleted bool  `json:"!,omitempty"`
}

func (cm *ConcurrentMap) GetRandomShard() *ConcurrentMapShared {
	return cm.Shared[rand.Intn(len(cm.Shared))]
}

// Flushes all data to the drive and then reopens the file
func (cm *ConcurrentMap) Flush() error {
	for _, shard := range cm.Shared {
		shard.Lock()
		shard.file.Close()
		f, err := os.OpenFile(shard.SyncDestination+"/shard_"+strconv.Itoa(shard.Id)+".gobs", os.O_RDWR, os.ModePerm)
		if err != nil {
			shard.Unlock()
			return err
		}
		shard.file = f
		shard.Unlock()
	}
	return nil
}

// deletes redundant data from the drive
// n - total sized of the data that has been removed
func (cm *ConcurrentMap) OptimizeShards() (n int64, err error) {
	for _, shard := range cm.Shared {
		n, err = shard.Optimize()
		if err != nil {
			return n, err
		}
	}
	return
}

func (cm *ConcurrentMap) SetCounterIndex(value uint64) error {
	if value >= uint64(SHARD_COUNT) || value < 0 {
		return errors.New("invalid value")
	}
	cm.counterMx.Lock()
	cm.counter = value
	cm.counterMx.Unlock()
	return nil
}

// synchronizes database with the drive
func (cm *ConcurrentMap) Sync() (err error) {
	for _, shard := range cm.Shared {
		err = shard.Sync()
		if err != nil {
			return err
		}
	}
	cm.counterMx.Lock()
	err = ioutil.WriteFile(cm.SyncDestination+"/map.index",
		[]byte(strconv.FormatUint(cm.counter, 10)+"\n"+cm.SyncDestination), os.ModePerm)
	cm.counterMx.Unlock()
	return err
}

// Creates a new concurrent map.
func NewConcurrentMap(syncDest string, files []*os.File) *ConcurrentMap {
	m := &ConcurrentMap{make([]*ConcurrentMapShared, SHARD_COUNT),
		0, sync.Mutex{}, syncDest}
	for i := 0; i < SHARD_COUNT; i++ {
		m.Shared[i] = NewConcurrentMapShared(syncDest, i, files[i])
	}
	return m
}

// Returns shard under given key
func (m *ConcurrentMap) GetShard(key string) *ConcurrentMapShared {
	return m.Shared[uint(fnv32(key))%uint(SHARD_COUNT)]
}

func (m *ConcurrentMap) GetNextShard() *ConcurrentMapShared {
	m.counterMx.Lock()
	defer m.counterMx.Unlock()

	m.counter++
	if m.counter >= 32 {
		m.counter = 0
	}
	return m.Shared[m.counter]
}

func (m *ConcurrentMap) ReadAtOffset(shard *ConcurrentMapShared, offset *ShardOffset) ([]byte, error) {
	data := make([]byte, offset.Length)
	_, err := shard.file.ReadAt(data, offset.Start)
	return data, err
}

func (m *ConcurrentMap) RestoreByKey(key, value string, limit int) int {
	counter := 0
	for n := 0; n < SHARD_COUNT; n++ {
		shard := m.Shared[n]
		shard.Lock()
		kv := key + ":" + value
		en := ":" + kv
		length := shard.GetCapacityKey(kv)
		tempKey := ""
		for i := length - 1; i >= 0; i-- {
			tempKey = strconv.Itoa(i) + en
			if item, ok := shard.Items[tempKey]; ok {
				if !item.Deleted {
					continue
				}
				item.Deleted = false
				counter++
				if counter == limit {
					shard.Unlock()
					return limit
				}
			} else {
				break
			}
			i++
		}
		shard.SetCapacityKey(kv, length+counter)
		shard.Unlock()
	}
	return counter
}

func (m *ConcurrentMap) RestoreByUniqueKey(shard *ConcurrentMapShared, key, value string) error {
	shard.Lock()
	defer shard.Unlock()
	if item, ok := shard.Items[key+":"+value]; ok {
		item.Deleted = false
		return nil
	}
	return errors.New("object footprint was already evicted")
}

func (m *ConcurrentMap) DeleteById(shard *ConcurrentMapShared, id string) error {
	return m.DeleteByUniqueKey(shard, "id", id)
}

func (m *ConcurrentMap) DeleteByUniqueKey(shard *ConcurrentMapShared, key, value string) error {
	shard.Lock()
	defer shard.Unlock()
	if item, ok := shard.Items[key+":"+value]; ok {
		item.Deleted = true
		return nil
	}
	return errors.New("object under specified unique key was not found")
}

func (m *ConcurrentMap) DeleteByKey(key, value string, limit int) (deletedDests []string) {
	counter := 0
	deletedDests = make([]string, 0)
	for n := 0; n < SHARD_COUNT; n++ {
		shard := m.Shared[n]
		shard.Lock()
		kv := key + ":" + value
		en := ":" + kv
		length := shard.GetCapacityKey(kv)
		tempKey := ""
		for i := length - 1; i >= 0; i-- {
			tempKey = strconv.Itoa(i) + en
			if item, ok := shard.Items[tempKey]; ok {
				if item.Deleted {
					continue
				}
				item.Deleted = true
				deletedDests = append(deletedDests, tempKey)
				counter++
				if counter == limit {
					shard.Unlock()
					return deletedDests
				}
			} else {
				break
			}
			i++
		}
		shard.Unlock()
	}
	return deletedDests
}

func (m *ConcurrentMap) FindById(shard *ConcurrentMapShared, id string) ([]byte, error) {
	return m.FindByUniqueKey(shard, "id", id)
}

func (m *ConcurrentMap) FindByUniqueKey(shard *ConcurrentMapShared, key, value string) ([]byte, error) {
	shard.RLock()
	defer shard.RUnlock()

	if item, ok := shard.Items[key+":"+value]; ok {
		return m.ReadAtOffset(shard, item)
	}
	return nil, errors.New("not found")
}

func (m *ConcurrentMap) FindByKeyInShard(shard *ConcurrentMapShared, key, value string, limit int) ([][]byte, error) {
	shard.RLock()
	defer shard.RUnlock()

	kv := ":" + key + ":" + value
	results := make([][]byte, 0, limit)
	i := 0
	for {
		if item, ok := shard.Items[strconv.Itoa(i)+kv]; ok {
			if item.Deleted {
				continue
			}
			data, err := m.ReadAtOffset(shard, item)
			if err != nil {
				return nil, err
			}
			results = append(results, data)
			if len(results) == limit {
				return results, nil
			}
		} else {
			break
		}
		i++
	}
	return results, nil
}

func (m *ConcurrentMap) FindByKey(key, value string, limit int) ([][]byte, error) {
	results := make([][]byte, 0, limit)
	kv := ":" + key + ":" + value
	for n := 0; n < SHARD_COUNT; n++ {
		shard := m.Shared[n]
		shard.Lock()
		i := 0
		for {
			if item, ok := shard.Items[strconv.Itoa(i)+kv]; ok {
				if item.Deleted {
					continue
				}
				data, err := m.ReadAtOffset(shard, item)
				if err != nil {
					shard.Unlock()
					return nil, err
				}
				results = append(results, data)
				if len(results) == limit {
					shard.Unlock()
					return results, nil
				}
			} else {
				break
			}
			i++
		}
		shard.Unlock()
	}
	return results, nil
}

func (m *ConcurrentMap) Set(indexData []*FullDataIndex, value interface{}) (map[string]*int, error) {
	idStr := xid.New().String()
	// marshal the payload
	elem := Element{idStr, value}
	encodedData, err := EncodeGob(elem)
	if err != nil {
		return nil, err
	}
	// get map shard
	shard := m.GetNextShard()
	shard.Lock()
	defer shard.Unlock()
	// write to the end of the file
	ret, err := shard.file.Seek(0, 2)
	if err != nil {
		return nil, err
	}
	// write encoded data to the file
	n := 0
	n, err = shard.file.Write(encodedData)
	if err != nil {
		return nil, err
	}
	// write "next line" symbol to the file
	destMap := make(map[string]*int)
	pId := &shard.Id

	offset := ShardOffset{ret, n, false}
	if indexData != nil {
		for _, ix := range indexData {
			fullKey := ix.Field + ":" + ix.Data
			// Unique index key
			if ix.Unique {
				if _, ok := shard.Items[fullKey]; ok {
					return nil, errors.New("unique primary key duplicate")
				}
				shard.Items[fullKey] = &offset
				destMap[fullKey] = pId
			} else {
				// Regular key
				index := shard.GetCapacityKey(fullKey)
				lastAvailable := ""
				for {
					lastAvailable = strconv.Itoa(index) + ":" + fullKey
					if _, ok := shard.Items[lastAvailable]; ok {
						index++
					} else {
						break
					}
				}
				shard.Items[lastAvailable] = &offset
				shard.SetCapacityKey(fullKey, index)
				destMap[lastAvailable] = pId
			}
		}
	}
	idKey := "id:" + idStr
	shard.Items[idKey] = &offset
	destMap[idKey] = pId
	return destMap, nil
}

// Retrieves an element from map under given key.
func (m *ConcurrentMap) Get(key string) (*ShardOffset, bool) {
	// Get shard
	shard := m.GetShard(key)
	shard.RLock()
	// Get item from the shard.
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

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < len(key); i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
}
