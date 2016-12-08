// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cluster

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/m3db/m3cluster/shard"
	"github.com/m3db/m3db/sharding"
	"github.com/m3db/m3db/storage"
	"github.com/m3db/m3db/storage/namespace"
	"github.com/m3db/m3db/topology"
	"github.com/m3db/m3x/log"
)

var (
	// newStorageDatabase is the injected constructor to construct a database,
	// useful for replacing which database constructor is called in tests
	newStorageDatabase newStorageDatabaseFn = storage.NewDatabase

	errAlreadyWatchingTopology = errors.New("cluster db is already watching topology")
	errNotWatchingTopology     = errors.New("cluster db is not watching topology")
)

type newStorageDatabaseFn func(
	namespaces []namespace.Metadata,
	shardSet sharding.ShardSet,
	opts storage.Options,
) (storage.Database, error)

type clusterDB struct {
	storage.Database

	log    xlog.Logger
	hostID string
	topo   topology.Topology
	watch  topology.MapWatch

	watchMutex sync.Mutex
	watching   bool
	doneCh     chan struct{}
	closedCh   chan struct{}

	initializing   map[uint32]shard.Shard
	bootstrapCount map[uint32]int
}

// NewDatabase creates a new clustered time series database
func NewDatabase(
	namespaces []namespace.Metadata,
	hostID string,
	topoInit topology.Initializer,
	opts storage.Options,
) (Database, error) {
	log := opts.InstrumentOptions().Logger()
	topo, err := topoInit.Init()
	if err != nil {
		return nil, err
	}

	watch, err := topo.Watch()
	if err != nil {
		return nil, err
	}

	// Wait for the topology to be available
	<-watch.C()

	d := &clusterDB{
		log:            log,
		hostID:         hostID,
		topo:           topo,
		watch:          watch,
		initializing:   make(map[uint32]shard.Shard),
		bootstrapCount: make(map[uint32]int),
	}

	shardSet := d.hostOrEmptyShardSet(watch.Get())
	db, err := newStorageDatabase(namespaces, shardSet, opts)
	if err != nil {
		return nil, err
	}

	d.Database = db
	return d, nil
}

func (d *clusterDB) Open() error {
	select {
	case <-d.watch.C():
		shardSet := d.hostOrEmptyShardSet(d.watch.Get())
		d.Database.AssignShardSet(shardSet)
	default:
		// No updates to the topology since cluster DB created
	}
	if err := d.Database.Open(); err != nil {
		return err
	}
	return d.startActiveTopologyWatch()
}

func (d *clusterDB) Close() error {
	if err := d.Database.Close(); err != nil {
		return err
	}
	return d.stopActiveTopologyWatch()
}

func (d *clusterDB) startActiveTopologyWatch() error {
	d.watchMutex.Lock()
	defer d.watchMutex.Unlock()

	if d.watching {
		return errAlreadyWatchingTopology
	}

	d.watching = true

	d.doneCh = make(chan struct{}, 1)
	d.closedCh = make(chan struct{}, 1)

	go d.activeTopologyWatch()

	return nil
}

func (d *clusterDB) stopActiveTopologyWatch() error {
	d.watchMutex.Lock()
	defer d.watchMutex.Unlock()

	if !d.watching {
		return errNotWatchingTopology
	}

	d.watching = false

	close(d.doneCh)
	<-d.closedCh

	return nil
}

func (d *clusterDB) activeTopologyWatch() {
	reportClosingCh := make(chan struct{}, 1)
	reportClosedCh := make(chan struct{}, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		for {
			select {
			case <-ticker.C:
				d.analyzeAndReportShardStates()
			case <-reportClosingCh:
				ticker.Stop()
				close(reportClosedCh)
				return
			}
		}
	}()

	for {
		select {
		case <-d.doneCh:
			// Issue closing signal to report channel
			close(reportClosingCh)
			// Wait for report channel to close
			<-reportClosedCh
			// Signal all closed
			close(d.closedCh)
			return
		case <-d.watch.C():
			shardSet := d.hostOrEmptyShardSet(d.watch.Get())
			d.Database.AssignShardSet(shardSet)
		}
	}
}

func (d *clusterDB) analyzeAndReportShardStates() {
	entry, ok := d.watch.Get().LookupHostShardSet(d.hostID)
	if !ok {
		return
	}

	// Manage the reuseable vars
	d.resetReuseable()
	defer d.resetReuseable()

	for _, s := range entry.ShardSet().All() {
		if s.State() == shard.Initializing {
			d.initializing[s.ID()] = s
		}
	}

	if len(d.initializing) == 0 {
		// No initializing shards
		return
	}

	// To mark any initializing shards as available we need a
	// dynamic topology, check if we have one and if not we will report
	// that shards are initialzing and that we do not have a dynamic
	// topology to mark them as available
	topo, ok := d.topo.(topology.DynamicTopology)
	if !ok {
		err := fmt.Errorf("topology constructed is not a dynamic topology")
		d.log.Errorf("cluster db cannot mark shard available: %v", err)
		return
	}

	// Count if initializing shards have bootstrapped in all namespaces
	namespaces := d.Database.Namespaces()
	for _, n := range namespaces {
		for _, s := range n.Shards() {
			if _, ok := d.initializing[s.ID()]; !ok {
				continue
			}
			if !s.IsBootstrapped() {
				continue
			}
			d.bootstrapCount[s.ID()]++
		}
	}

	for id := range d.initializing {
		count := d.bootstrapCount[id]
		if count != len(namespaces) {
			continue
		}

		// Mark this shard as available
		if err := topo.MarkShardAvailable(d.hostID, id); err != nil {
			d.log.Errorf("cluster db failed marking shard %d available: %v",
				id, err)
		} else {
			d.log.Infof("successfully marked shard %d available", id)
		}
	}
}

func (d *clusterDB) resetReuseable() {
	d.resetInitializing()
	d.resetBootstrapCount()
}

func (d *clusterDB) resetInitializing() {
	for id := range d.initializing {
		delete(d.initializing, id)
	}
}

func (d *clusterDB) resetBootstrapCount() {
	for id := range d.bootstrapCount {
		delete(d.bootstrapCount, id)
	}
}

// hostOrEmptyShardSet returns a shard set for the given host ID from a
// topology map and if none exists then an empty shard set. If successfully
// found the shard set for the host the second parameter returns true,
// otherwise false.
func (d *clusterDB) hostOrEmptyShardSet(m topology.Map) sharding.ShardSet {
	if hostShardSet, ok := m.LookupHostShardSet(d.hostID); ok {
		return hostShardSet.ShardSet()
	}
	d.log.Warnf("topology has no shard set for host ID: %s", d.hostID)
	return sharding.NewEmptyShardSet(m.ShardSet().HashFn())
}
