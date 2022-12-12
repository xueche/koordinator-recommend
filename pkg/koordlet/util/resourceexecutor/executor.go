/*
Copyright 2022 The Koordinator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resourceexecutor

// TODO: move to pkg/koordlet/resourceexecutor.

import (
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/koordinator-sh/koordinator/pkg/tools/cache"
)

var _ ResourceUpdateExecutor = &ResourceUpdateExecutorImpl{}

type ResourceUpdateExecutor interface {
	Update(cacheable bool, updater ResourceUpdater) (updated bool, err error)
	UpdateBatch(cacheable bool, updaters ...ResourceUpdater)
	// LeveledUpdateBatch is to cacheable update resources by the order of resources' level.
	// For cgroup interfaces like `cpuset.cpus` and `memory.min`, reconciliation from top to bottom should keep the
	// upper value larger/broader than the lower. Thus a Leveled updater is implemented as follows:
	// 1. update batch of cgroup resources group by cgroup interface, i.e. cgroup filename.
	// 2. update each cgroup resource by the order of layers: firstly update resources from upper to lower by merging
	//    the new value with old value; then update resources from lower to upper with the new value.
	LeveledUpdateBatch(cacheable bool, updaters [][]ResourceUpdater)
	Run(stopCh <-chan struct{})
}

type ResourceUpdateExecutorImpl struct {
	leveledUpdateLock  sync.Mutex
	resourceCache      *cache.Cache
	forceUpdateSeconds int
}

var (
	onceRun sync.Once

	singleton = &ResourceUpdateExecutorImpl{
		resourceCache: cache.NewCacheDefault(),
	}
)

func NewResourceUpdateExecutor() ResourceUpdateExecutor {
	return singleton
}

// Update updates the resources with the given cacheable attribute with the cacheable attribute directly.
func (e *ResourceUpdateExecutorImpl) Update(cacheable bool, resource ResourceUpdater) (bool, error) {
	if cacheable {
		return e.updateByCache(resource)
	}
	return true, e.update(resource)
}

// UpdateBatch updates a batch of resources with the given cacheable attribute.
// TODO: merge and resolve conflicts of batch updates from multiple callers.
func (e *ResourceUpdateExecutorImpl) UpdateBatch(cacheable bool, updaters ...ResourceUpdater) {
	failures := 0
	if cacheable {
		for _, updater := range updaters {
			isUpdated, err := e.updateByCache(updater)
			if err != nil {
				failures++
				klog.V(4).Infof("failed to cacheable update resource %s to %v, isUpdated %v, err: %v",
					updater.Path(), updater.Value(), isUpdated, err)
			}
			klog.V(5).Infof("successfully cacheable update resource %s to %v, isUpdated %v",
				updater.Path(), updater.Value(), isUpdated)
		}
	} else {
		for _, updater := range updaters {
			err := e.update(updater)
			if err != nil {
				failures++
				klog.V(4).Infof("failed to update resource %s to %v, err: %v", updater.Path(), updater.Value(), err)
			}
			klog.V(5).Infof("successfully update resource %s to %v", updater.Path(), updater.Value())
		}
	}
	klog.V(6).Infof("finished batch updating resources, isCacheable %v, total %v, failures %v",
		cacheable, len(updaters), failures)
}

func (e *ResourceUpdateExecutorImpl) LeveledUpdateBatch(cacheable bool, updaters [][]ResourceUpdater) {
	e.leveledUpdateLock.Lock()
	defer e.leveledUpdateLock.Unlock()
	var err error
	skipMerge := map[string]bool{}
	for i := 0; i < len(updaters); i++ {
		for _, updater := range updaters[i] {
			if !e.needUpdate(updater) {
				continue
			}

			mergedUpdater, err := updater.MergeUpdate()
			if err != nil {
				klog.V(4).Infof("failed merge update resource %s, err: %v", updater.Path(), err)
				continue
			}
			klog.V(6).Infof("successfully merge update resource %s to %v", updater.Path(), updater.Value())

			if mergedUpdater == nil {
				skipMerge[updater.Path()] = true
			} else {
				updater = mergedUpdater
			}

			updater.UpdateLastUpdateTimestamp(time.Now())
			err = e.resourceCache.SetDefault(updater.Path(), updater)
			if err != nil {
				klog.V(4).Infof("failed to SetDefault in resourceCache for resource %s, err: %v",
					updater.Path(), err)
			}
		}
	}

	for i := len(updaters) - 1; i >= 0; i-- {
		for _, updater := range updaters[i] {
			if !e.needUpdate(updater) {
				continue
			}

			// skip update twice for resources specified no merge
			if skipMerge[updater.Path()] {
				klog.V(6).Infof("skip update resource %s since it should skip the merge", updater.Path())
				continue
			}
			err = updater.Update()
			if err != nil {
				klog.V(4).Infof("failed update resource %s, err: %v", updater.Path(), err)
				continue
			}
			klog.V(6).Infof("successfully update resource %s to %v", updater.Path(), updater.Value())

			updater.UpdateLastUpdateTimestamp(time.Now())
			err = e.resourceCache.SetDefault(updater.Path(), updater)
			if err != nil {
				klog.V(4).Infof("failed to SetDefault in resourceCache for resource %s, err: %v",
					updater.Path(), err)
			}
		}
	}
}

// Run runs the ResourceUpdateExecutor.
// TODO: run single executor when the qos manager starts.
func (e *ResourceUpdateExecutorImpl) Run(stopCh <-chan struct{}) {
	onceRun.Do(func() {
		_ = e.resourceCache.Run(stopCh)
		klog.V(4).Info("starting ResourceUpdateExecutor successfully")
	})
}

func (e *ResourceUpdateExecutorImpl) needUpdate(updater ResourceUpdater) bool {
	preResource, _ := e.resourceCache.Get(updater.Path())
	if preResource == nil {
		klog.V(5).Infof("check for resource %s: pre is nil, need update", updater.Path())
		return true
	}
	preResourceUpdater := preResource.(ResourceUpdater)
	if updater.Value() != preResourceUpdater.Value() {
		klog.V(5).Infof("check for resource %s: current %v, pre %v, need update",
			updater.Path(), updater.Value(), preResourceUpdater.Value())
		return true
	}
	if time.Since(preResourceUpdater.GetLastUpdateTimestamp()) > time.Duration(e.forceUpdateSeconds)*time.Second {
		klog.V(5).Infof("check for resource %s: last update time(%v) is earlier than (%v)s ago, need update",
			preResourceUpdater.Path(), preResourceUpdater.GetLastUpdateTimestamp(), e.forceUpdateSeconds)
		return true
	}
	return false
}

func (e *ResourceUpdateExecutorImpl) update(updater ResourceUpdater) error {
	err := updater.Update()
	if err != nil {
		klog.V(4).Infof("failed to update resource %s to %v, err: %v", updater.Path(), updater.Value(), err)
		return err
	}
	klog.V(6).Infof("successfully update resource %s to %v", updater.Path(), updater.Value())
	return nil
}

func (e *ResourceUpdateExecutorImpl) updateByCache(updater ResourceUpdater) (bool, error) {
	if e.needUpdate(updater) {
		err := updater.Update()
		if err != nil {
			klog.V(5).Infof("failed to cacheable update resource %s to %v, err: %v", updater.Path(), updater.Value(), err)
			return false, err
		}
		updater.UpdateLastUpdateTimestamp(time.Now())
		err = e.resourceCache.SetDefault(updater.Path(), updater)
		if err != nil {
			klog.V(5).Infof("failed to SetDefault in resourceCache for resource %s, err: %v", updater.Path(), err)
			return true, err
		}
		klog.V(6).Infof("successfully cacheable update resource %s to %v", updater.Path(), updater.Value())
		return true, nil
	}
	return false, nil
}