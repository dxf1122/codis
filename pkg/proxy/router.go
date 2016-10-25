// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package proxy

import (
	"sync"

	"github.com/CodisLabs/codis/pkg/models"
	"github.com/CodisLabs/codis/pkg/utils/errors"
	"github.com/CodisLabs/codis/pkg/utils/log"
)

const MaxSlotNum = models.MaxSlotNum

type Router struct {
	mu sync.RWMutex

	pool struct {
		primary *sharedBackendConnPool
		replica *sharedBackendConnPool
	}
	slots [MaxSlotNum]Slot

	config *Config
	online bool
	closed bool
}

func NewRouter(config *Config) *Router {
	s := &Router{config: config}
	s.pool.primary = newSharedBackendConnPool(config.BackendPrimaryParallel)
	s.pool.replica = newSharedBackendConnPool(config.BackendReplicaParallel)
	for i := range s.slots {
		s.slots[i].id = i
	}
	return s
}

func (s *Router) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.online = true
}

func (s *Router) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true

	for i := range s.slots {
		s.fillSlot(&models.Slot{Id: i}, false)
	}
}

func (s *Router) GetGroupIds() map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var groups = make(map[int]bool)
	for i := range s.slots {
		if gid := s.slots[i].backend.id; gid != 0 {
			groups[gid] = true
		}
		if gid := s.slots[i].migrate.id; gid != 0 {
			groups[gid] = true
		}
	}
	return groups
}

func (s *Router) GetSlots() []*models.Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	slots := make([]*models.Slot, MaxSlotNum)
	for i := range s.slots {
		slots[i] = s.slots[i].snapshot(true)
	}
	return slots
}

func (s *Router) GetSlot(id int) *models.Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id < 0 || id >= MaxSlotNum {
		return nil
	}
	slot := &s.slots[id]
	return slot.snapshot(true)
}

func (s *Router) HasSwitched() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.slots {
		if s.slots[i].switched {
			return true
		}
	}
	return false
}

var (
	ErrClosedRouter  = errors.New("use of closed router")
	ErrInvalidSlotId = errors.New("use of invalid slot id")
)

func (s *Router) FillSlot(m *models.Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedRouter
	}
	if m.Id < 0 || m.Id >= MaxSlotNum {
		return ErrInvalidSlotId
	}
	s.fillSlot(m, false)
	return nil
}

func (s *Router) KeepAlive() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosedRouter
	}
	s.pool.primary.KeepAliveAll()
	s.pool.replica.KeepAliveAll()
	return nil
}

func (s *Router) isOnline() bool {
	return s.online && !s.closed
}

func (s *Router) dispatch(r *Request) error {
	hkey := getHashKey(r.Multi, r.OpStr)
	var id = Hash(hkey) % MaxSlotNum
	slot := &s.slots[id]
	return slot.forward(r, hkey)
}

func (s *Router) dispatchSlot(r *Request, id int) error {
	if id < 0 || id >= MaxSlotNum {
		return ErrInvalidSlotId
	}
	slot := &s.slots[id]
	return slot.forward(r, nil)
}

func (s *Router) dispatchAddr(r *Request, addr string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var seed = r.Seed()
	if bc := s.pool.primary.Get(addr).BackendConn(seed, false); bc != nil {
		bc.PushBack(r)
		return true
	}
	if bc := s.pool.replica.Get(addr).BackendConn(seed, false); bc != nil {
		bc.PushBack(r)
		return true
	}
	return false
}

func (s *Router) fillSlot(m *models.Slot, switched bool) {
	slot := &s.slots[m.Id]
	slot.blockAndWait()

	slot.backend.bc.Release()
	slot.backend.bc = nil
	slot.backend.id = 0
	slot.migrate.bc.Release()
	slot.migrate.bc = nil
	slot.migrate.id = 0
	for i := range slot.replicaGroups {
		for _, bc := range slot.replicaGroups[i] {
			bc.Release()
		}
	}
	slot.replicaGroups = nil

	slot.switched = switched

	if addr := m.BackendAddr; len(addr) != 0 {
		slot.backend.bc = s.pool.primary.Retain(addr, s.config)
		slot.backend.id = m.BackendAddrGroupId
	}
	if from := m.MigrateFrom; len(from) != 0 {
		slot.migrate.bc = s.pool.primary.Retain(from, s.config)
		slot.migrate.id = m.MigrateFromGroupId
	}
	for i := range m.ReplicaGroups {
		var group []*sharedBackendConn
		for _, addr := range m.ReplicaGroups[i] {
			group = append(group, s.pool.replica.Retain(addr, s.config))
		}
		if len(group) == 0 {
			continue
		}
		slot.replicaGroups = append(slot.replicaGroups, group)
	}

	if !m.Locked {
		slot.unblock()
	}
	if !s.closed {
		if slot.migrate.bc != nil {
			log.Warnf("fill   slot %04d, backend.addr = %s, migrate.from = %s, locked = %t",
				slot.id, slot.backend.bc.Addr(), slot.migrate.bc.Addr(), slot.lock.hold)
		} else {
			log.Warnf("fill   slot %04d, backend.addr = %s, locked = %t",
				slot.id, slot.backend.bc.Addr(), slot.lock.hold)
		}
	}
}

func (s *Router) SwitchMasters(masters map[int]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedRouter
	}
	for i := range s.slots {
		s.trySwitchMaster(i, masters)
	}
	return nil
}

func (s *Router) trySwitchMaster(id int, masters map[int]string) {
	var update bool
	var m = s.slots[id].snapshot(false)

	if addr := masters[m.BackendAddrGroupId]; addr != "" {
		if addr != m.BackendAddr {
			m.BackendAddr = addr
			update = true
		}
	}
	if from := masters[m.MigrateFromGroupId]; from != "" {
		if from != m.MigrateFrom {
			m.MigrateFrom = from
			update = true
		}
	}
	if !update {
		return
	}
	log.Warnf("slot %04d +switch-master", id)

	s.fillSlot(m, true)
}
