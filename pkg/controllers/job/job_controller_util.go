/*
Copyright 2017 The Volcano Authors.

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

package job

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kbapi "volcano.sh/volcano/pkg/apis/scheduling/v1alpha1"

	"volcano.sh/volcano/pkg/apis/batch/v1alpha1"
	vkv1 "volcano.sh/volcano/pkg/apis/batch/v1alpha1"
	"volcano.sh/volcano/pkg/apis/helpers"
	"volcano.sh/volcano/pkg/controllers/apis"
	vkjobhelpers "volcano.sh/volcano/pkg/controllers/job/helpers"
)

//MakePodName append podname,jobname,taskName and index and returns the string
func MakePodName(jobName string, taskName string, index int) string {
	return fmt.Sprintf(vkjobhelpers.PodNameFmt, jobName, taskName, index)
}

func createJobPod(job *vkv1.Job, template *v1.PodTemplateSpec, ix int) *v1.Pod {
	templateCopy := template.DeepCopy()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vkjobhelpers.MakePodName(job.Name, template.Name, ix),
			Namespace: job.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(job, helpers.JobKind),
			},
			Labels:      templateCopy.Labels,
			Annotations: templateCopy.Annotations,
		},
		Spec: templateCopy.Spec,
	}

	// If no scheduler name in Pod, use scheduler name from Job.
	if len(pod.Spec.SchedulerName) == 0 {
		pod.Spec.SchedulerName = job.Spec.SchedulerName
	}

	volumeMap := make(map[string]bool)
	for _, volume := range job.Spec.Volumes {
		vcName := volume.VolumeClaimName
		if _, ok := volumeMap[vcName]; !ok {
			if _, ok := job.Status.ControlledResources["volume-emptyDir-"+vcName]; ok && volume.VolumeClaim == nil {
				volume := v1.Volume{
					Name: vcName,
				}
				volume.EmptyDir = &v1.EmptyDirVolumeSource{}
				pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
			} else {
				volume := v1.Volume{
					Name: vcName,
				}
				volume.PersistentVolumeClaim = &v1.PersistentVolumeClaimVolumeSource{
					ClaimName: vcName,
				}
				pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
			}
			volumeMap[vcName] = true
		}

		for i, c := range pod.Spec.Containers {
			vm := v1.VolumeMount{
				MountPath: volume.MountPath,
				Name:      vcName,
			}
			pod.Spec.Containers[i].VolumeMounts = append(c.VolumeMounts, vm)
		}
	}

	if len(pod.Annotations) == 0 {
		pod.Annotations = make(map[string]string)
	}

	tsKey := templateCopy.Name
	if len(tsKey) == 0 {
		tsKey = vkv1.DefaultTaskSpec
	}

	if len(pod.Annotations) == 0 {
		pod.Annotations = make(map[string]string)
	}

	pod.Annotations[vkv1.TaskSpecKey] = tsKey
	pod.Annotations[kbapi.GroupNameAnnotationKey] = job.Name
	pod.Annotations[vkv1.JobNameKey] = job.Name
	pod.Annotations[vkv1.JobVersion] = fmt.Sprintf("%d", job.Status.Version)

	if len(pod.Labels) == 0 {
		pod.Labels = make(map[string]string)
	}

	// Set pod labels for Service.
	pod.Labels[vkv1.JobNameKey] = job.Name
	pod.Labels[vkv1.JobNamespaceKey] = job.Namespace

	// we fill the schedulerName in the pod definition with the one specified in the QJ template
	if job.Spec.SchedulerName != "" && pod.Spec.SchedulerName == "" {
		pod.Spec.SchedulerName = job.Spec.SchedulerName
	}

	return pod
}

func applyPolicies(job *vkv1.Job, req *apis.Request) vkv1.Action {
	if len(req.Action) != 0 {
		return req.Action
	}

	if req.Event == vkv1.OutOfSyncEvent {
		return vkv1.SyncJobAction
	}

	// For all the requests triggered from discarded job resources will perform sync action instead
	if req.JobVersion < job.Status.Version {
		glog.Infof("Request %s is outdated, will perform sync instead.", req)
		return vkv1.SyncJobAction
	}

	// Overwrite Job level policies
	if len(req.TaskName) != 0 {
		// Parse task level policies
		for _, task := range job.Spec.Tasks {
			if task.Name == req.TaskName {
				for _, policy := range task.Policies {
					policyEvents := getEventlist(policy)

					if len(policyEvents) > 0 && len(req.Event) > 0 {
						if checkEventExist(policyEvents, req.Event) || checkEventExist(policyEvents, vkv1.AnyEvent) {
							return policy.Action
						}
					}

					// 0 is not an error code, is prevented in validation admission controller
					if policy.ExitCode != nil && *policy.ExitCode == req.ExitCode {
						return policy.Action
					}
				}
				break
			}
		}
	}

	// Parse Job level policies
	for _, policy := range job.Spec.Policies {
		policyEvents := getEventlist(policy)

		if len(policyEvents) > 0 && len(req.Event) > 0 {
			if checkEventExist(policyEvents, req.Event) || checkEventExist(policyEvents, vkv1.AnyEvent) {
				return policy.Action
			}
		}

		// 0 is not an error code, is prevented in validation admission controller
		if policy.ExitCode != nil && *policy.ExitCode == req.ExitCode {
			return policy.Action
		}
	}

	return vkv1.SyncJobAction
}

func getEventlist(policy v1alpha1.LifecyclePolicy) []v1alpha1.Event {
	policyEventsList := policy.Events
	if len(policy.Event) > 0 {
		policyEventsList = append(policyEventsList, policy.Event)
	}
	return policyEventsList
}

func checkEventExist(policyEvents []v1alpha1.Event, reqEvent v1alpha1.Event) bool {
	for _, event := range policyEvents {
		if event == reqEvent {
			return true
		}
	}
	return false

}

func addResourceList(list, req, limit v1.ResourceList) {
	for name, quantity := range req {

		if value, ok := list[name]; !ok {
			list[name] = *quantity.Copy()
		} else {
			value.Add(quantity)
			list[name] = value
		}
	}

	// If Requests is omitted for a container,
	// it defaults to Limits if that is explicitly specified.
	for name, quantity := range limit {
		if _, ok := list[name]; !ok {
			list[name] = *quantity.Copy()
		}
	}
}

//TaskPriority structure
type TaskPriority struct {
	priority int32

	vkv1.TaskSpec
}

//TasksPriority is a slice of TaskPriority
type TasksPriority []TaskPriority

func (p TasksPriority) Len() int { return len(p) }

func (p TasksPriority) Less(i, j int) bool {
	return p[i].priority > p[j].priority
}

func (p TasksPriority) Swap(i, j int) { p[i], p[j] = p[j], p[i] }

func isControlledBy(obj metav1.Object, gvk schema.GroupVersionKind) bool {
	controlerRef := metav1.GetControllerOf(obj)
	if controlerRef == nil {
		return false
	}
	if controlerRef.APIVersion == gvk.GroupVersion().String() && controlerRef.Kind == gvk.Kind {
		return true
	}
	return false
}

func UpdateJobPhase(status *vkv1.JobStatus, newphase vkv1.JobPhase, message string) {
	switch newphase {
	case vkv1.Pending, vkv1.Inqueue:
		SetCondition(status, NewStateCondition(vkv1.JobCreated, "Job created", message))
		break
	case vkv1.Running:
		SetCondition(status, NewStateCondition(vkv1.JobScheduled, "Job successfully scheduled", message))
		break
	case vkv1.Completed:
		SetCondition(status, NewStateCondition(vkv1.JobSucceed, "Job completed", message))
		break
	case vkv1.Failed, vkv1.Terminated, vkv1.Aborted:
		SetCondition(status, NewStateCondition(vkv1.JobStopped, "Job stopped", message))
		break
	case vkv1.Restarting:
		SetCondition(status, NewStateCondition(vkv1.JobRestarting, "Job is restarting", message))
		break
	}
	status.Phase = newphase
	now := metav1.Time{Time: time.Now()}
	//Remove Completion time
	if !status.CompletionTime.IsZero() && HasCondition(*status, vkv1.JobRestarting) {
		status.CompletionTime = nil
	}
	//Update the timestamp
	if status.StartTime.IsZero() && HasCondition(*status, vkv1.JobCreated) {
		status.StartTime = &now
	}
	if status.CompletionTime.IsZero() &&
		(HasCondition(*status, vkv1.JobSucceed) || HasCondition(*status, vkv1.JobSucceed)) {
		status.CompletionTime = &now
	}
}

// NewStateCondition creates a new job condition.
func NewStateCondition(conditionType vkv1.JobConditionType, reason, message string) vkv1.JobCondition {
	return vkv1.JobCondition{
		Type:               conditionType,
		Status:             v1.ConditionTrue,
		LastUpdateTime:     metav1.Now(),
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

func HasCondition(status vkv1.JobStatus, condType vkv1.JobConditionType) bool {
	for _, condition := range status.Conditions {
		if condition.Type == condType && condition.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func GetCondition(status vkv1.JobStatus, condType vkv1.JobConditionType) *vkv1.JobCondition {
	for _, condition := range status.Conditions {
		if condition.Type == condType {
			return &condition
		}
	}
	return nil
}

func SetCondition(status *vkv1.JobStatus, condition vkv1.JobCondition) {

	currentCond := GetCondition(*status, condition.Type)

	// Do nothing if condition doesn't change
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason {
		return
	}

	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}

	// Append the updated condition to the job status
	newConditions := filterOutCondition(status, condition)
	status.Conditions = append(newConditions, condition)
}

func filterOutCondition(states *vkv1.JobStatus, currentCondition vkv1.JobCondition) []vkv1.JobCondition {

	var newConditions []vkv1.JobCondition
	for _, condition := range states.Conditions {
		//Filter out the same condition
		if condition.Type == currentCondition.Type {
			continue
		}

		//Remove scheduled/succeed/stopped if current condition is restarting
		if (currentCondition.Type == vkv1.JobRestarting &&
			currentCondition.Status == v1.ConditionTrue) && (condition.Type == vkv1.JobScheduled ||
			condition.Type == vkv1.JobSucceed ||
			condition.Type == vkv1.JobStopped) {
			continue
		}

		//Update restarting condition if status transits into others
		if condition.Type == vkv1.JobRestarting &&
			condition.Status == v1.ConditionTrue &&
			currentCondition.Type != condition.Type {
			condition.Status = v1.ConditionFalse
			condition.LastUpdateTime = metav1.Now()
			condition.Message = ""
			condition.Reason = fmt.Sprintf("Job finished restarting.")
		}
		newConditions = append(newConditions, condition)
	}
	return newConditions
}
