package postgrescluster

/*
 Copyright 2021 Crunchy Data Solutions, Inc.
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

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/internal/logging"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/internal/patroni"
	"github.com/crunchydata/postgres-operator/internal/pgbackrest"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

const (
	// ConditionReplicaCreate is the type used in a condition to indicate whether or not
	// pgBackRest can be utilized for replica creation
	ConditionReplicaCreate = "PGBackRestReplicaCreate"

	// ConditionReplicaRepoReady is the type used in a condition to indicate whether or not
	// the pgBackRest repository for creating replicas is ready
	ConditionReplicaRepoReady = "PGBackRestReplicaRepoReady"

	// ConditionRepoHostReady is the type used in a condition to indicate whether or not a
	// pgBackRest repository host PostgresCluster is ready
	ConditionRepoHostReady = "PGBackRestRepoHostReady"

	// EventRepoHostNotFound is used to indicate that a pgBackRest repository was not
	// found when reconciling
	EventRepoHostNotFound = "RepoDeploymentNotFound"

	// EventRepoHostCreated is the event reason utilized when a pgBackRest repository host is
	// created
	EventRepoHostCreated = "RepoHostCreated"

	// EventUnableToCreateStanzas is the event reason utilized when pgBackRest is unable to create
	// stanzas for the repositories in a PostgreSQL cluster
	EventUnableToCreateStanzas = "UnableToCreateStanzas"

	// EventStanzasCreated is the event reason utilized when a pgBackRest stanza create command
	// completes successfully
	EventStanzasCreated = "StanzasCreated"

	// EventUnableToCreatePGBackRestCronJob is the event reason utilized when a pgBackRest backup
	// CronJob fails to create successfully
	EventUnableToCreatePGBackRestCronJob = "UnableToCreatePGBackRestCronJob"
)

// backup types
const (
	full         = "full"
	differential = "diff"
	incremental  = "incr"
)

// regexRepoIndex is the regex used to obtain the repo index from a pgBackRest repo name
var regexRepoIndex = regexp.MustCompile(`\d+`)

// RepoResources is used to store various resources for pgBackRest repositories and
// repository hosts
type RepoResources struct {
	cronjobs                []*batchv1beta1.CronJob
	replicaCreateBackupJobs []*batchv1.Job
	hosts                   []*appsv1.StatefulSet
	pvcs                    []*v1.PersistentVolumeClaim
	sshConfig               *v1.ConfigMap
	sshSecret               *v1.Secret
}

// applyRepoHostIntent ensures the pgBackRest repository host StatefulSet is synchronized with the
// proper configuration according to the provided PostgresCluster custom resource.  This is done by
// applying the PostgresCluster controller's fully specified intent for the repository host
// StatefulSet.  Any changes to the deployment spec as a result of synchronization will result in a
// rollout of the pgBackRest repository host StatefulSet in accordance with its configured
// strategy.
func (r *Reconciler) applyRepoHostIntent(ctx context.Context, postgresCluster *v1beta1.PostgresCluster,
	repoHostName string) (*appsv1.StatefulSet, error) {

	repo, err := r.generateRepoHostIntent(postgresCluster, repoHostName)
	if err != nil {
		return nil, err
	}

	if err := r.apply(ctx, repo); err != nil {
		return nil, err
	}

	return repo, nil
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=create;patch

// applyRepoVolumeIntent ensures the pgBackRest repository host deployment is synchronized with the
// proper configuration according to the provided PostgresCluster custom resource.  This is done by
// applying the PostgresCluster controller's fully specified intent for the PersistentVolumeClaim
// representing a repository.
func (r *Reconciler) applyRepoVolumeIntent(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster, spec *v1.PersistentVolumeClaimSpec,
	repoName string) (*v1.PersistentVolumeClaim, error) {

	repo, err := r.generateRepoVolumeIntent(postgresCluster, spec, repoName)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := r.apply(ctx, repo); err != nil {
		return nil, r.handlePersistentVolumeClaimError(postgresCluster,
			errors.WithStack(err))
	}

	return repo, nil
}

// getPGBackRestResources returns the existing pgBackRest resources that should utilized by the
// PostgresCluster controller during reconciliation.  Any items returned are verified to be owned
// by the PostgresCluster controller and still applicable per the current PostgresCluster spec.
// Additionally, and resources identified that no longer correspond to any current configuration
// are deleted.
func (r *Reconciler) getPGBackRestResources(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster) (*RepoResources, error) {

	repoResources := &RepoResources{}

	gvks := []schema.GroupVersionKind{{
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "ConfigMapList",
	}, {
		Group:   batchv1.SchemeGroupVersion.Group,
		Version: batchv1.SchemeGroupVersion.Version,
		Kind:    "JobList",
	}, {
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "PersistentVolumeClaimList",
	}, {
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "SecretList",
	}, {
		Group:   appsv1.SchemeGroupVersion.Group,
		Version: appsv1.SchemeGroupVersion.Version,
		Kind:    "StatefulSetList",
	}, {
		Group:   batchv1beta1.SchemeGroupVersion.Group,
		Version: batchv1beta1.SchemeGroupVersion.Version,
		Kind:    "CronJob",
	}}

	selector := naming.PGBackRestSelector(postgresCluster.GetName())
	for _, gvk := range gvks {
		uList := &unstructured.UnstructuredList{}
		uList.SetGroupVersionKind(gvk)
		if err := r.Client.List(context.Background(), uList,
			client.InNamespace(postgresCluster.GetNamespace()),
			client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, errors.WithStack(err)
		}
		if len(uList.Items) == 0 {
			continue
		}

		owned := []unstructured.Unstructured{}
		for i, u := range uList.Items {
			if metav1.IsControlledBy(&uList.Items[i], postgresCluster) {
				owned = append(owned, u)
			}
		}

		owned, err := r.cleanupRepoResources(ctx, postgresCluster, owned)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		uList.Items = owned
		if err := unstructuredToRepoResources(postgresCluster, gvk.Kind,
			repoResources, uList); err != nil {
			return nil, errors.WithStack(err)
		}
	}

	return repoResources, nil
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=delete

// cleanupRepoResources cleans up pgBackRest repository resources that should no longer be
// reconciled by deleting them.  This includes deleting repos (i.e. PersistentVolumeClaims) that
// are no longer associated with any repository configured within the PostgresCluster spec, or any
// pgBackRest repository host resources if a repository host is no longer configured.
func (r *Reconciler) cleanupRepoResources(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster,
	ownedResources []unstructured.Unstructured) ([]unstructured.Unstructured, error) {

	// stores the resources that should not be deleted
	ownedNoDelete := []unstructured.Unstructured{}
	for i, owned := range ownedResources {
		delete := true

		// helper to determine if a label is present in the PostgresCluster
		hasLabel := func(label string) bool { _, ok := owned.GetLabels()[label]; return ok }

		// this switch identifies the type of pgBackRest resource via its labels, and then
		// determines whether or not it should be deleted according to the current PostgresCluster
		// spec
		switch {
		case hasLabel(naming.LabelPGBackRestConfig):
			// Simply add the things we never want to delete (e.g. the pgBackRest configuration)
			// to the slice and do not delete
			ownedNoDelete = append(ownedNoDelete, owned)
			delete = false
		case hasLabel(naming.LabelPGBackRestDedicated):
			// If a dedicated repo host resource and a dedicated repo host is enabled, then
			// add to the slice and do not delete.  Note that dedicated repo host resources are
			// checked before repo host resources since both share the same "repo-host" label, and
			// we need to distiguish (and seprately handle) dedicated repo host resources.
			if pgbackrest.DedicatedRepoHostEnabled(postgresCluster) {
				ownedNoDelete = append(ownedNoDelete, owned)
				delete = false
			}
		case hasLabel(naming.LabelPGBackRestRepoHost):
			// If a repo host is enabled and this is a repo host resource, then add to the
			// slice and do not delete.  Note that dedicated repo host resources are checked
			// before repo host resources since both share the same "repo-host" label, and
			// we need to distiguish (and seprately handle) dedicated repo host resources.
			if pgbackrest.RepoHostEnabled(postgresCluster) {
				ownedNoDelete = append(ownedNoDelete, owned)
				delete = false
			}
		case hasLabel(naming.LabelPGBackRestRepoVolume):
			// If a volume (PVC) is identified for a repo that no longer exists in the
			// spec then delete it.  Otherwise add it to the slice and continue.
			// If a volume (PVC) is identified for a repo that no longer exists in the
			// spec then delete it.  Otherwise add it to the slice and continue.
			for _, repo := range postgresCluster.Spec.Archive.PGBackRest.Repos {
				// we only care about cleaning up local repo volumes (PVCs), and ignore other repo
				// types (e.g. for external Azure, GCS or S3 repositories)
				if repo.Volume != nil &&
					(repo.Name == owned.GetLabels()[naming.LabelPGBackRestRepo]) {
					ownedNoDelete = append(ownedNoDelete, owned)
					delete = false
				}
			}
		case hasLabel(naming.LabelPGBackRestBackup):
			// If a Job is identified for a repo that no longer exists in the spec then
			// delete it.  Otherwise add it to the slice and continue.
			for _, repo := range postgresCluster.Spec.Archive.PGBackRest.Repos {
				if repo.Name == owned.GetLabels()[naming.LabelPGBackRestRepo] {
					ownedNoDelete = append(ownedNoDelete, owned)
					delete = false
				}
			}
		case hasLabel(naming.LabelPGBackRestCronJob):
			for _, repo := range postgresCluster.Spec.Archive.PGBackRest.Repos {
				if repo.Name == owned.GetLabels()[naming.LabelPGBackRestRepo] {
					if backupScheduleFound(repo,
						owned.GetLabels()[naming.LabelPGBackRestCronJob]) {
						delete = false
						ownedNoDelete = append(ownedNoDelete, owned)
					}
					break
				}
			}
		}

		// If nothing has specified that the resource should not be deleted, then delete
		if delete {
			if err := r.Client.Delete(ctx, &ownedResources[i],
				client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				return []unstructured.Unstructured{}, errors.WithStack(err)
			}
		}
	}

	// return the remaining resources after properly cleaning up any that should no longer exist
	return ownedNoDelete, nil
}

// backupScheduleFound returns true if the CronJob in question should be created as
// defined by the postgrescluster CRD, otherwise it returns false.
func backupScheduleFound(repo v1beta1.PGBackRestRepo, backupType string) bool {
	if repo.BackupSchedules != nil {
		switch backupType {
		case full:
			return repo.BackupSchedules.Full != nil
		case differential:
			return repo.BackupSchedules.Differential != nil
		case incremental:
			return repo.BackupSchedules.Incremental != nil
		default:
			return false
		}
	}
	return false
}

// unstructuredToRepoResources converts unstructred pgBackRest repository resources (specifically
// unstructured StatefulSetLists and PersistentVolumeClaimList) into their structured equivalent.
func unstructuredToRepoResources(postgresCluster *v1beta1.PostgresCluster, kind string,
	repoResources *RepoResources, uList *unstructured.UnstructuredList) error {

	switch kind {
	case "ConfigMapList":
		var cmList v1.ConfigMapList
		if err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(uList.UnstructuredContent(), &cmList); err != nil {
			return errors.WithStack(err)
		}
		// we only care about ConfigMaps with the proper names
		for i, cm := range cmList.Items {
			if cm.GetName() == naming.PGBackRestSSHConfig(postgresCluster).Name {
				repoResources.sshConfig = &cmList.Items[i]
				break
			}
		}
	case "JobList":
		var jobList batchv1.JobList
		if err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(uList.UnstructuredContent(), &jobList); err != nil {
			return errors.WithStack(err)
		}
		// we only care about replica create backup jobs
		for i, job := range jobList.Items {
			val := job.GetLabels()[naming.LabelPGBackRestBackup]
			if val == string(naming.BackupReplicaCreate) {
				repoResources.replicaCreateBackupJobs =
					append(repoResources.replicaCreateBackupJobs, &jobList.Items[i])
			}
		}
	case "PersistentVolumeClaimList":
		var pvcList v1.PersistentVolumeClaimList
		if err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(uList.UnstructuredContent(), &pvcList); err != nil {
			return errors.WithStack(err)
		}
		for i := range pvcList.Items {
			repoResources.pvcs = append(repoResources.pvcs, &pvcList.Items[i])
		}
	case "SecretList":
		var secretList v1.SecretList
		if err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(uList.UnstructuredContent(), &secretList); err != nil {
			return errors.WithStack(err)
		}
		// we only care about Secret with the proper names
		for i, secret := range secretList.Items {
			if secret.GetName() == naming.PGBackRestSSHSecret(postgresCluster).Name {
				repoResources.sshSecret = &secretList.Items[i]
				break
			}
		}
	case "StatefulSetList":
		var stsList appsv1.StatefulSetList
		if err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(uList.UnstructuredContent(), &stsList); err != nil {
			return errors.WithStack(err)
		}
		for i := range stsList.Items {
			repoResources.hosts = append(repoResources.hosts, &stsList.Items[i])
		}
	case "CronJob":
		var cronList batchv1beta1.CronJobList
		if err := runtime.DefaultUnstructuredConverter.
			FromUnstructured(uList.UnstructuredContent(), &cronList); err != nil {
			return errors.WithStack(err)
		}
		for i := range cronList.Items {
			repoResources.cronjobs = append(repoResources.cronjobs, &cronList.Items[i])
		}
	default:
		return fmt.Errorf("unexpected kind %q", kind)
	}

	return nil
}

// generateRepoHostIntent creates and populates StatefulSet with the PostgresCluster's full intent
// as needed to create and reconcile a pgBackRest dedicated repository host within the kubernetes
// cluster.
func (r *Reconciler) generateRepoHostIntent(postgresCluster *v1beta1.PostgresCluster,
	repoHostName string) (*appsv1.StatefulSet, error) {

	annotations := naming.Merge(
		postgresCluster.Spec.Metadata.GetAnnotationsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetAnnotationsOrNil())
	labels := naming.Merge(
		postgresCluster.Spec.Metadata.GetLabelsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetLabelsOrNil(),
		naming.PGBackRestDedicatedLabels(postgresCluster.GetName()),
	)

	repo := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "StatefulSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        repoHostName,
			Namespace:   postgresCluster.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: naming.PGBackRestDedicatedLabels(postgresCluster.GetName()),
			},
			ServiceName: naming.ClusterPodService(postgresCluster).Name,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
			},
		},
	}

	podSecurityContext := &v1.PodSecurityContext{SupplementalGroups: []int64{65534}}
	// set fsGroups if not OpenShift
	if postgresCluster.Spec.OpenShift == nil || !*postgresCluster.Spec.OpenShift {
		podSecurityContext.FSGroup = initialize.Int64(26)
	}
	repo.Spec.Template.Spec.SecurityContext = podSecurityContext

	// add ssh pod info
	if err := pgbackrest.AddSSHToPod(postgresCluster, &repo.Spec.Template); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := pgbackrest.AddRepoVolumesToPod(postgresCluster, &repo.Spec.Template,
		naming.PGBackRestRepoContainerName); err != nil {
		return nil, errors.WithStack(err)
	}
	// add configs to pod
	if err := pgbackrest.AddConfigsToPod(postgresCluster, &repo.Spec.Template,
		pgbackrest.CMRepoKey, naming.PGBackRestRepoContainerName); err != nil {
		return nil, errors.WithStack(err)
	}

	// add nss_wrapper init container and add nss_wrapper env vars to the pgbackrest
	// container
	addNSSWrapper(postgresCluster.Spec.Archive.PGBackRest.Image, &repo.Spec.Template)
	addTMPEmptyDir(&repo.Spec.Template)

	// set ownership references
	if err := controllerutil.SetControllerReference(postgresCluster, repo,
		r.Client.Scheme()); err != nil {
		return nil, err
	}

	return repo, nil
}

func (r *Reconciler) generateRepoVolumeIntent(postgresCluster *v1beta1.PostgresCluster,
	spec *v1.PersistentVolumeClaimSpec, repoName string) (*v1.PersistentVolumeClaim, error) {

	annotations := naming.Merge(
		postgresCluster.Spec.Metadata.GetAnnotationsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetAnnotationsOrNil())
	labels := naming.Merge(
		postgresCluster.Spec.Metadata.GetLabelsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetLabelsOrNil(),
		naming.PGBackRestRepoVolumeLabels(postgresCluster.GetName(), repoName),
	)

	// generate metadata
	meta := naming.PGBackRestRepoVolume(postgresCluster, repoName)
	meta.Labels = labels
	meta.Annotations = annotations

	repoVol := &v1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1.SchemeGroupVersion.String(),
			Kind:       "PersistentVolumeClaim",
		},
		ObjectMeta: meta,
		Spec:       *spec,
	}

	// set ownership references
	if err := controllerutil.SetControllerReference(postgresCluster, repoVol,
		r.Client.Scheme()); err != nil {
		return nil, err
	}

	return repoVol, nil
}

// generateBackupJobSpecIntent generates a JobSpec for a pgBackRest backup job
func generateBackupJobSpecIntent(postgresCluster *v1beta1.PostgresCluster, selector,
	containerName, repoName, serviceAccountName, configName string,
	labels map[string]string) (*batchv1.JobSpec, error) {

	repoIndex := regexRepoIndex.FindString(repoName)
	cmdOpts := []string{
		"--stanza=" + pgbackrest.DefaultStanzaName,
		"--repo=" + repoIndex,
	}

	jobSpec := &batchv1.JobSpec{
		Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Command: []string{"/opt/crunchy/bin/pgbackrest"},
					Env: []v1.EnvVar{
						{Name: "COMMAND", Value: "backup"},
						{Name: "COMMAND_OPTS", Value: strings.Join(cmdOpts, " ")},
						{Name: "COMPARE_HASH", Value: "true"},
						{Name: "CONTAINER", Value: containerName},
						{Name: "NAMESPACE", Value: postgresCluster.GetNamespace()},
						{Name: "SELECTOR", Value: selector},
					},
					Image: postgresCluster.Spec.Archive.PGBackRest.Image,
					Name:  naming.PGBackRestRepoContainerName,
				}},
				RestartPolicy:      v1.RestartPolicyNever,
				ServiceAccountName: serviceAccountName,
			},
		},
	}
	// add pgBackRest configs to template
	if err := pgbackrest.AddConfigsToPod(postgresCluster, &jobSpec.Template,
		configName, naming.PGBackRestRepoContainerName); err != nil {
		return nil, errors.WithStack(err)
	}

	return jobSpec, nil
}

// reconcilePGBackRest is responsible for reconciling any/all pgBackRest resources owned by a
// specific PostgresCluster (e.g. Deployments, ConfigMaps, Secrets, etc.).  This function will
// ensure various reconciliation logic is run as needed for each pgBackRest resource, while then
// also generating the proper Result as needed to ensure proper event requeuing according to
// the results of any attempts to properly reconcile these resources.
func (r *Reconciler) reconcilePGBackRest(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster, instanceNames []string) (reconcile.Result, error) {

	// add some additional context about what component is being reconciled
	log := logging.FromContext(ctx).WithValues("reconciler", "pgBackRest")

	// if nil, create the pgBackRest status that will be updated when reconciling various
	// pgBackRest resources
	if postgresCluster.Status.PGBackRest == nil {
		postgresCluster.Status.PGBackRest = &v1beta1.PGBackRestStatus{}
	}

	// create the Result that will be updated while reconciling any/all pgBackRest resources
	result := reconcile.Result{}

	// Get all currently owned pgBackRest resources in the environment as needed for
	// reconciliation.  This includes deleting resources that should no longer exist per the
	// current spec (e.g. if repos, repo hosts, etc. have been removed).
	repoResources, err := r.getPGBackRestResources(ctx, postgresCluster)
	if err != nil {
		// exit early if can't get and clean existing resources as needed to reconcile
		return reconcile.Result{}, errors.WithStack(err)
	}

	var repoHost *appsv1.StatefulSet
	var repoHostName string
	dedicatedEnabled := (postgresCluster.Spec.Archive.PGBackRest.RepoHost != nil) &&
		(postgresCluster.Spec.Archive.PGBackRest.RepoHost.Dedicated != nil)
	if dedicatedEnabled {
		// reconcile the pgbackrest repository host
		repoHost, err = r.reconcileDedicatedRepoHost(ctx, postgresCluster, repoResources)
		if err != nil {
			log.Error(err, "unable to reconcile pgBackRest repo host")
			result = updateReconcileResult(result, reconcile.Result{Requeue: true})
		}
		repoHostName = repoHost.GetName()
	} else if len(postgresCluster.Status.Conditions) > 0 {
		// remove the dedicated repo host status if a dedicated host is not enabled
		meta.RemoveStatusCondition(&postgresCluster.Status.Conditions, ConditionRepoHostReady)
	}

	// calculate hashes for the external repository configurations in the spec (e.g. for Azure,
	// GCS and/or S3 repositories) as needed to properly detect changes to external repository
	// configuration (and then execute stanza create commands accordingly)
	configHashes, configHash, err := pgbackrest.CalculateConfigHashes(postgresCluster)
	if err != nil {
		log.Error(err, "unable to calculate config hashes")
		result = updateReconcileResult(result, reconcile.Result{Requeue: true})
	}

	// reconcile all pgbackrest repository repos
	replicaCreateRepo, err := r.reconcileRepos(ctx, postgresCluster, configHashes)
	if err != nil {
		log.Error(err, "unable to reconcile pgBackRest repo host")
		result = updateReconcileResult(result, reconcile.Result{Requeue: true})
	}

	// reconcile all pgbackrest configuration and secrets
	if err := r.reconcilePGBackRestConfig(ctx, postgresCluster, repoHostName,
		configHash, instanceNames, repoResources.sshSecret); err != nil {
		log.Error(err, "unable to reconcile pgBackRest configuration")
		result = updateReconcileResult(result, reconcile.Result{Requeue: true})
	}

	// reconcile the RBAC required to run pgBackRest Jobs (e.g. for backups)
	sa, err := r.reconcilePGBackRestRBAC(ctx, postgresCluster)
	if err != nil {
		log.Error(err, "unable to create replica creation backup")
		result = updateReconcileResult(result, reconcile.Result{Requeue: true})
	}

	// reconcile the pgBackRest stanza for all configuration pgBackRest repos
	configHashMismatch, err := r.reconcileStanzaCreate(ctx, postgresCluster, configHash)
	// If a stanza create error then requeue but don't return the error.  This prevents
	// stanza-create errors from bubbling up to the main Reconcile() function, which would
	// prevent subsequent reconciles from occurring.  Also, this provides a better chance
	// that the pgBackRest status will be updated at the end of the Reconcile() function,
	// e.g. to set the "stanzaCreated" indicator to false for any repos failing stanza creation
	// (assuming no other reconcile errors bubble up to the Reconcile() function and block the
	// status update).  And finally, add some time to each requeue to slow down subsequent
	// stanza create attempts in order to prevent pgBackRest mis-configuration (e.g. due to
	// custom confiugration) from spamming the logs, while also ensuring stanza creation is
	// re-attempted until successful (e.g. allowing users to correct mis-configurations in
	// custom configuration and ensure stanzas are still created).
	if err != nil {
		log.Error(err, "unable to create stanza")
		result = updateReconcileResult(result, reconcile.Result{RequeueAfter: 10 * time.Second})
	}
	// If a config hash mismatch, then log an info message and requeue to try again.  Add some time
	// to the requeue to give the pgBackRest configuration changes a chance to propagate to the
	// container.
	if configHashMismatch {
		log.Info("pgBackRest config hash mismatch detected, requeuing to reattempt stanza create")
		result = updateReconcileResult(result, reconcile.Result{RequeueAfter: 10 * time.Second})
	}

	// reconcile the pgBackRest backup CronJobs
	requeue := r.reconcilePGBackRestCronJob(ctx, postgresCluster)
	// If the pgBackRest backup CronJob reconciliation function has encountered an error, requeue
	// after 10 seconds. The error will not bubble up to allow the reconcile loop to continue.
	// An error is not logged because an event was already created.
	// TODO(tjmoore4): Is this the desired eventing/logging/reconciliation strategy?
	// A potential option to handle this proactively would be to use a webhook:
	// https://book.kubebuilder.io/cronjob-tutorial/webhook-implementation.html
	if requeue {
		result = updateReconcileResult(result, reconcile.Result{RequeueAfter: 10 * time.Second})
	}

	// Reconcile the initial backup that is needed to enable replica creation using pgBackRest.
	// This is done once stanza creation is successful
	if err := r.reconcileReplicaCreateBackup(ctx, postgresCluster,
		repoResources.replicaCreateBackupJobs, sa, configHash, replicaCreateRepo); err != nil {
		log.Error(err, "unable to create replica creation backup")
		result = updateReconcileResult(result, reconcile.Result{Requeue: true})
	}

	return result, nil
}

// reconcileRepoHosts is responsible for reconciling the pgBackRest ConfigMaps and Secrets.
func (r *Reconciler) reconcilePGBackRestConfig(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster, repoHostName, configHash string,
	instanceNames []string, sshSecret *v1.Secret) error {

	log := logging.FromContext(ctx).WithValues("reconcileResource", "repoConfig")
	errMsg := "reconciling pgBackRest configuration"

	backrestConfig := pgbackrest.CreatePGBackRestConfigMapIntent(postgresCluster, repoHostName,
		configHash, instanceNames)
	if err := controllerutil.SetControllerReference(postgresCluster, backrestConfig,
		r.Client.Scheme()); err != nil {
		return err
	}
	if err := r.apply(ctx, backrestConfig); err != nil {
		return errors.WithStack(err)
	}

	repoHostConfigured := (postgresCluster.Spec.Archive.PGBackRest.RepoHost != nil)

	if !repoHostConfigured {
		log.V(1).Info("skipping SSH reconciliation, no repo hosts configured")
		return nil
	}

	sshdConfig := pgbackrest.CreateSSHConfigMapIntent(postgresCluster)
	// set ownership references
	if err := controllerutil.SetControllerReference(postgresCluster, &sshdConfig,
		r.Client.Scheme()); err != nil {
		log.Error(err, errMsg)
		return err
	}
	if err := r.apply(ctx, &sshdConfig); err != nil {
		log.Error(err, errMsg)
		return err
	}

	sshdSecret, err := pgbackrest.CreateSSHSecretIntent(postgresCluster, sshSecret)
	if err != nil {
		log.Error(err, errMsg)
		return err
	}
	if err := controllerutil.SetControllerReference(postgresCluster, &sshdSecret,
		r.Client.Scheme()); err != nil {
		return err
	}
	if err := r.apply(ctx, &sshdSecret); err != nil {
		log.Error(err, errMsg)
		return err
	}

	return nil
}

// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;patch

// reconcileInstanceRBAC reconciles the Role, RoleBinding, and ServiceAccount for
// pgBackRest
func (r *Reconciler) reconcilePGBackRestRBAC(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster) (*v1.ServiceAccount, error) {

	sa := &v1.ServiceAccount{ObjectMeta: naming.PGBackRestRBAC(postgresCluster)}
	sa.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("ServiceAccount"))

	role := &rbacv1.Role{ObjectMeta: naming.PGBackRestRBAC(postgresCluster)}
	role.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("Role"))

	binding := &rbacv1.RoleBinding{ObjectMeta: naming.PGBackRestRBAC(postgresCluster)}
	binding.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("RoleBinding"))

	if err := r.setControllerReference(postgresCluster, sa); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := r.setControllerReference(postgresCluster, binding); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := r.setControllerReference(postgresCluster, role); err != nil {
		return nil, errors.WithStack(err)
	}

	sa.Annotations = naming.Merge(postgresCluster.Spec.Metadata.GetAnnotationsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetAnnotationsOrNil())
	sa.Labels = naming.Merge(postgresCluster.Spec.Metadata.GetLabelsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetLabelsOrNil(),
		naming.PGBackRestLabels(postgresCluster.GetName()))
	binding.Annotations = naming.Merge(postgresCluster.Spec.Metadata.GetAnnotationsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetAnnotationsOrNil())
	binding.Labels = naming.Merge(postgresCluster.Spec.Metadata.GetLabelsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetLabelsOrNil(),
		naming.PGBackRestLabels(postgresCluster.GetName()))
	role.Annotations = naming.Merge(postgresCluster.Spec.Metadata.GetAnnotationsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetAnnotationsOrNil())
	role.Labels = naming.Merge(postgresCluster.Spec.Metadata.GetLabelsOrNil(),
		postgresCluster.Spec.Archive.PGBackRest.Metadata.GetLabelsOrNil(),
		naming.PGBackRestLabels(postgresCluster.GetName()))

	binding.RoleRef = rbacv1.RoleRef{
		APIGroup: rbacv1.SchemeGroupVersion.Group,
		Kind:     role.Kind,
		Name:     role.Name,
	}
	binding.Subjects = []rbacv1.Subject{{
		Kind: sa.Kind,
		Name: sa.Name,
	}}
	role.Rules = pgbackrest.Permissions(postgresCluster)

	if err := r.apply(ctx, sa); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := r.apply(ctx, role); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := r.apply(ctx, binding); err != nil {
		return nil, errors.WithStack(err)
	}

	return sa, nil
}

// reconcileDedicatedRepoHost is responsible for reconciling a pgBackRest dedicated repository host
// StatefulSet according to a specific PostgresCluster custom resource.
func (r *Reconciler) reconcileDedicatedRepoHost(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster,
	repoResources *RepoResources) (*appsv1.StatefulSet, error) {

	log := logging.FromContext(ctx).WithValues("reconcileResource", "repoHost")

	// ensure conditions are set before returning as needed by subsequent reconcile functions
	defer func() {
		repoHostReady := metav1.Condition{
			ObservedGeneration: postgresCluster.GetGeneration(),
			Type:               ConditionRepoHostReady,
		}
		if postgresCluster.Status.PGBackRest.RepoHost == nil {
			repoHostReady.Status = metav1.ConditionUnknown
			repoHostReady.Reason = "RepoHostStatusMissing"
			repoHostReady.Message = "pgBackRest dedicated repository host status is missing"
		} else if postgresCluster.Status.PGBackRest.RepoHost.Ready {
			repoHostReady.Status = metav1.ConditionTrue
			repoHostReady.Reason = "RepoHostReady"
			repoHostReady.Message = "pgBackRest dedicated repository host is ready"
		} else {
			repoHostReady.Status = metav1.ConditionFalse
			repoHostReady.Reason = "RepoHostNotReady"
			repoHostReady.Message = "pgBackRest dedicated repository host is not ready"
		}
		meta.SetStatusCondition(&postgresCluster.Status.Conditions, repoHostReady)
	}()

	var isCreate bool
	if len(repoResources.hosts) == 0 {
		name := fmt.Sprintf("%s-%s", postgresCluster.GetName(), "repo-host")
		repoResources.hosts = append(repoResources.hosts, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			}})
		isCreate = true
	} else {
		sort.Slice(repoResources.hosts, func(i, j int) bool {
			return repoResources.hosts[i].CreationTimestamp.Before(
				&repoResources.hosts[j].CreationTimestamp)
		})
	}
	repoHostName := repoResources.hosts[0].Name
	repoHost, err := r.applyRepoHostIntent(ctx, postgresCluster, repoHostName)
	if err != nil {
		log.Error(err, "reconciling repository host")
		return nil, err
	}

	postgresCluster.Status.PGBackRest.RepoHost = getRepoHostStatus(repoHost)

	if isCreate {
		r.Recorder.Eventf(postgresCluster, v1.EventTypeNormal, EventRepoHostCreated,
			"created pgBackRest repository host %s/%s", repoHost.TypeMeta.Kind, repoHostName)
	}

	return repoHost, nil
}

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;patch;update;delete

// reconcileReplicaCreateBackup is responsible for reconciling a full pgBackRest backup for the
// cluster as required to create replicas
func (r *Reconciler) reconcileReplicaCreateBackup(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster, replicaCreateBackupJobs []*batchv1.Job,
	serviceAccount *v1.ServiceAccount, configHash, replicaCreateRepoName string) error {

	var replicaCreateRepoStatus *v1beta1.RepoStatus
	for i, r := range postgresCluster.Status.PGBackRest.Repos {
		if r.Name == replicaCreateRepoName {
			replicaCreateRepoStatus = &postgresCluster.Status.PGBackRest.Repos[i]
			break
		}
	}

	// ensure condition is set before returning as needed by subsequent reconcile functions
	defer func() {
		replicaCreate := metav1.Condition{
			ObservedGeneration: postgresCluster.GetGeneration(),
			Type:               ConditionReplicaCreate,
		}
		if replicaCreateRepoStatus == nil {
			replicaCreate.Status = metav1.ConditionUnknown
			replicaCreate.Reason = "RepoStatusMissing"
			replicaCreate.Message = "Status is missing for the replica create repo"
		} else if replicaCreateRepoStatus.ReplicaCreateBackupComplete {
			replicaCreate.Status = metav1.ConditionTrue
			replicaCreate.Reason = "RepoBackupComplete"
			replicaCreate.Message = "pgBackRest replica creation is now possible"
		} else {
			replicaCreate.Status = metav1.ConditionFalse
			replicaCreate.Reason = "RepoBackupNotComplete"
			replicaCreate.Message = "pgBackRest replica creation is not currently " +
				"possible"
		}
		meta.SetStatusCondition(&postgresCluster.Status.Conditions, replicaCreate)
	}()

	// if the cluster has yet to be bootstrapped, or if the replicaCreateRepoStatus is nil,
	// then simply return
	if !patroni.ClusterBootstrapped(postgresCluster) || replicaCreateRepoStatus == nil {
		return nil
	}

	// simply return if the backup is already complete
	if replicaCreateRepoStatus.ReplicaCreateBackupComplete {
		return nil
	}

	// determine if the replica create repo is ready using the "PGBackRestReplicaRepoReady" condition
	var replicaRepoReady bool
	condition := meta.FindStatusCondition(postgresCluster.Status.Conditions, ConditionReplicaRepoReady)
	if condition != nil {
		replicaRepoReady = (condition.Status == metav1.ConditionTrue)
	}

	// get pod name and container name as needed to exec into the proper pod and create
	// the pgBackRest backup
	selector, containerName, err := getPGBackRestExecSelector(postgresCluster)
	if err != nil {
		return errors.WithStack(err)
	}

	// Find the name of the current primary.  Only proceed if/when the primary can be identified
	pods := &v1.PodList{}
	if err := r.Client.List(ctx, pods, client.InNamespace(postgresCluster.GetNamespace()),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return errors.WithStack(err)
	}
	if len(pods.Items) != 1 {
		return errors.WithStack(
			errors.New("invalid number of Pods found when attempting to create replica create " +
				"backup"))
	}
	primaryInstance := pods.Items[0].GetLabels()[naming.LabelInstance]

	// set the name of the pgbackrest config file that will be mounted to the backup Job
	configName := primaryInstance + ".conf"
	if pgbackrest.DedicatedRepoHostEnabled(postgresCluster) {
		configName = pgbackrest.CMRepoKey
	}

	// determine if the dedicated repository host is ready using the repo host ready status
	dedicatedRepoReady := true
	condition = meta.FindStatusCondition(postgresCluster.Status.Conditions, ConditionRepoHostReady)
	if condition != nil {
		dedicatedRepoReady = (condition.Status == metav1.ConditionTrue)
	}

	// grab the current job if one exists, and perform any required Job cleanup or update the
	// PostgresCluster status as required
	var job *batchv1.Job
	if len(replicaCreateBackupJobs) > 0 {
		job = replicaCreateBackupJobs[0]

		failed := jobFailed(job)
		completed := jobCompleted(job)

		// Delete a running Job under the following conditions:
		// - The dedicated repo host is not ready.  Wait for it to become ready, at which point the
		//   Job will be recreated to try again.
		// - The replica creation repo is not ready.  Wait for it to be ready (i.e. after
		//   successful stanza creation) and try again.
		if (!completed && !failed) && (!dedicatedRepoReady || !replicaRepoReady) {
			if err := r.Client.Delete(ctx, job,
				client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				return errors.WithStack(err)
			}
		}

		// determine if the replica creation repo has changed
		replicaCreateRepoChanged := true
		if replicaCreateRepoName == job.GetLabels()[naming.LabelPGBackRestRepo] {
			replicaCreateRepoChanged = false
		}

		// Delete an existing Job (whether running or not) under the following conditions:
		// - The job has failed.  The Job will be deleted and recreated to try again.
		// - The replica creation repo has changed since the Job was created.  Delete and recreate
		//   with the Job with the proper repo configured.
		// - The "config" annotation has changed, indicating there is a new primary.  Delete and
		//   recreate the Job with the proper config mounted (applicable when a dedicated repo
		//   host is not enabled).
		// - The "config hash" annotation has changed, indicating a configuration change has been
		//   made in the spec (specifically a change to the config for an external repo).  Delete
		//   and recreate the Job with proper hash per the current config.
		if failed || replicaCreateRepoChanged ||
			(job.GetAnnotations()[naming.PGBackRestCurrentConfig] != configName) ||
			(job.GetAnnotations()[naming.PGBackRestConfigHash] != configHash) {
			if err := r.Client.Delete(ctx, job,
				client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				return errors.WithStack(err)
			}
			return nil
		}

		// if the Job completed then update status and return
		if completed {
			replicaCreateRepoStatus.ReplicaCreateBackupComplete = true
			return nil
		}
	}

	// return if the replica repo or the dedicated repo host is not ready
	if !dedicatedRepoReady || !replicaRepoReady {
		return nil
	}

	var labels map[string]string
	// create the backup Job, and populate ObjectMeta based on whether or not a Job already exists
	backupJob := &batchv1.Job{}
	if job != nil {
		backupJob.ObjectMeta.Name = job.ObjectMeta.GetName()
		backupJob.ObjectMeta.Namespace = job.ObjectMeta.GetNamespace()
		labels = naming.Merge(postgresCluster.Spec.Metadata.GetLabelsOrNil(),
			postgresCluster.Spec.Archive.PGBackRest.Metadata.GetLabelsOrNil(),
			job.ObjectMeta.GetLabels())
		backupJob.ObjectMeta.Annotations = naming.Merge(
			postgresCluster.Spec.Metadata.GetAnnotationsOrNil(),
			postgresCluster.Spec.Archive.PGBackRest.Metadata.GetAnnotationsOrNil(),
			job.ObjectMeta.GetAnnotations())
	} else {
		backupJob.ObjectMeta = naming.PGBackRestBackupJob(postgresCluster)
		labels = naming.Merge(postgresCluster.Spec.Metadata.GetLabelsOrNil(),
			postgresCluster.Spec.Archive.PGBackRest.Metadata.GetLabelsOrNil(),
			naming.PGBackRestBackupJobLabels(postgresCluster.GetName(),
				postgresCluster.Spec.Archive.PGBackRest.Repos[0].Name, naming.BackupReplicaCreate))
		backupJob.ObjectMeta.Annotations = naming.Merge(
			postgresCluster.Spec.Metadata.GetAnnotationsOrNil(),
			postgresCluster.Spec.Archive.PGBackRest.Metadata.GetAnnotationsOrNil(),
			map[string]string{
				naming.PGBackRestCurrentConfig: configName,
				naming.PGBackRestConfigHash:    configHash,
			})
	}

	// set the labels for the Job and generate and set the JobSpec intent
	backupJob.ObjectMeta.Labels = labels
	spec, err := generateBackupJobSpecIntent(postgresCluster, selector.String(), containerName,
		replicaCreateRepoName, serviceAccount.GetName(), configName, labels)
	if err != nil {
		return errors.WithStack(err)
	}
	backupJob.Spec = *spec

	// set gvk and ownership refs
	backupJob.SetGroupVersionKind(batchv1.SchemeGroupVersion.WithKind("Job"))
	if err := controllerutil.SetControllerReference(postgresCluster, backupJob,
		r.Client.Scheme()); err != nil {
		return errors.WithStack(err)
	}

	if err := r.apply(ctx, backupJob); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// reconcileRepos is responsible for reconciling any pgBackRest repositories configured
// for the cluster
func (r *Reconciler) reconcileRepos(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster, extConfigHashes map[string]string) (string, error) {

	log := logging.FromContext(ctx).WithValues("reconcileResource", "repoVolume")

	errors := []error{}
	errMsg := "reconciling repository volume"
	repoVols := []*v1.PersistentVolumeClaim{}
	var replicaCreateRepoName string
	for i, repo := range postgresCluster.Spec.Archive.PGBackRest.Repos {
		// the repo at index 0 is the replica creation repo
		if i == 0 {
			replicaCreateRepoName = repo.Name
		}
		// we only care about reconciling repo volumes, so ignore everything else
		if repo.Volume == nil {
			continue
		}
		repo, err := r.applyRepoVolumeIntent(ctx, postgresCluster, &repo.Volume.VolumeClaimSpec,
			repo.Name)
		if err != nil {
			log.Error(err, errMsg)
			errors = append(errors, err)
			continue
		}
		repoVols = append(repoVols, repo)
	}

	postgresCluster.Status.PGBackRest.Repos =
		getRepoVolumeStatus(postgresCluster.Status.PGBackRest.Repos, repoVols, extConfigHashes,
			replicaCreateRepoName)

	if len(errors) > 0 {
		return "", utilerrors.NewAggregate(errors)
	}

	return replicaCreateRepoName, nil
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

// reconcileStanzaCreate is responsible for ensuring stanzas are properly created for the
// pgBackRest repositories configured for a PostgresCluster.  If the bool returned from this
// function is false, this indicates that a pgBackRest config hash mismatch was identified that
// prevented the "pgbackrest stanza-create" command from running (with a config has mitmatch
// indicating that pgBackRest configuration as stored in the pgBackRest ConfigMap has not yet
// propagated to the Pod).
func (r *Reconciler) reconcileStanzaCreate(ctx context.Context,
	postgresCluster *v1beta1.PostgresCluster, configHash string) (bool, error) {

	// ensure conditions are set before returning as needed by subsequent reconcile functions
	defer func() {
		var replicaCreateRepoStatus *v1beta1.RepoStatus
		if len(postgresCluster.Spec.Archive.PGBackRest.Repos) == 0 {
			return
		}
		replicaCreateRepoName := postgresCluster.Spec.Archive.PGBackRest.Repos[0].Name
		for i, r := range postgresCluster.Status.PGBackRest.Repos {
			if r.Name == replicaCreateRepoName {
				replicaCreateRepoStatus = &postgresCluster.Status.PGBackRest.Repos[i]
				break
			}
		}

		replicaCreateRepoReady := metav1.Condition{
			ObservedGeneration: postgresCluster.GetGeneration(),
			Type:               ConditionReplicaRepoReady,
		}
		if replicaCreateRepoStatus == nil {
			replicaCreateRepoReady.Status = metav1.ConditionUnknown
			replicaCreateRepoReady.Reason = "RepoStatusMissing"
			replicaCreateRepoReady.Message = "Status is missing for the replica creation repo"
		} else if replicaCreateRepoStatus.StanzaCreated {
			replicaCreateRepoReady.Status = metav1.ConditionTrue
			replicaCreateRepoReady.Reason = "StanzaCreated"
			replicaCreateRepoReady.Message = "pgBackRest replica create repo is ready for " +
				"backups"
		} else {
			replicaCreateRepoReady.Status = metav1.ConditionFalse
			replicaCreateRepoReady.Reason = "StanzaNotCreated"
			replicaCreateRepoReady.Message = "pgBackRest replica create repo is not ready " +
				"for backups"
		}
		meta.SetStatusCondition(&postgresCluster.Status.Conditions, replicaCreateRepoReady)
	}()

	// determine if the cluster has been initialized
	clusterBootstrapped := patroni.ClusterBootstrapped(postgresCluster)

	// determine if the dedicated repository host is ready using the repo host ready status
	dedicatedRepoReady := true
	condition := meta.FindStatusCondition(postgresCluster.Status.Conditions, ConditionRepoHostReady)
	if condition != nil {
		dedicatedRepoReady = (condition.Status == metav1.ConditionTrue)
	}

	stanzasCreated := true
	for _, repoStatus := range postgresCluster.Status.PGBackRest.Repos {
		if !repoStatus.StanzaCreated {
			stanzasCreated = false
			break
		}
	}

	// return if the cluster has not yet been initialized, or if it has been initialized and
	// all stanzas have already been created successfully
	if !clusterBootstrapped || !dedicatedRepoReady || stanzasCreated {
		return false, nil
	}

	// get pod name and container name as needed to exec into the proper pod and create
	// pgBackRest stanzas
	selector, containerName, err := getPGBackRestExecSelector(postgresCluster)
	if err != nil {
		return false, errors.WithStack(err)
	}

	pods := &v1.PodList{}
	if err := r.Client.List(ctx, pods, client.InNamespace(postgresCluster.GetNamespace()),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return false, err
	}

	// TODO(andrewlecuyer): Returning an error to address an out-of-sync cache (e.g, if the
	// expected Pods are not found) is a symptom of a missed event. Consider watching Pods instead
	// instead to ensure the these events are not missed
	if len(pods.Items) != 1 {
		return false, errors.WithStack(
			errors.New("invalid number of Pods found when attempting to create stanzas"))
	}

	// create a pgBackRest executor and attempt stanza creation
	exec := func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer,
		command ...string) error {
		return r.PodExec(postgresCluster.GetNamespace(), pods.Items[0].GetName(), containerName,
			stdin, stdout, stderr, command...)
	}
	configHashMismatch, err := pgbackrest.Executor(exec).StanzaCreate(ctx, configHash)
	if err != nil {
		// record and log any errors resulting from running the stanza-create command
		r.Recorder.Event(postgresCluster, v1.EventTypeWarning, EventUnableToCreateStanzas,
			err.Error())

		return false, errors.WithStack(err)
	}
	// Don't record event or return an error if configHashMismatch is true, since this just means
	// configuration changes in ConfigMaps/Secrets have not yet propagated to the container.
	// Therefore, just log an an info message and return an error to requeue and try again.
	if configHashMismatch {

		return true, nil
	}

	// record an event indicating successful stanza creation
	r.Recorder.Event(postgresCluster, v1.EventTypeNormal, EventStanzasCreated,
		"pgBackRest stanza creation completed successfully")

	// if no errors then stanza(s) created successfully
	for i := range postgresCluster.Status.PGBackRest.Repos {
		postgresCluster.Status.PGBackRest.Repos[i].StanzaCreated = true
	}

	return false, nil
}

// getPGBackRestExecSelector returns a selector and container name that allows the proper
// Pod (along with a specific container within it) to be found within the Kubernetes
// cluster as needed to exec into the container and run a pgBackRest command.
func getPGBackRestExecSelector(
	postgresCluster *v1beta1.PostgresCluster) (labels.Selector, string, error) {

	clusterName := postgresCluster.GetName()

	// create the proper pod selector based on whether or not a a dedicated repository host is
	// enabled.  If a dedicated repo host is enabled, then the pgBackRest command will be
	// run there.  Otherwise it will be run on the current primary.
	dedicatedEnabled := pgbackrest.DedicatedRepoHostEnabled(postgresCluster)
	repoHostEnabled := pgbackrest.RepoHostEnabled(postgresCluster)
	var err error
	var podSelector labels.Selector
	var containerName string
	if dedicatedEnabled {
		podSelector = naming.PGBackRestDedicatedSelector(clusterName)
		containerName = naming.PGBackRestRepoContainerName
	} else {
		primarySelector := naming.ClusterPrimary(clusterName)
		podSelector, err = metav1.LabelSelectorAsSelector(&primarySelector)
		if err != nil {
			return nil, "", err
		}
		// There will only be a pgBackRest container if using a repo host.  Otherwise
		// the pgBackRest command will be run in the database container.
		if repoHostEnabled {
			containerName = naming.PGBackRestRepoContainerName
		} else {
			containerName = naming.ContainerDatabase
		}
	}

	return podSelector, containerName, nil
}

// getRepoHostStatus is responsible for returning the pgBackRest status for the provided pgBackRest
// repository host
func getRepoHostStatus(repoHost *appsv1.StatefulSet) *v1beta1.RepoHostStatus {

	repoHostStatus := &v1beta1.RepoHostStatus{}

	repoHostStatus.TypeMeta = repoHost.TypeMeta

	if repoHost.Status.ReadyReplicas == *repoHost.Spec.Replicas {
		repoHostStatus.Ready = true
	} else {
		repoHostStatus.Ready = false
	}

	return repoHostStatus
}

// getRepoVolumeStatus is responsible for creating an array of repo statuses based on the
// existing/current status for any repos in the cluster, the repository volumes
// (i.e. PVCs) reconciled  for the cluster, and the hashes calculated for the configuration for any
// external repositories defined for the cluster.
func getRepoVolumeStatus(repoStatus []v1beta1.RepoStatus, repoVolumes []*v1.PersistentVolumeClaim,
	configHashes map[string]string, replicaCreateRepoName string) []v1beta1.RepoStatus {

	// the new repository status that will be generated and returned
	updatedRepoStatus := []v1beta1.RepoStatus{}

	// Update the repo status based on the repo volumes (PVCs) that were reconciled.  This includes
	// updating the status for any existing repository volumes, and adding status for any new
	// repository volumes.
	for _, rv := range repoVolumes {
		newRepoVolStatus := true
		repoName := rv.Labels[naming.LabelPGBackRestRepo]
		for _, rs := range repoStatus {
			if rs.Name == repoName {
				newRepoVolStatus = false

				// if we find a status with ReplicaCreateBackupComplete set to "true" but the repo name
				// for that status does not match the current replica create repo name, then reset
				// ReplicaCreateBackupComplete and StanzaCreate back to false
				if rs.ReplicaCreateBackupComplete && (rs.Name != replicaCreateRepoName) {
					rs.ReplicaCreateBackupComplete = false
				}

				// update binding info if needed
				if rs.Bound != (rv.Status.Phase == v1.ClaimBound) {
					rs.Bound = (rv.Status.Phase == v1.ClaimBound)
				}

				updatedRepoStatus = append(updatedRepoStatus, rs)
				break
			}
		}
		if newRepoVolStatus {
			updatedRepoStatus = append(updatedRepoStatus, v1beta1.RepoStatus{
				Bound:      (rv.Status.Phase == v1.ClaimBound),
				Name:       repoName,
				VolumeName: rv.Spec.VolumeName,
			})
		}
	}

	// Update the repo status based on the configuration hashes for any external repositories
	// configured for the cluster (e.g. Azure, GCS or S3 repositories).  This includes
	// updating the status for any existing external repositories, and adding status for any new
	// external repositories.
	for repoName, hash := range configHashes {
		newExtRepoStatus := true
		for _, rs := range repoStatus {
			if rs.Name == repoName {
				newExtRepoStatus = false

				// if we find a status with ReplicaCreateBackupComplete set to "true" but the repo name
				// for that status does not match the current replica create repo name, then reset
				// ReplicaCreateBackupComplete back to false
				if rs.ReplicaCreateBackupComplete && (rs.Name != replicaCreateRepoName) {
					rs.ReplicaCreateBackupComplete = false
				}

				// Update the hash if needed. Setting StanzaCreated to "false" will force another
				// run of the  pgBackRest stanza-create command, while also setting
				// ReplicaCreateBackupComplete to false (this will result in a new replica creation
				// backup if this is the replica creation repo)
				if rs.RepoOptionsHash != hash {
					rs.RepoOptionsHash = hash
					rs.StanzaCreated = false
					rs.ReplicaCreateBackupComplete = false
				}

				updatedRepoStatus = append(updatedRepoStatus, rs)
				break
			}
		}
		if newExtRepoStatus {
			updatedRepoStatus = append(updatedRepoStatus, v1beta1.RepoStatus{
				Name:            repoName,
				RepoOptionsHash: hash,
			})
		}
	}

	// sort to ensure repo status always displays in a consistent order according to repo name
	sort.Slice(updatedRepoStatus, func(i, j int) bool {
		return updatedRepoStatus[i].Name < updatedRepoStatus[j].Name
	})

	return updatedRepoStatus
}

// reconcilePGBackRestCronJob creates a pgBackRest backup CronJob for each backup type defined
// for each repo
func (r *Reconciler) reconcilePGBackRestCronJob(
	ctx context.Context, cluster *v1beta1.PostgresCluster,
) bool {
	// requeue if there is an error during creation
	var requeue bool

	for _, repo := range cluster.Spec.Archive.PGBackRest.Repos {
		// if the repo level backup schedules block has not been created,
		// there are no schedules defined
		if repo.BackupSchedules != nil {
			// next if the repo level schedule is not nil, create the CronJob.
			if repo.BackupSchedules.Full != nil {
				if err := r.createCronJob(ctx, cluster, repo.Name, full,
					repo.BackupSchedules.Full); err != nil {
					requeue = true
				}
			}
			if repo.BackupSchedules.Differential != nil {
				if err := r.createCronJob(ctx, cluster, repo.Name, differential,
					repo.BackupSchedules.Differential); err != nil {
					requeue = true
				}
			}
			if repo.BackupSchedules.Incremental != nil {
				if err := r.createCronJob(ctx, cluster, repo.Name, incremental,
					repo.BackupSchedules.Incremental); err != nil {
					requeue = true
				}
			}
		}
	}
	return requeue
}

// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=create;patch

// createCronJob creates the CronJob for the given repo, pgBackRest backup type and schedule
func (r *Reconciler) createCronJob(
	ctx context.Context, cluster *v1beta1.PostgresCluster, repoName,
	backupType string, schedule *string,
) error {

	log := logging.FromContext(ctx).WithValues("reconcileResource", "repoCronJob")

	annotations := naming.Merge(
		cluster.Spec.Metadata.GetAnnotationsOrNil(),
		cluster.Spec.Archive.PGBackRest.Metadata.GetAnnotationsOrNil())
	labels := naming.Merge(
		cluster.Spec.Metadata.GetLabelsOrNil(),
		cluster.Spec.Archive.PGBackRest.Metadata.GetLabelsOrNil(),
		naming.PGBackRestCronJobLabels(cluster.Name, repoName, backupType),
	)
	meta := naming.PGBackRestCronJob(cluster, backupType, repoName)
	meta.Labels = labels
	meta.Annotations = annotations

	pgBackRestCronJob := &batchv1beta1.CronJob{
		ObjectMeta: meta,
		Spec: batchv1beta1.CronJobSpec{
			Schedule: *schedule,
			JobTemplate: batchv1beta1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: annotations,
					Labels:      labels,
				},
				Spec: batchv1.JobSpec{
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: annotations,
							Labels:      labels,
						},
						Spec: v1.PodSpec{
							RestartPolicy: "OnFailure",
							Containers: []v1.Container{
								{
									Name: "pgbackrest",
									// TODO(tjmoore4): This is likely the correct image to use, but the image
									// value in the spec is currently optional. Should the image be required,
									// or should this be referencing its own image spec value?
									Image: cluster.Spec.Archive.PGBackRest.Image,
									Args:  []string{"/bin/sh", "-c", "date; echo pgBackRest " + backupType + " backup scheduled..."},
								},
							},
						},
					},
				},
			},
		},
	}

	// set metadata
	pgBackRestCronJob.SetGroupVersionKind(batchv1beta1.SchemeGroupVersion.WithKind("CronJob"))
	err := errors.WithStack(r.setControllerReference(cluster, pgBackRestCronJob))

	if err == nil {
		err = r.apply(ctx, pgBackRestCronJob)
	}
	if err != nil {
		// record and log any errors resulting from trying to create the pgBackRest backup CronJob
		r.Recorder.Event(cluster, v1.EventTypeWarning, EventUnableToCreatePGBackRestCronJob,
			err.Error())
		log.Error(err, "error when attempting to create pgBackRest CronJob")
	}
	return err
}