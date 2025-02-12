// Copyright 2018 Envoyproxy Authors
//
//   Licensed under the Apache License, Version 2.0 (the "License");
//   you may not use this file except in compliance with the License.
//   You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package xds

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	envoy_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_cache "github.com/envoyproxy/go-control-plane/pkg/cache"
	envoy_log "github.com/envoyproxy/go-control-plane/pkg/log"
)

// This is a slightly modified version of SnapshotCache from github.com/envoyproxy/go-control-plane
// library.
// The only difference is that Snapshot type has been turned into an interface
// so that SnapshotCache could be used for arbitrary xDS resources rather than just core Envoy's ones.

// Snapshot is an internally consistent snapshot of xDS resources.
// Consistency is important for the convergence as different resource types
// from the snapshot may be delivered to the proxy in arbitrary order.
//
// Notice that unlike in the original github.com/envoyproxy/go-control-plane library,
// Snapshot is an interface which will allow to reuse SnapshotCache for arbitrary
// xDS resources rather than just core Envoy's ones.
type Snapshot interface {

	// GetSupportedTypes returns a list of xDS types supported by this snapshot.
	GetSupportedTypes() []string

	// Consistent check verifies that the dependent resources are exactly listed in the
	// snapshot:
	// - all EDS resources are listed by name in CDS resources
	// - all RDS resources are listed by name in LDS resources
	//
	// Note that clusters and listeners are requested without name references, so
	// Envoy will accept the snapshot list of clusters as-is even if it does not match
	// all references found in xDS.
	Consistent() error

	// GetResources selects snapshot resources by type.
	GetResources(typ string) map[string]envoy_cache.Resource

	// GetVersion returns the version for a resource type.
	GetVersion(typ string) string

	// WithVersion creates a new snapshot with a different version for a given resource type.
	WithVersion(typ string, version string) Snapshot
}

// SnapshotCache is a snapshot-based cache that maintains a single versioned
// snapshot of responses per node. SnapshotCache consistently replies with the
// latest snapshot. For the protocol to work correctly in ADS mode, EDS/RDS
// requests are responded only when all resources in the snapshot xDS response
// are named as part of the request. It is expected that the CDS response names
// all EDS clusters, and the LDS response names all RDS routes in a snapshot,
// to ensure that Envoy makes the request for all EDS clusters or RDS routes
// eventually.
//
// SnapshotCache can operate as a REST or regular xDS backend. The snapshot
// can be partial, e.g. only include RDS or EDS resources.
type SnapshotCache interface {
	envoy_cache.Cache

	// SetSnapshot sets a response snapshot for a node. For ADS, the snapshots
	// should have distinct versions and be internally consistent (e.g. all
	// referenced resources must be included in the snapshot).
	//
	// This method will cause the server to respond to all open watches, for which
	// the version differs from the snapshot version.
	SetSnapshot(node string, snapshot Snapshot) error

	// GetSnapshots gets the snapshot for a node.
	GetSnapshot(node string) (Snapshot, error)

	// ClearSnapshot removes all status and snapshot information associated with a node.
	ClearSnapshot(node string)
}

type snapshotCache struct {
	log envoy_log.Logger

	// ads flag to hold responses until all resources are named
	ads bool

	// snapshots are cached resources indexed by node IDs
	snapshots map[string]Snapshot

	// status information for all nodes indexed by node IDs
	status map[string]*statusInfo

	// hash is the hashing function for Envoy nodes
	hash envoy_cache.NodeHash

	// watchCount is an atomic counter incremented for each watch
	watchCount int64

	mu sync.RWMutex
}

// NewSnapshotCache initializes a simple cache.
//
// ADS flag forces a delay in responding to streaming requests until all
// resources are explicitly named in the request. This avoids the problem of a
// partial request over a single stream for a subset of resources which would
// require generating a fresh version for acknowledgement. ADS flag requires
// snapshot consistency. For non-ADS case (and fetch), multiple partial
// requests are sent across multiple streams and re-using the snapshot version
// is OK.
//
// Logger is optional.
func NewSnapshotCache(ads bool, hash envoy_cache.NodeHash, logger envoy_log.Logger) SnapshotCache {
	return &snapshotCache{
		log:       logger,
		ads:       ads,
		snapshots: make(map[string]Snapshot),
		status:    make(map[string]*statusInfo),
		hash:      hash,
	}
}

// SetSnapshotCache updates a snapshot for a node.
func (cache *snapshotCache) SetSnapshot(node string, snapshot Snapshot) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	// update the existing entry
	cache.snapshots[node] = snapshot

	// trigger existing watches for which version changed
	if info, ok := cache.status[node]; ok {
		info.mu.Lock()
		for id, watch := range info.watches {
			version := snapshot.GetVersion(watch.Request.TypeUrl)
			if version != watch.Request.VersionInfo {
				if cache.log != nil {
					cache.log.Infof("respond open watch %d%v with new version %q", id, watch.Request.ResourceNames, version)
				}
				cache.respond(watch.Request, watch.Response, snapshot.GetResources(watch.Request.TypeUrl), version)

				// discard the watch
				delete(info.watches, id)
			}
		}
		info.mu.Unlock()
	}

	return nil
}

// GetSnapshots gets the snapshot for a node, and returns an error if not found.
func (cache *snapshotCache) GetSnapshot(node string) (Snapshot, error) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	snap, ok := cache.snapshots[node]
	if !ok {
		return nil, fmt.Errorf("no snapshot found for node %s", node)
	}
	return snap, nil
}

// ClearSnapshot clears snapshot and info for a node.
func (cache *snapshotCache) ClearSnapshot(node string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	delete(cache.snapshots, node)
	delete(cache.status, node)
}

// nameSet creates a map from a string slice to value true.
func nameSet(names []string) map[string]bool {
	set := make(map[string]bool)
	for _, name := range names {
		set[name] = true
	}
	return set
}

// superset checks that all resources are listed in the names set.
func superset(names map[string]bool, resources map[string]envoy_cache.Resource) error {
	for resourceName := range resources {
		if _, exists := names[resourceName]; !exists {
			return fmt.Errorf("%q not listed", resourceName)
		}
	}
	return nil
}

// CreateWatch returns a watch for an xDS request.
func (cache *snapshotCache) CreateWatch(request envoy_cache.Request) (chan envoy_cache.Response, func()) {
	nodeID := cache.hash.ID(request.Node)

	cache.mu.Lock()
	defer cache.mu.Unlock()

	info, ok := cache.status[nodeID]
	if !ok {
		info = newStatusInfo(request.Node)
		cache.status[nodeID] = info
	}

	// update last watch request time
	info.mu.Lock()
	info.lastWatchRequestTime = time.Now()
	info.mu.Unlock()

	// allocate capacity 1 to allow one-time non-blocking use
	value := make(chan envoy_cache.Response, 1)

	snapshot, exists := cache.snapshots[nodeID]
	version := ""
	if exists {
		version = snapshot.GetVersion(request.TypeUrl)
	}

	// if the requested version is up-to-date or missing a response, leave an open watch
	if !exists || request.VersionInfo == version {
		watchID := cache.nextWatchID()
		if cache.log != nil {
			cache.log.Infof("open watch %d for %s%v from nodeID %q, version %q", watchID,
				request.TypeUrl, request.ResourceNames, nodeID, request.VersionInfo)
		}
		info.mu.Lock()
		info.watches[watchID] = envoy_cache.ResponseWatch{Request: request, Response: value}
		info.mu.Unlock()
		return value, cache.cancelWatch(nodeID, watchID)
	}

	// otherwise, the watch may be responded immediately
	cache.respond(request, value, snapshot.GetResources(request.TypeUrl), version)

	return value, nil
}

func (cache *snapshotCache) nextWatchID() int64 {
	return atomic.AddInt64(&cache.watchCount, 1)
}

// cancellation function for cleaning stale watches
func (cache *snapshotCache) cancelWatch(nodeID string, watchID int64) func() {
	return func() {
		// uses the cache mutex
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if info, ok := cache.status[nodeID]; ok {
			info.mu.Lock()
			delete(info.watches, watchID)
			info.mu.Unlock()
		}
	}
}

// Respond to a watch with the snapshot value. The value channel should have capacity not to block.
// TODO(kuat) do not respond always, see issue https://github.com/envoyproxy/go-control-plane/issues/46
func (cache *snapshotCache) respond(request envoy_cache.Request, value chan envoy_cache.Response, resources map[string]envoy_cache.Resource, version string) {
	// for ADS, the request names must match the snapshot names
	// if they do not, then the watch is never responded, and it is expected that envoy makes another request
	if len(request.ResourceNames) != 0 && cache.ads {
		if err := superset(nameSet(request.ResourceNames), resources); err != nil {
			if cache.log != nil {
				cache.log.Infof("ADS mode: not responding to request: %v", err)
			}
			return
		}
	}
	if cache.log != nil {
		cache.log.Infof("respond %s%v version %q with version %q",
			request.TypeUrl, request.ResourceNames, request.VersionInfo, version)
	}

	value <- createResponse(request, resources, version)
}

func createResponse(request envoy_cache.Request, resources map[string]envoy_cache.Resource, version string) envoy_cache.Response {
	filtered := make([]envoy_cache.Resource, 0, len(resources))

	// Reply only with the requested resources. Envoy may ask each resource
	// individually in a separate stream. It is ok to reply with the same version
	// on separate streams since requests do not share their response versions.
	if len(request.ResourceNames) != 0 {
		set := nameSet(request.ResourceNames)
		for name, resource := range resources {
			if set[name] {
				filtered = append(filtered, resource)
			}
		}
	} else {
		for _, resource := range resources {
			filtered = append(filtered, resource)
		}
	}

	return envoy_cache.Response{
		Request:   request,
		Version:   version,
		Resources: filtered,
	}
}

// Fetch implements the cache fetch function.
// Fetch is called on multiple streams, so responding to individual names with the same version works.
func (cache *snapshotCache) Fetch(ctx context.Context, request envoy_cache.Request) (*envoy_cache.Response, error) {
	nodeID := cache.hash.ID(request.Node)

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	if snapshot, exists := cache.snapshots[nodeID]; exists {
		// Respond only if the request version is distinct from the current snapshot state.
		// It might be beneficial to hold the request since Envoy will re-attempt the refresh.
		version := snapshot.GetVersion(request.TypeUrl)
		if request.VersionInfo == version {
			return nil, &envoy_cache.SkipFetchError{}
		}

		resources := snapshot.GetResources(request.TypeUrl)
		out := createResponse(request, resources, version)
		return &out, nil
	}

	return nil, fmt.Errorf("missing snapshot for %q", nodeID)
}

// GetStatusInfo retrieves the status info for the node.
func (cache *snapshotCache) GetStatusInfo(node string) envoy_cache.StatusInfo {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	info, exists := cache.status[node]
	if !exists {
		return nil
	}

	return info
}

// GetStatusKeys retrieves all node IDs in the status map.
func (cache *snapshotCache) GetStatusKeys() []string {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	out := make([]string, 0, len(cache.status))
	for id := range cache.status {
		out = append(out, id)
	}

	return out
}

type statusInfo struct {
	// node is the constant Envoy node metadata.
	node *envoy_core.Node

	// watches are indexed channels for the response watches and the original requests.
	watches map[int64]envoy_cache.ResponseWatch

	// the timestamp of the last watch request
	lastWatchRequestTime time.Time

	// mutex to protect the status fields.
	// should not acquire mutex of the parent cache after acquiring this mutex.
	mu sync.RWMutex
}

// newStatusInfo initializes a status info data structure.
func newStatusInfo(node *envoy_core.Node) *statusInfo {
	out := statusInfo{
		node:    node,
		watches: make(map[int64]envoy_cache.ResponseWatch),
	}
	return &out
}

func (info *statusInfo) GetNode() *envoy_core.Node {
	info.mu.RLock()
	defer info.mu.RUnlock()
	return info.node
}

func (info *statusInfo) GetNumWatches() int {
	info.mu.RLock()
	defer info.mu.RUnlock()
	return len(info.watches)
}

func (info *statusInfo) GetLastWatchRequestTime() time.Time {
	info.mu.RLock()
	defer info.mu.RUnlock()
	return info.lastWatchRequestTime
}
