/*
Copyright 2025.

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
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appv1alpha1 "github.com/piny940/kube-cronjob/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ref "k8s.io/client-go/tools/reference"
)

var (
	scheduledTimeAnnotation = "app.piny940.com/scheduled-at"
)

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock
}

// +kubebuilder:rbac:groups=app.piny940.com,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=app.piny940.com,resources=cronjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=app.piny940.com,resources=cronjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CronJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var cronjob appv1alpha1.CronJob
	if err := r.Client.Get(ctx, req.NamespacedName, &cronjob); err != nil {
		log.Error(err, "failed to get cronjob")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var jobs batchv1.JobList
	if err := r.Client.List(ctx, &jobs, client.InNamespace(req.Namespace), client.MatchingFields{
		"jobOwnerKey": req.Name,
	}); err != nil {
		log.Error(err, "failed to list child jobs", "cronjob", req.Name)
		return ctrl.Result{}, err
	}

	var activeJobs []*batchv1.Job
	var successfulJobs []*batchv1.Job
	var failedJobs []*batchv1.Job
	var mostRecentTime *time.Time

	// ----- update cronjob status -----
	isJobFinished := func(job *batchv1.Job) (bool, batchv1.JobConditionType) {
		for _, t := range job.Status.Conditions {
			if t.Type == batchv1.JobComplete || t.Type == batchv1.JobFailed {
				return true, t.Type
			}
		}
		return false, ""
	}
	getScheduledAt := func(job *batchv1.Job) (*time.Time, error) {
		rawTime, ok := job.Annotations[scheduledTimeAnnotation]
		if !ok {
			return nil, nil
		}
		timeParsed, err := time.Parse(time.RFC3339, rawTime)
		if err != nil {
			return nil, err
		}
		return &timeParsed, nil
	}
	for _, job := range jobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case batchv1.JobComplete:
			successfulJobs = append(successfulJobs, &job)
		case batchv1.JobFailed:
			failedJobs = append(failedJobs, &job)
		default:
			activeJobs = append(activeJobs, &job)
		}
		scheduledAt, err := getScheduledAt(&job)
		if err != nil {
			log.Error(err, "failed to get scheduled time for child job", "job", &job)
			continue
		}
		if scheduledAt != nil {
			if mostRecentTime == nil || mostRecentTime.Before(*scheduledAt) {
				mostRecentTime = scheduledAt
			}
		}
	}
	if mostRecentTime != nil {
		cronjob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		cronjob.Status.LastScheduleTime = nil
	}
	cronjob.Status.Active = nil
	for _, activejob := range activeJobs {
		jobRef, err := ref.GetReference(r.Scheme, activejob)
		if err != nil {
			log.Error(err, "failed to get reference of child object", "job", activejob)
			continue
		}
		cronjob.Status.Active = append(cronjob.Status.Active, *jobRef)
	}
	log.V(1).Info("job count", "active jobs", len(activeJobs), "successful jobs", len(successfulJobs), "failed jobs", len(failedJobs))
	if err := r.Status().Update(ctx, &cronjob); err != nil {
		log.Error(err, "failed to update cronjob status")
		return ctrl.Result{}, err
	}

	// ----- update child jobs -----
	if cronjob.Spec.SuccessfulJobsHistoryLimit != nil {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i].Status.StartTime == nil {
				return successfulJobs[j].Status.StartTime != nil
			}
			return successfulJobs[i].Status.StartTime.Before(successfulJobs[j].Status.StartTime)
		})
		for i := 0; i < len(successfulJobs)-int(*cronjob.Spec.SuccessfulJobsHistoryLimit); i++ {
			job := successfulJobs[i]
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				log.Error(err, "failed to delete job", "job", job)
			} else {
				log.V(0).Info("deleted old successful job", "job", job)
			}
		}
	}
	if cronjob.Spec.FailedJobsHistoryLimit != nil {
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.StartTime == nil {
				return failedJobs[j].Status.StartTime != nil
			}
			return failedJobs[i].Status.StartTime.Before(failedJobs[j].Status.StartTime)
		})
		for i := 0; i < len(failedJobs)-int(*cronjob.Spec.FailedJobsHistoryLimit); i++ {
			job := failedJobs[i]
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				log.Error(err, "failed to delete job", "job", job)
			} else {
				log.V(0).Info("deleted old failed job", "job", job)
			}
		}
	}

	if cronjob.Spec.Suspend != nil && *cronjob.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1alpha1.CronJob{}).
		Complete(r)
}
