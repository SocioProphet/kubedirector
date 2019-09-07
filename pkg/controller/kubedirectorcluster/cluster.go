// Copyright 2018 BlueData Software, Inc.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubedirectorcluster

import (
	"reflect"
	"time"

	kdv1 "github.com/bluek8s/kubedirector/pkg/apis/kubedirector.bluedata.io/v1alpha1"
	"github.com/bluek8s/kubedirector/pkg/catalog"
	"github.com/bluek8s/kubedirector/pkg/executor"
	"github.com/bluek8s/kubedirector/pkg/observer"
	"github.com/bluek8s/kubedirector/pkg/shared"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/errors"
)

var (
	// ClusterStatusGens is exported so that the validator can have access.
	ClusterStatusGens = shared.NewStatusGens()
)

// syncCluster runs the reconciliation logic. It is invoked because of a
// change in or addition of a KubeDirectorCluster instance, or a periodic
// polling to check on such a resource.
func (r *ReconcileKubeDirectorCluster) syncCluster(
	reqLogger logr.Logger,
	cr *kdv1.KubeDirectorCluster,
) error {

	// We use a finalizer to maintain KubeDirector state consistency;
	// e.g. app references and ClusterStatusGens.
	doExit, finalizerErr := r.handleFinalizers(reqLogger, cr)
	if finalizerErr != nil {
		return finalizerErr
	}
	if doExit {
		return nil
	}

	// Make sure we have a Status object to work with.
	if cr.Status == nil {
		cr.Status = &kdv1.KubeDirectorClusterStatus{}
		cr.Status.Roles = make([]kdv1.RoleStatus, 0)
	}

	// Set up logic to update status as necessary when reconciler exits.
	oldStatus := cr.Status.DeepCopy()
	defer func() {
		if !reflect.DeepEqual(cr.Status, oldStatus) {
			// Write back the status. Don't exit this reconciler until we
			// succeed (will block other reconcilers for this resource).
			wait := time.Second
			maxWait := 4096 * time.Second
			for {
				cr.Status.GenerationUID = uuid.New().String()
				ClusterStatusGens.WriteStatusGen(cr.UID, cr.Status.GenerationUID)
				updateErr := executor.UpdateClusterStatus(cr)
				if updateErr == nil {
					return
				}
				// Update failed. If the cluster has been or is being
				// deleted, that's ok... otherwise wait and try again.
				currentCluster, currentClusterErr := observer.GetCluster(
					cr.Namespace,
					cr.Name,
				)
				if currentClusterErr != nil {
					if errors.IsNotFound(currentClusterErr) {
						return
					}
				} else {
					if currentCluster.DeletionTimestamp != nil {
						return
					}
					if errors.IsConflict(updateErr) {
						// If the update failed with a ResourceVersion
						// conflict then we need to use the current
						// version of the cluster. Otherwise, the status
						// update will never succeed and this loop will
						// never terminate.
						currentCluster.Status = cr.Status
						*cr = *currentCluster
						continue
					}
				}
				if wait < maxWait {
					wait = wait * 2
				}
				shared.LogErrorf(
					reqLogger,
					updateErr,
					cr,
					shared.EventReasonCluster,
					"trying status update again in %v; failed",
					wait,
				)
				time.Sleep(wait)
			}
		}
	}()

	// For a new CR just update the status state/gen.
	shouldProcessCR := r.handleNewCluster(reqLogger, cr)
	if !shouldProcessCR {
		return nil
	}

	errLog := func(domain string, err error) {
		shared.LogErrorf(
			reqLogger,
			err,
			cr,
			shared.EventReasonCluster,
			"failed to sync %s",
			domain,
		)
	}

	clusterServiceErr := syncClusterService(reqLogger, cr)
	if clusterServiceErr != nil {
		errLog("cluster service", clusterServiceErr)
		return clusterServiceErr
	}

	roles, state, rolesErr := syncClusterRoles(reqLogger, cr)
	if rolesErr != nil {
		errLog("roles", rolesErr)
		return rolesErr
	}

	memberServicesErr := syncMemberServices(reqLogger, cr, roles)
	if memberServicesErr != nil {
		errLog("member services", memberServicesErr)
		return memberServicesErr
	}

	if state == clusterMembersStableReady {
		if cr.Status.State != string(clusterReady) {
			shared.LogInfo(
				reqLogger,
				cr,
				shared.EventReasonCluster,
				"stable",
			)
			cr.Status.State = string(clusterReady)
		}
		return nil
	}

	if cr.Status.State != string(clusterCreating) {
		cr.Status.State = string(clusterUpdating)
	}

	configmetaGen, configMetaErr := catalog.ConfigmetaGenerator(
		cr,
		calcMembersForRoles(roles),
	)
	if configMetaErr != nil {
		shared.LogError(
			reqLogger,
			configMetaErr,
			cr,
			shared.EventReasonCluster,
			"failed to generate cluster config",
		)
		return configMetaErr
	}

	membersHaveChanged := (state == clusterMembersChangedUnready)
	membersErr := syncMembers(reqLogger, cr, roles, membersHaveChanged, configmetaGen)
	if membersErr != nil {
		errLog("members", membersErr)
		return membersErr
	}

	return nil
}

// handleNewCluster looks in the cache for the last-known status generation
// UID for this CR. If there is one, return true to keep processing the CR.
// If there is not any last-known UID, this is either a new CR or one that
// was created before this KD came up. In the former case, where the CR status
// itself has no generation UID: set the cluster state to creating (this will
// also trigger population of the generation UID) and return false to cause
// this handler to exit; we'll pick up further processing in the next handler.
// In the latter case, sync up our internal state with the visible state of
// the CR and return true to continue processing. In either-new cluster case
// invoke shared.EnsureClusterAppReference to mark that the app is being used.
func (r *ReconcileKubeDirectorCluster) handleNewCluster(
	reqLogger logr.Logger,
	cr *kdv1.KubeDirectorCluster,
) bool {

	// Have we seen this cluster before?
	_, ok := ClusterStatusGens.ReadStatusGen(cr.UID)
	if ok {
		// Yep we've already done processing for this cluster previously.
		return true
	}
	// This is a new cluster, or at least "new to us", so mark that its app
	// is in use.
	shared.EnsureClusterAppReference(
		cr.Namespace,
		cr.Name,
		*(cr.Spec.AppCatalog),
		cr.Spec.AppID,
	)
	// There are creation-race or KD-recovery cases where the app might not
	// exist, so check that now.
	_, appErr := catalog.GetApp(cr)
	if appErr != nil {
		shared.LogError(
			reqLogger,
			appErr,
			cr,
			shared.EventReasonCluster,
			"app referenced by cluster does not exist",
		)
		// We're not going to take any other steps at this point... not even
		// going to remove the app reference. Operations on this cluster
		// could fail, but it might be recoverable by re-creating the app CR.
	}
	incoming := cr.Status.GenerationUID
	if incoming == "" {
		// This is an actual newly-created cluster, so kick off the processing.
		shared.LogInfo(
			reqLogger,
			cr,
			shared.EventReasonCluster,
			"new",
		)
		cr.Status.State = string(clusterCreating)
		return false
	}
	// This cluster has been processed before but we're not aware of it yet.
	// Probably KD has been restarted. Make us aware of this cluster.
	shared.LogInfof(
		reqLogger,
		cr,
		shared.EventReasonNoEvent,
		"unknown cluster with incoming gen uid %s",
		incoming,
	)
	ClusterStatusGens.WriteStatusGen(cr.UID, incoming)
	ClusterStatusGens.ValidateStatusGen(cr.UID)
	return true
}

// handleFinalizers will remove our finalizer if deletion has been requested.
// Otherwise it will add our finalizer if it is absent.
func (r *ReconcileKubeDirectorCluster) handleFinalizers(
	reqLogger logr.Logger,
	cr *kdv1.KubeDirectorCluster,
) (bool, error) {

	if cr.DeletionTimestamp != nil {
		// If a deletion has been requested, while ours (or other) finalizers
		// existed on the CR, go ahead and remove our finalizer.
		removeErr := executor.RemoveClusterFinalizer(reqLogger, cr)
		if removeErr == nil {
			shared.LogInfo(
				reqLogger,
				cr,
				shared.EventReasonCluster,
				"greenlighting for deletion",
			)
		}
		// Also clear the status gen from our cache, regardless of whether
		// finalizer modification succeeded.
		ClusterStatusGens.DeleteStatusGen(cr.UID)
		shared.RemoveClusterAppReference(
			cr.Namespace,
			cr.Name,
			*(cr.Spec.AppCatalog),
			cr.Spec.AppID,
		)
		return true, removeErr
	}

	// If our finalizer doesn't exist on the CR, put it in there.
	ensureErr := executor.EnsureClusterFinalizer(reqLogger, cr)
	if ensureErr != nil {
		return true, ensureErr
	}

	return false, nil
}

// calcMembersForRoles generates a map of role name to list of all member
// in the role that are intended to exist -- i.e. members in states
// memberCreatePending, memberCreating, memberReady or memberConfigError
func calcMembersForRoles(
	roles []*roleInfo,
) map[string][]*kdv1.MemberStatus {

	result := make(map[string][]*kdv1.MemberStatus)
	for _, roleInfo := range roles {
		if roleInfo.roleSpec != nil {
			var membersStatus []*kdv1.MemberStatus

			membersStatus = append(
				append(
					append(
						roleInfo.membersByState[memberCreatePending],
						roleInfo.membersByState[memberCreating]...,
					),
					roleInfo.membersByState[memberReady]...,
				),
				roleInfo.membersByState[memberConfigError]...,
			)
			result[roleInfo.roleSpec.Name] = membersStatus
		}
	}
	return result
}
