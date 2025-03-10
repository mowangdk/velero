/*
Copyright 2020 the Velero contributors.

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

package clientmgmt

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	biav1cli "github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt/backupitemaction/v1"
	"github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt/process"
	"github.com/vmware-tanzu/velero/pkg/plugin/framework/common"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	biav1 "github.com/vmware-tanzu/velero/pkg/plugin/velero/backupitemaction/v1"
	isv1 "github.com/vmware-tanzu/velero/pkg/plugin/velero/item_snapshotter/v1"
)

// Manager manages the lifecycles of plugins.
type Manager interface {
	// GetObjectStore returns the ObjectStore plugin for name.
	GetObjectStore(name string) (velero.ObjectStore, error)

	// GetVolumeSnapshotter returns the VolumeSnapshotter plugin for name.
	GetVolumeSnapshotter(name string) (velero.VolumeSnapshotter, error)

	// GetBackupItemActions returns all v1 backup item action plugins.
	GetBackupItemActions() ([]biav1.BackupItemAction, error)

	// GetBackupItemAction returns the backup item action plugin for name.
	GetBackupItemAction(name string) (biav1.BackupItemAction, error)

	// GetRestoreItemActions returns all restore item action plugins.
	GetRestoreItemActions() ([]velero.RestoreItemAction, error)

	// GetRestoreItemAction returns the restore item action plugin for name.
	GetRestoreItemAction(name string) (velero.RestoreItemAction, error)

	// GetDeleteItemActions returns all delete item action plugins.
	GetDeleteItemActions() ([]velero.DeleteItemAction, error)

	// GetDeleteItemAction returns the delete item action plugin for name.
	GetDeleteItemAction(name string) (velero.DeleteItemAction, error)

	// GetItemSnapshotter returns the item snapshotter plugin for name
	GetItemSnapshotter(name string) (isv1.ItemSnapshotter, error)

	// GetItemSnapshotters returns all item snapshotter plugins
	GetItemSnapshotters() ([]isv1.ItemSnapshotter, error)

	// CleanupClients terminates all of the Manager's running plugin processes.
	CleanupClients()
}

// Used checking for adapted plugin versions
var pluginNotFoundErrType = &process.PluginNotFoundError{}

// manager implements Manager.
type manager struct {
	logger   logrus.FieldLogger
	logLevel logrus.Level
	registry process.Registry

	restartableProcessFactory process.RestartableProcessFactory

	// lock guards restartableProcesses
	lock                 sync.Mutex
	restartableProcesses map[string]process.RestartableProcess
}

// NewManager constructs a manager for getting plugins.
func NewManager(logger logrus.FieldLogger, level logrus.Level, registry process.Registry) Manager {
	return &manager{
		logger:   logger,
		logLevel: level,
		registry: registry,

		restartableProcessFactory: process.NewRestartableProcessFactory(),

		restartableProcesses: make(map[string]process.RestartableProcess),
	}
}

func (m *manager) CleanupClients() {
	m.lock.Lock()

	for _, restartableProcess := range m.restartableProcesses {
		restartableProcess.Stop()
	}

	m.lock.Unlock()
}

// getRestartableProcess returns a restartableProcess for a plugin identified by kind and name, creating a
// restartableProcess if it is the first time it has been requested.
func (m *manager) getRestartableProcess(kind common.PluginKind, name string) (process.RestartableProcess, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	logger := m.logger.WithFields(logrus.Fields{
		"kind": kind.String(),
		"name": name,
	})
	logger.Debug("looking for plugin in registry")

	info, err := m.registry.Get(kind, name)
	if err != nil {
		return nil, err
	}

	logger = logger.WithField("command", info.Command)

	restartableProcess, found := m.restartableProcesses[info.Command]
	if found {
		logger.Debug("found preexisting restartable plugin process")
		return restartableProcess, nil
	}

	logger.Debug("creating new restartable plugin process")

	restartableProcess, err = m.restartableProcessFactory.NewRestartableProcess(info.Command, m.logger, m.logLevel)
	if err != nil {
		return nil, err
	}

	m.restartableProcesses[info.Command] = restartableProcess

	return restartableProcess, nil
}

// GetObjectStore returns a restartableObjectStore for name.
func (m *manager) GetObjectStore(name string) (velero.ObjectStore, error) {
	name = sanitizeName(name)

	restartableProcess, err := m.getRestartableProcess(common.PluginKindObjectStore, name)
	if err != nil {
		return nil, err
	}

	r := NewRestartableObjectStore(name, restartableProcess)

	return r, nil
}

// GetVolumeSnapshotter returns a restartableVolumeSnapshotter for name.
func (m *manager) GetVolumeSnapshotter(name string) (velero.VolumeSnapshotter, error) {
	name = sanitizeName(name)

	restartableProcess, err := m.getRestartableProcess(common.PluginKindVolumeSnapshotter, name)
	if err != nil {
		return nil, err
	}

	r := NewRestartableVolumeSnapshotter(name, restartableProcess)

	return r, nil
}

// GetBackupItemActions returns all backup item actions as restartableBackupItemActions.
func (m *manager) GetBackupItemActions() ([]biav1.BackupItemAction, error) {
	list := m.registry.List(common.PluginKindBackupItemAction)

	actions := make([]biav1.BackupItemAction, 0, len(list))

	for i := range list {
		id := list[i]

		r, err := m.GetBackupItemAction(id.Name)
		if err != nil {
			return nil, err
		}

		actions = append(actions, r)
	}

	return actions, nil
}

// GetBackupItemAction returns a restartableBackupItemAction for name.
func (m *manager) GetBackupItemAction(name string) (biav1.BackupItemAction, error) {
	name = sanitizeName(name)

	for _, adaptedBackupItemAction := range biav1cli.AdaptedBackupItemActions() {
		restartableProcess, err := m.getRestartableProcess(adaptedBackupItemAction.Kind, name)
		// Check if plugin was not found
		if errors.As(err, &pluginNotFoundErrType) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return adaptedBackupItemAction.GetRestartable(name, restartableProcess), nil
	}
	return nil, fmt.Errorf("unable to get valid BackupItemAction for %q", name)
}

// GetRestoreItemActions returns all restore item actions as restartableRestoreItemActions.
func (m *manager) GetRestoreItemActions() ([]velero.RestoreItemAction, error) {
	list := m.registry.List(common.PluginKindRestoreItemAction)

	actions := make([]velero.RestoreItemAction, 0, len(list))

	for i := range list {
		id := list[i]

		r, err := m.GetRestoreItemAction(id.Name)
		if err != nil {
			return nil, err
		}

		actions = append(actions, r)
	}

	return actions, nil
}

// GetRestoreItemAction returns a restartableRestoreItemAction for name.
func (m *manager) GetRestoreItemAction(name string) (velero.RestoreItemAction, error) {
	name = sanitizeName(name)

	restartableProcess, err := m.getRestartableProcess(common.PluginKindRestoreItemAction, name)
	if err != nil {
		return nil, err
	}

	r := NewRestartableRestoreItemAction(name, restartableProcess)
	return r, nil
}

// GetDeleteItemActions returns all delete item actions as restartableDeleteItemActions.
func (m *manager) GetDeleteItemActions() ([]velero.DeleteItemAction, error) {
	list := m.registry.List(common.PluginKindDeleteItemAction)

	actions := make([]velero.DeleteItemAction, 0, len(list))

	for i := range list {
		id := list[i]

		r, err := m.GetDeleteItemAction(id.Name)
		if err != nil {
			return nil, err
		}

		actions = append(actions, r)
	}

	return actions, nil
}

// GetDeleteItemAction returns a restartableDeleteItemAction for name.
func (m *manager) GetDeleteItemAction(name string) (velero.DeleteItemAction, error) {
	name = sanitizeName(name)

	restartableProcess, err := m.getRestartableProcess(common.PluginKindDeleteItemAction, name)
	if err != nil {
		return nil, err
	}

	r := NewRestartableDeleteItemAction(name, restartableProcess)
	return r, nil
}

func (m *manager) GetItemSnapshotter(name string) (isv1.ItemSnapshotter, error) {
	name = sanitizeName(name)

	restartableProcess, err := m.getRestartableProcess(common.PluginKindItemSnapshotter, name)
	if err != nil {
		return nil, err
	}

	r := NewRestartableItemSnapshotter(name, restartableProcess)
	return r, nil
}

func (m *manager) GetItemSnapshotters() ([]isv1.ItemSnapshotter, error) {
	list := m.registry.List(common.PluginKindItemSnapshotter)

	actions := make([]isv1.ItemSnapshotter, 0, len(list))

	for i := range list {
		id := list[i]

		r, err := m.GetItemSnapshotter(id.Name)
		if err != nil {
			return nil, err
		}

		actions = append(actions, r)
	}

	return actions, nil
}

// sanitizeName adds "velero.io" to legacy plugins that weren't namespaced.
func sanitizeName(name string) string {
	// Backwards compatibility with non-namespaced Velero plugins, following principle of least surprise
	// since DeleteItemActions were not bundled with Velero when plugins were non-namespaced.
	if !strings.Contains(name, "/") {
		name = "velero.io/" + name
	}

	return name
}
