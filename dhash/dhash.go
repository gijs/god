package dhash

import (
	"../client"
	"../common"
	"../discord"
	"../murmur"
	"../radix"
	"../timenet"
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type SyncListener func(dhash *Node, fetched, distributed int)
type CleanListener func(dhash *Node, cleaned, redistributed int)
type MigrateListener func(dhash *Node, source, destination []byte)

const (
	syncInterval    = time.Second * 1
	cleanInterval   = time.Second * 1
	migrateInterval = time.Second * 1
	migrateLimit    = 3
	migrateTarget   = 2
	migrateWait     = 10
)

const (
	created = iota
	started
	stopped
)

type Node struct {
	state            int32
	lastClean        int64
	lastSync         int64
	lastMigrate      int64
	lock             *sync.RWMutex
	syncListeners    []SyncListener
	cleanListeners   []CleanListener
	migrateListeners []MigrateListener
	node             *discord.Node
	timer            *timenet.Timer
	tree             *radix.Tree
}

func NewNode(addr string) (result *Node) {
	result = &Node{
		node:  discord.NewNode(addr),
		lock:  new(sync.RWMutex),
		state: created,
		tree:  radix.NewTree(),
	}
	result.timer = timenet.NewTimer((*dhashPeerProducer)(result))
	result.node.Export("Timenet", (*timerServer)(result.timer))
	result.node.Export("DHash", (*dhashServer)(result))
	result.node.Export("HashTree", (*hashTreeServer)(result.tree))
	return
}
func (self *Node) AddCleanListener(l CleanListener) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.cleanListeners = append(self.cleanListeners, l)
}
func (self *Node) AddMigrateListener(l MigrateListener) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.migrateListeners = append(self.migrateListeners, l)
}
func (self *Node) AddSyncListener(l SyncListener) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.syncListeners = append(self.syncListeners, l)
}
func (self *Node) hasState(s int32) bool {
	return atomic.LoadInt32(&self.state) == s
}
func (self *Node) changeState(old, neu int32) bool {
	return atomic.CompareAndSwapInt32(&self.state, old, neu)
}
func (self *Node) GetAddr() string {
	return self.node.GetAddr()
}
func (self *Node) AddChangeListener(f common.RingChangeListener) {
	self.node.AddChangeListener(f)
}
func (self *Node) Stop() {
	if self.changeState(started, stopped) {
		self.node.Stop()
		self.timer.Stop()
	}
}
func (self *Node) Start() (err error) {
	if !self.changeState(created, started) {
		return fmt.Errorf("%v can only be started when in state 'created'", self)
	}
	if err = self.node.Start(); err != nil {
		return
	}
	self.timer.Start()
	go self.syncPeriodically()
	go self.cleanPeriodically()
	go self.migratePeriodically()
	return
}
func (self *Node) sync() {
	fetched := 0
	distributed := 0
	nextSuccessor := self.node.GetSuccessor()
	for i := 0; i < self.node.Redundancy()-1; i++ {
		distributed += radix.NewSync(self.tree, (remoteHashTree)(nextSuccessor)).From(self.node.GetPredecessor().Pos).To(self.node.GetPosition()).Run().PutCount()
		fetched += radix.NewSync((remoteHashTree)(nextSuccessor), self.tree).From(self.node.GetPredecessor().Pos).To(self.node.GetPosition()).Run().PutCount()
		nextSuccessor = self.node.GetSuccessorFor(nextSuccessor.Pos)
	}
	if fetched != 0 || distributed != 0 {
		atomic.StoreInt64(&self.lastSync, time.Now().UnixNano())
		self.lock.RLock()
		defer self.lock.RUnlock()
		for _, l := range self.syncListeners {
			l(self, fetched, distributed)
		}
	}
}
func (self *Node) migratePeriodically() {
	for self.hasState(started) {
		self.migrate()
		time.Sleep(migrateInterval)
	}
}
func (self *Node) syncPeriodically() {
	for self.hasState(started) {
		self.sync()
		time.Sleep(syncInterval)
	}
}
func (self *Node) cleanPeriodically() {
	for self.hasState(started) {
		self.clean()
		time.Sleep(cleanInterval)
	}
}
func (self *Node) treeSizeBetween(p1, p2 []byte) int {
	if bytes.Compare(p1, p2) < 1 {
		return self.tree.SizeBetween(p1, p2, true, false)
	}
	return self.tree.SizeBetween(p1, nil, true, false) + self.tree.SizeBetween(nil, p2, true, false)
}
func (self *Node) changePosition(newPos []byte) {
	for len(newPos) < murmur.Size {
		newPos = append(newPos, 0)
	}
	oldPos := self.node.GetPosition()
	self.node.SetPosition(newPos)
	atomic.StoreInt64(&self.lastMigrate, time.Now().UnixNano())
	self.lock.RLock()
	defer self.lock.RUnlock()
	for _, l := range self.migrateListeners {
		l(self, oldPos, newPos)
	}
}
func (self *Node) migrate() {
	lastAllowedChange := time.Now().Add(-1 * migrateWait * time.Second).UnixNano()
	if lastAllowedChange > common.Max64(atomic.LoadInt64(&self.lastSync), atomic.LoadInt64(&self.lastClean), atomic.LoadInt64(&self.lastMigrate)) {
		// If we are not busy synchronizing or cleaning
		pred := self.node.GetPredecessor()
		grandPred := self.node.GetPredecessorFor(pred.Pos)
		mySize := self.treeSizeBetween(pred.Pos, self.node.GetPosition())
		predSize := common.Max(1, self.treeSizeBetween(grandPred.Pos, pred.Pos))
		// If we have more keys than our predecessor * migrateLimit
		if mySize > predSize*migrateLimit {
			// We want to have predecessor size * migrateTarget keys
			goalSize := predSize * migrateTarget
			if bytes.Compare(pred.Pos, self.node.GetPosition()) < 1 {
				if newPos, _, _, _, existed := self.tree.NextIndex(self.tree.SizeBetween(nil, pred.Pos, true, false) + goalSize); existed {
					self.changePosition(newPos)
				}
			} else {
				sizeBetweenPredAndZero := self.tree.SizeBetween(pred.Pos, nil, true, false)
				if sizeBetweenPredAndZero < goalSize {
					if newPos, _, _, _, existed := self.tree.NextIndex(goalSize - sizeBetweenPredAndZero); existed {
						self.changePosition(newPos)
					}
				} else {
					if newPos, _, _, _, existed := self.tree.NextIndex(self.tree.SizeBetween(self.node.GetPosition(), pred.Pos, true, false) + goalSize); existed {
						self.changePosition(newPos)
					}
				}
			}
		}
	}
}
func (self *Node) circularNext(key []byte) (nextKey []byte, existed bool) {
	if nextKey, _, _, existed = self.tree.Next(key); existed {
		return
	}
	nextKey = make([]byte, murmur.Size)
	if _, _, existed = self.tree.Get(nextKey); existed {
		return
	}
	nextKey, _, _, existed = self.tree.Next(nextKey)
	return
}
func (self *Node) owners(key []byte) (owners common.Remotes, isOwner bool) {
	owners = append(owners, self.node.GetSuccessorFor(key))
	if owners[0].Addr == self.node.GetAddr() {
		isOwner = true
	}
	for i := 1; i < self.node.Redundancy(); i++ {
		owners = append(owners, self.node.GetSuccessorFor(owners[i-1].Pos))
		if owners[i].Addr == self.node.GetAddr() {
			isOwner = true
		}
	}
	return
}
func (self *Node) clean() {
	deleted := 0
	put := 0
	if nextKey, existed := self.circularNext(self.node.GetPosition()); existed {
		if owners, isOwner := self.owners(nextKey); !isOwner {
			var sync *radix.Sync
			for index, owner := range owners {
				sync = radix.NewSync(self.tree, (remoteHashTree)(owner)).From(nextKey).To(owners[0].Pos)
				if index == len(owners)-2 {
					sync.Destroy()
				}
				sync.Run()
				deleted += sync.DelCount()
				put += sync.PutCount()
			}
		}
		if deleted != 0 || put != 0 {
			atomic.StoreInt64(&self.lastClean, time.Now().UnixNano())
			self.lock.RLock()
			defer self.lock.RUnlock()
			for _, l := range self.cleanListeners {
				l(self, deleted, put)
			}
		}
	}
}
func (self *Node) MustStart() *Node {
	if err := self.Start(); err != nil {
		panic(err)
	}
	return self
}
func (self *Node) MustJoin(addr string) {
	self.timer.Conform(remotePeer(common.Remote{Addr: addr}))
	self.node.MustJoin(addr)
}
func (self *Node) Time() time.Time {
	return time.Unix(0, self.timer.ContinuousTime())
}
func (self *Node) Description() common.DHashDescription {
	return common.DHashDescription{
		LastClean:    time.Unix(0, atomic.LoadInt64(&self.lastClean)),
		LastSync:     time.Unix(0, atomic.LoadInt64(&self.lastSync)),
		LastMigrate:  time.Unix(0, atomic.LoadInt64(&self.lastMigrate)),
		Timer:        self.timer.ActualTime(),
		OwnedEntries: self.tree.SizeBetween(self.node.GetPredecessor().Pos, self.node.GetPosition(), true, false),
		HeldEntries:  self.tree.Size(),
		Nodes:        self.node.GetNodes(),
	}
}
func (self *Node) Describe() string {
	return self.Description().Describe()
}
func (self *Node) DescribeTree() string {
	return self.tree.Describe()
}
func (self *Node) client() *client.Conn {
	return client.NewConnRing(common.NewRingNodes(self.node.Nodes()))
}
func (self *Node) SubFind(data common.Item, result *common.Item) error {
	*result = data
	result.Value, result.Exists = self.client().Get(data.Key)
	return nil
}
func (self *Node) Find(data common.Item, result *common.Item) error {
	*result = data
	result.Value, result.Exists = self.client().Get(data.Key)
	return nil
}
func (self *Node) Prev(data common.Item, result *common.Item) error {
	*result = data
	if key, value, timestamp, exists := self.tree.Prev(data.Key); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Key, result.Value, result.Timestamp, result.Exists = key, []byte(byteHasher), timestamp, exists
		}
	}
	return nil
}
func (self *Node) RingHash(x int, ringHash *[]byte) error {
	*ringHash = self.node.RingHash()
	return nil
}
func (self *Node) Count(r common.Range, result *int) error {
	*result = self.tree.SubSizeBetween(r.Key, r.Min, r.Max, r.MinInc, r.MaxInc)
	return nil
}
func (self *Node) Next(data common.Item, result *common.Item) error {
	*result = data
	if key, value, timestamp, exists := self.tree.Next(data.Key); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Key, result.Value, result.Timestamp, result.Exists = key, []byte(byteHasher), timestamp, exists
		}
	}
	return nil
}
func (self *Node) Last(data common.Item, result *common.Item) error {
	if key, value, timestamp, exists := self.tree.SubLast(data.Key); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Key, result.Value, result.Timestamp, result.Exists = key, []byte(byteHasher), timestamp, exists
		}
	}
	return nil
}
func (self *Node) PrevIndex(data common.Item, result *common.Item) error {
	if key, value, timestamp, index, exists := self.tree.SubPrevIndex(data.Key, data.Index); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Key, result.Value, result.Timestamp, result.Index, result.Exists = key, []byte(byteHasher), timestamp, index, exists
		}
	}
	return nil
}
func (self *Node) NextIndex(data common.Item, result *common.Item) error {
	if key, value, timestamp, index, exists := self.tree.SubNextIndex(data.Key, data.Index); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Key, result.Value, result.Timestamp, result.Index, result.Exists = key, []byte(byteHasher), timestamp, index, exists
		}
	}
	return nil
}
func (self *Node) First(data common.Item, result *common.Item) error {
	if key, value, timestamp, exists := self.tree.SubFirst(data.Key); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Key, result.Value, result.Timestamp, result.Exists = key, []byte(byteHasher), timestamp, exists
		}
	}
	return nil
}
func (self *Node) SubPrev(data common.Item, result *common.Item) error {
	if key, value, timestamp, exists := self.tree.SubPrev(data.Key, data.SubKey); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Key, result.Value, result.Timestamp, result.Exists = key, []byte(byteHasher), timestamp, exists
		}
	}
	return nil
}
func (self *Node) SubNext(data common.Item, result *common.Item) error {
	if key, value, timestamp, exists := self.tree.SubNext(data.Key, data.SubKey); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Key, result.Value, result.Timestamp, result.Exists = key, []byte(byteHasher), timestamp, exists
		}
	}
	return nil
}
func (self *Node) Get(data common.Item, result *common.Item) error {
	*result = data
	if value, timestamp, exists := self.tree.Get(data.Key); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Exists, result.Value, result.Timestamp = true, []byte(byteHasher), timestamp
		}
	}
	return nil
}
func (self *Node) SliceIndex(r common.Range, items *[]common.Item) error {
	min := &r.MinIndex
	max := &r.MaxIndex
	if !r.MinInc {
		min = nil
	}
	if !r.MaxInc {
		max = nil
	}
	self.tree.SubEachBetweenIndex(r.Key, min, max, func(key []byte, value radix.Hasher, version int64, index int) bool {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			*items = append(*items, common.Item{
				Key:       key,
				Value:     []byte(byteHasher),
				Timestamp: version,
				Index:     index,
			})
		}
		return true
	})
	return nil
}
func (self *Node) ReverseSliceIndex(r common.Range, items *[]common.Item) error {
	min := &r.MinIndex
	max := &r.MaxIndex
	if !r.MinInc {
		min = nil
	}
	if !r.MaxInc {
		max = nil
	}
	self.tree.SubReverseEachBetweenIndex(r.Key, min, max, func(key []byte, value radix.Hasher, version int64, index int) bool {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			*items = append(*items, common.Item{
				Key:       key,
				Value:     []byte(byteHasher),
				Timestamp: version,
				Index:     index,
			})
		}
		return true
	})
	return nil
}
func (self *Node) ReverseSlice(r common.Range, items *[]common.Item) error {
	self.tree.SubReverseEachBetween(r.Key, r.Min, r.Max, r.MinInc, r.MaxInc, func(key []byte, value radix.Hasher, version int64) bool {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			*items = append(*items, common.Item{
				Key:       key,
				Value:     []byte(byteHasher),
				Timestamp: version,
			})
		}
		return true
	})
	return nil
}
func (self *Node) Slice(r common.Range, items *[]common.Item) error {
	self.tree.SubEachBetween(r.Key, r.Min, r.Max, r.MinInc, r.MaxInc, func(key []byte, value radix.Hasher, version int64) bool {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			*items = append(*items, common.Item{
				Key:       key,
				Value:     []byte(byteHasher),
				Timestamp: version,
			})
		}
		return true
	})
	return nil
}
func (self *Node) ReverseIndexOf(data common.Item, result *common.Index) error {
	result.N, result.Existed = self.tree.SubReverseIndexOf(data.Key, data.SubKey)
	return nil
}
func (self *Node) IndexOf(data common.Item, result *common.Index) error {
	result.N, result.Existed = self.tree.SubIndexOf(data.Key, data.SubKey)
	return nil
}
func (self *Node) SubGet(data common.Item, result *common.Item) error {
	*result = data
	if value, timestamp, exists := self.tree.SubGet(data.Key, data.SubKey); exists {
		if byteHasher, ok := value.(radix.ByteHasher); ok {
			result.Exists, result.Value, result.Timestamp = true, []byte(byteHasher), timestamp
		}
	}
	return nil
}
func (self *Node) SubPut(data common.Item) error {
	successor := self.node.GetSuccessorFor(data.Key)
	if successor.Addr != self.node.GetAddr() {
		var x int
		return successor.Call("DHash.SubPut", data, &x)
	}
	data.TTL, data.Timestamp = self.node.Redundancy(), self.timer.ContinuousTime()
	return self.subPut(data)
}
func (self *Node) Put(data common.Item) error {
	successor := self.node.GetSuccessorFor(data.Key)
	if successor.Addr != self.node.GetAddr() {
		var x int
		return successor.Call("DHash.Put", data, &x)
	}
	data.TTL, data.Timestamp = self.node.Redundancy(), self.timer.ContinuousTime()
	return self.put(data)
}
func (self *Node) forwardOperation(data common.Item, operation string) {
	data.TTL--
	successor := self.node.GetSuccessorFor(self.node.GetPosition())
	var x int
	err := successor.Call(operation, data, &x)
	for err != nil {
		self.node.RemoveNode(successor)
		successor = self.node.GetSuccessorFor(self.node.GetPosition())
		err = successor.Call(operation, data, &x)
	}
}
func (self *Node) subPut(data common.Item) error {
	if data.TTL > 1 {
		if data.Sync {
			self.forwardOperation(data, "DHash.SlaveSubPut")
		} else {
			go self.forwardOperation(data, "DHash.SlaveSubPut")
		}
	}
	self.tree.SubPut(data.Key, data.SubKey, radix.ByteHasher(data.Value), data.Timestamp)
	return nil
}
func (self *Node) put(data common.Item) error {
	if data.TTL > 1 {
		if data.Sync {
			self.forwardOperation(data, "DHash.SlavePut")
		} else {
			go self.forwardOperation(data, "DHash.SlavePut")
		}
	}
	self.tree.Put(data.Key, radix.ByteHasher(data.Value), data.Timestamp)
	return nil
}
