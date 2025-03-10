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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	kubeerrs "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/vmware-tanzu/velero/internal/delete"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/discovery"
	"github.com/vmware-tanzu/velero/pkg/label"
	"github.com/vmware-tanzu/velero/pkg/metrics"
	"github.com/vmware-tanzu/velero/pkg/persistence"
	"github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"github.com/vmware-tanzu/velero/pkg/repository"
	"github.com/vmware-tanzu/velero/pkg/util/filesystem"
	"github.com/vmware-tanzu/velero/pkg/util/kube"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vmware-tanzu/velero/pkg/podvolume"
)

const (
	resticTimeout             = time.Minute
	deleteBackupRequestMaxAge = 24 * time.Hour
)

type backupDeletionReconciler struct {
	client.Client
	logger            logrus.FieldLogger
	backupTracker     BackupTracker
	repoMgr           repository.Manager
	metrics           *metrics.ServerMetrics
	clock             clock.Clock
	discoveryHelper   discovery.Helper
	newPluginManager  func(logrus.FieldLogger) clientmgmt.Manager
	backupStoreGetter persistence.ObjectBackupStoreGetter
}

// NewBackupDeletionReconciler creates a new backup deletion reconciler.
func NewBackupDeletionReconciler(
	logger logrus.FieldLogger,
	client client.Client,
	backupTracker BackupTracker,
	repoMgr repository.Manager,
	metrics *metrics.ServerMetrics,
	helper discovery.Helper,
	newPluginManager func(logrus.FieldLogger) clientmgmt.Manager,
	backupStoreGetter persistence.ObjectBackupStoreGetter,
) *backupDeletionReconciler {
	return &backupDeletionReconciler{
		Client:            client,
		logger:            logger,
		backupTracker:     backupTracker,
		repoMgr:           repoMgr,
		metrics:           metrics,
		clock:             clock.RealClock{},
		discoveryHelper:   helper,
		newPluginManager:  newPluginManager,
		backupStoreGetter: backupStoreGetter,
	}
}

func (r *backupDeletionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Make sure the expired requests can be deleted eventually
	s := kube.NewPeriodicalEnqueueSource(r.logger, mgr.GetClient(), &velerov1api.DeleteBackupRequestList{}, time.Hour, kube.PeriodicalEnqueueSourceOption{})
	return ctrl.NewControllerManagedBy(mgr).
		For(&velerov1api.DeleteBackupRequest{}).
		Watches(s, nil).
		Complete(r)
}

// +kubebuilder:rbac:groups=velero.io,resources=deletebackuprequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=velero.io,resources=deletebackuprequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=velero.io,resources=backups,verbs=delete

func (r *backupDeletionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.logger.WithFields(logrus.Fields{
		"controller":          BackupDeletion,
		"deletebackuprequest": req.String(),
	})
	log.Debug("Getting deletebackuprequest")
	dbr := &velerov1api.DeleteBackupRequest{}
	if err := r.Get(ctx, req.NamespacedName, dbr); err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("Unable to find the deletebackuprequest")
			return ctrl.Result{}, nil
		}
		log.WithError(err).Error("Error getting deletebackuprequest")
		return ctrl.Result{}, err
	}

	// Since we use the reconciler along with the PeriodicalEnqueueSource, there may be reconciliation triggered by
	// stale requests.
	if dbr.Status.Phase == velerov1api.DeleteBackupRequestPhaseProcessed {
		age := r.clock.Now().Sub(dbr.CreationTimestamp.Time)
		if age >= deleteBackupRequestMaxAge { // delete the expired request
			log.Debug("The request is expired, deleting it.")
			if err := r.Delete(ctx, dbr); err != nil {
				log.WithError(err).Error("Error deleting DeleteBackupRequest")
			}
		} else {
			log.Info("The request has been processed, skip.")
		}
		return ctrl.Result{}, nil
	}

	// Make sure we have the backup name
	if dbr.Spec.BackupName == "" {
		_, err := r.patchDeleteBackupRequest(ctx, dbr, func(res *velerov1api.DeleteBackupRequest) {
			res.Status.Phase = velerov1api.DeleteBackupRequestPhaseProcessed
			res.Status.Errors = []string{"spec.backupName is required"}
		})
		return ctrl.Result{}, err
	}

	log = log.WithField("backup", dbr.Spec.BackupName)

	// Remove any existing deletion requests for this backup so we only have
	// one at a time
	if errs := r.deleteExistingDeletionRequests(ctx, dbr, log); errs != nil {
		return ctrl.Result{}, kubeerrs.NewAggregate(errs)
	}

	// Don't allow deleting an in-progress backup
	if r.backupTracker.Contains(dbr.Namespace, dbr.Spec.BackupName) {
		_, err := r.patchDeleteBackupRequest(ctx, dbr, func(r *velerov1api.DeleteBackupRequest) {
			r.Status.Phase = velerov1api.DeleteBackupRequestPhaseProcessed
			r.Status.Errors = []string{"backup is still in progress"}
		})
		return ctrl.Result{}, err
	}

	// Get the backup we're trying to delete
	backup := &velerov1api.Backup{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: dbr.Namespace,
		Name:      dbr.Spec.BackupName,
	}, backup); apierrors.IsNotFound(err) {
		// Couldn't find backup - update status to Processed and record the not-found error
		_, err = r.patchDeleteBackupRequest(ctx, dbr, func(r *velerov1api.DeleteBackupRequest) {
			r.Status.Phase = velerov1api.DeleteBackupRequestPhaseProcessed
			r.Status.Errors = []string{"backup not found"}
		})
		return ctrl.Result{}, err
	} else if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error getting backup")
	}

	// Don't allow deleting backups in read-only storage locations
	location := &velerov1api.BackupStorageLocation{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: backup.Namespace,
		Name:      backup.Spec.StorageLocation,
	}, location); err != nil {
		if apierrors.IsNotFound(err) {
			_, err := r.patchDeleteBackupRequest(ctx, dbr, func(r *velerov1api.DeleteBackupRequest) {
				r.Status.Phase = velerov1api.DeleteBackupRequestPhaseProcessed
				r.Status.Errors = append(r.Status.Errors, fmt.Sprintf("backup storage location %s not found", backup.Spec.StorageLocation))
			})
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, errors.Wrap(err, "error getting backup storage location")
	}

	if location.Spec.AccessMode == velerov1api.BackupStorageLocationAccessModeReadOnly {
		_, err := r.patchDeleteBackupRequest(ctx, dbr, func(r *velerov1api.DeleteBackupRequest) {
			r.Status.Phase = velerov1api.DeleteBackupRequestPhaseProcessed
			r.Status.Errors = append(r.Status.Errors, fmt.Sprintf("cannot delete backup because backup storage location %s is currently in read-only mode", location.Name))
		})
		return ctrl.Result{}, err
	}

	// if the request object has no labels defined, initialize an empty map since
	// we will be updating labels
	if dbr.Labels == nil {
		dbr.Labels = map[string]string{}
	}
	// Update status to InProgress and set backup-name and backup-uid label if needed
	dbr, err := r.patchDeleteBackupRequest(ctx, dbr, func(r *velerov1api.DeleteBackupRequest) {
		r.Status.Phase = velerov1api.DeleteBackupRequestPhaseInProgress

		if r.Labels[velerov1api.BackupNameLabel] == "" {
			r.Labels[velerov1api.BackupNameLabel] = label.GetValidName(dbr.Spec.BackupName)
		}

		if r.Labels[velerov1api.BackupUIDLabel] == "" {
			r.Labels[velerov1api.BackupUIDLabel] = string(backup.UID)
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Set backup status to Deleting
	backup, err = r.patchBackup(ctx, backup, func(b *velerov1api.Backup) {
		b.Status.Phase = velerov1api.BackupPhaseDeleting
	})
	if err != nil {
		log.WithError(errors.WithStack(err)).Error("Error setting backup phase to deleting")
		return ctrl.Result{}, err
	}

	backupScheduleName := backup.GetLabels()[velerov1api.ScheduleNameLabel]
	r.metrics.RegisterBackupDeletionAttempt(backupScheduleName)

	pluginManager := r.newPluginManager(log)
	defer pluginManager.CleanupClients()

	backupStore, err := r.backupStoreGetter.Get(location, pluginManager, log)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error getting the backup store")
	}

	actions, err := pluginManager.GetDeleteItemActions()
	log.Debugf("%d actions before invoking actions", len(actions))
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "error getting delete item actions")
	}
	// don't defer CleanupClients here, since it was already called above.

	if len(actions) > 0 {
		// Download the tarball
		backupFile, err := downloadToTempFile(backup.Name, backupStore, log)

		if err != nil {
			log.WithError(err).Errorf("Unable to download tarball for backup %s, skipping associated DeleteItemAction plugins", backup.Name)
		} else {
			defer closeAndRemoveFile(backupFile, r.logger)
			ctx := &delete.Context{
				Backup:          backup,
				BackupReader:    backupFile,
				Actions:         actions,
				Log:             r.logger,
				DiscoveryHelper: r.discoveryHelper,
				Filesystem:      filesystem.NewFileSystem(),
			}

			// Optimization: wrap in a gofunc? Would be useful for large backups with lots of objects.
			// but what do we do with the error returned? We can't just swallow it as that may lead to dangling resources.
			err = delete.InvokeDeleteActions(ctx)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "error invoking delete item actions")
			}
		}
	}

	var errs []string

	if backupStore != nil {
		log.Info("Removing PV snapshots")

		if snapshots, err := backupStore.GetBackupVolumeSnapshots(backup.Name); err != nil {
			errs = append(errs, errors.Wrap(err, "error getting backup's volume snapshots").Error())
		} else {
			volumeSnapshotters := make(map[string]velero.VolumeSnapshotter)

			for _, snapshot := range snapshots {
				log.WithField("providerSnapshotID", snapshot.Status.ProviderSnapshotID).Info("Removing snapshot associated with backup")

				volumeSnapshotter, ok := volumeSnapshotters[snapshot.Spec.Location]
				if !ok {
					if volumeSnapshotter, err = volumeSnapshottersForVSL(ctx, backup.Namespace, snapshot.Spec.Location, r.Client, pluginManager); err != nil {
						errs = append(errs, err.Error())
						continue
					}
					volumeSnapshotters[snapshot.Spec.Location] = volumeSnapshotter
				}

				if err := volumeSnapshotter.DeleteSnapshot(snapshot.Status.ProviderSnapshotID); err != nil {
					errs = append(errs, errors.Wrapf(err, "error deleting snapshot %s", snapshot.Status.ProviderSnapshotID).Error())
				}
			}
		}
	}
	log.Info("Removing restic snapshots")
	if deleteErrs := r.deleteResticSnapshots(ctx, backup); len(deleteErrs) > 0 {
		for _, err := range deleteErrs {
			errs = append(errs, err.Error())
		}
	}

	if backupStore != nil {
		log.Info("Removing backup from backup storage")
		if err := backupStore.DeleteBackup(backup.Name); err != nil {
			errs = append(errs, err.Error())
		}
	}

	log.Info("Removing restores")
	restoreList := &velerov1api.RestoreList{}
	selector := labels.Everything()
	if err := r.List(ctx, restoreList, &client.ListOptions{
		Namespace:     backup.Namespace,
		LabelSelector: selector,
	}); err != nil {
		log.WithError(errors.WithStack(err)).Error("Error listing restore API objects")
	} else {
		for _, restore := range restoreList.Items {
			if restore.Spec.BackupName != backup.Name {
				continue
			}
			restoreLog := log.WithField("restore", kube.NamespaceAndName(&restore))

			restoreLog.Info("Deleting restore log/results from backup storage")
			if err := backupStore.DeleteRestore(restore.Name); err != nil {
				errs = append(errs, err.Error())
				// if we couldn't delete the restore files, don't delete the API object
				continue
			}

			restoreLog.Info("Deleting restore referencing backup")
			if err := r.Delete(ctx, &restore); err != nil {
				errs = append(errs, errors.Wrapf(err, "error deleting restore %s", kube.NamespaceAndName(&restore)).Error())
			}
		}
	}

	if len(errs) == 0 {
		// Only try to delete the backup object from kube if everything preceding went smoothly
		if err := r.Delete(ctx, backup); err != nil {
			errs = append(errs, errors.Wrapf(err, "error deleting backup %s", kube.NamespaceAndName(backup)).Error())
		}
	}

	if len(errs) == 0 {
		r.metrics.RegisterBackupDeletionSuccess(backupScheduleName)
	} else {
		r.metrics.RegisterBackupDeletionFailed(backupScheduleName)
	}

	// Update status to processed and record errors
	if _, err := r.patchDeleteBackupRequest(ctx, dbr, func(r *velerov1api.DeleteBackupRequest) {
		r.Status.Phase = velerov1api.DeleteBackupRequestPhaseProcessed
		r.Status.Errors = errs
	}); err != nil {
		return ctrl.Result{}, err
	}
	// Everything deleted correctly, so we can delete all DeleteBackupRequests for this backup
	if len(errs) == 0 {
		labelSelector, err := labels.Parse(fmt.Sprintf("%s=%s,%s=%s", velerov1api.BackupNameLabel, label.GetValidName(backup.Name), velerov1api.BackupUIDLabel, backup.UID))
		if err != nil {
			// Should not be here
			r.logger.WithError(err).WithField("backup", kube.NamespaceAndName(backup)).Error("error creating label selector for the backup for deleting DeleteBackupRequests")
			return ctrl.Result{}, nil
		}
		alldbr := &velerov1api.DeleteBackupRequest{}
		err = r.DeleteAllOf(ctx, alldbr, client.MatchingLabelsSelector{
			Selector: labelSelector,
		}, client.InNamespace(dbr.Namespace))
		if err != nil {
			// If this errors, all we can do is log it.
			r.logger.WithError(err).WithField("backup", kube.NamespaceAndName(backup)).Error("error deleting all associated DeleteBackupRequests after successfully deleting the backup")
		}
	}
	log.Infof("Reconciliation done")

	return ctrl.Result{}, nil
}

func volumeSnapshottersForVSL(
	ctx context.Context,
	namespace, vslName string,
	client client.Client,
	pluginManager clientmgmt.Manager,
) (velero.VolumeSnapshotter, error) {
	vsl := &velerov1api.VolumeSnapshotLocation{}
	if err := client.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      vslName,
	}, vsl); err != nil {
		return nil, errors.Wrapf(err, "error getting volume snapshot location %s", vslName)
	}
	volumeSnapshotter, err := pluginManager.GetVolumeSnapshotter(vsl.Spec.Provider)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting volume snapshotter for provider %s", vsl.Spec.Provider)
	}

	if err = volumeSnapshotter.Init(vsl.Spec.Config); err != nil {
		return nil, errors.Wrapf(err, "error initializing volume snapshotter for volume snapshot location %s", vslName)
	}

	return volumeSnapshotter, nil
}

func (r *backupDeletionReconciler) deleteExistingDeletionRequests(ctx context.Context, req *velerov1api.DeleteBackupRequest, log logrus.FieldLogger) []error {
	log.Info("Removing existing deletion requests for backup")
	dbrList := &velerov1api.DeleteBackupRequestList{}
	selector := label.NewSelectorForBackup(req.Spec.BackupName)
	if err := r.List(ctx, dbrList, &client.ListOptions{
		Namespace:     req.Namespace,
		LabelSelector: selector,
	}); err != nil {
		return []error{errors.Wrap(err, "error listing existing DeleteBackupRequests for backup")}
	}
	var errs []error
	for _, dbr := range dbrList.Items {
		if dbr.Name == req.Name {
			continue
		}
		if err := r.Delete(ctx, &dbr); err != nil {
			errs = append(errs, errors.WithStack(err))
		} else {
			log.Infof("deletion request '%s' removed.", dbr.Name)
		}
	}
	return errs
}

func (r *backupDeletionReconciler) deleteResticSnapshots(ctx context.Context, backup *velerov1api.Backup) []error {
	if r.repoMgr == nil {
		return nil
	}

	snapshots, err := getSnapshotsInBackup(ctx, backup, r.Client)
	if err != nil {
		return []error{err}
	}

	ctx2, cancelFunc := context.WithTimeout(ctx, resticTimeout)
	defer cancelFunc()

	var errs []error
	for _, snapshot := range snapshots {
		if err := r.repoMgr.Forget(ctx2, snapshot); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (r *backupDeletionReconciler) patchDeleteBackupRequest(ctx context.Context, req *velerov1api.DeleteBackupRequest, mutate func(*velerov1api.DeleteBackupRequest)) (*velerov1api.DeleteBackupRequest, error) {
	original := req.DeepCopy()
	mutate(req)
	if err := r.Patch(ctx, req, client.MergeFrom(original)); err != nil {
		return nil, errors.Wrap(err, "error patching the deletebackuprquest")
	}
	return req, nil
}

func (r *backupDeletionReconciler) patchBackup(ctx context.Context, backup *velerov1api.Backup, mutate func(*velerov1api.Backup)) (*velerov1api.Backup, error) {
	//TODO: The patchHelper can't be used here because the `backup/xxx/status` does not exist, until the bakcup resource is refactored

	// Record original json
	oldData, err := json.Marshal(backup)
	if err != nil {
		return nil, errors.Wrap(err, "error marshalling original Backup")
	}

	newBackup := backup.DeepCopy()
	mutate(newBackup)
	newData, err := json.Marshal(newBackup)
	if err != nil {
		return nil, errors.Wrap(err, "error marshalling updated Backup")
	}
	patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
	if err != nil {
		return nil, errors.Wrap(err, "error creating json merge patch for Backup")
	}

	if err := r.Client.Patch(ctx, backup, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
		return nil, errors.Wrap(err, "error patching Backup")
	}
	return backup, nil
}

// getSnapshotsInBackup returns a list of all restic snapshot ids associated with
// a given Velero backup.
func getSnapshotsInBackup(ctx context.Context, backup *velerov1api.Backup, kbClient client.Client) ([]repository.SnapshotIdentifier, error) {
	podVolumeBackups := &velerov1api.PodVolumeBackupList{}
	options := &client.ListOptions{
		LabelSelector: labels.Set(map[string]string{
			velerov1api.BackupNameLabel: label.GetValidName(backup.Name),
		}).AsSelector(),
	}

	err := kbClient.List(ctx, podVolumeBackups, options)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return podvolume.GetSnapshotIdentifier(podVolumeBackups), nil
}
