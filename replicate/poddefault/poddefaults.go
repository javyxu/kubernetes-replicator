package poddefault

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mittwald/kubernetes-replicator/replicate/common"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	poddefaultv1 "github.com/kubeflow/kubeflow/components/admission-webhook/pkg/apis/settings/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Replicator struct {
	*common.GenericReplicator
}

var gvr = schema.GroupVersionResource{
	Group:    "kubeflow.org",
	Version:  "v1alpha1",
	Resource: "poddefaults",
}

// NewReplicator creates a new poddefault replicator
func NewReplicator(client dynamic.Interface, resyncPeriod time.Duration, allowAll bool) common.Replicator {
	repl := Replicator{
		GenericReplicator: common.NewGenericReplicator(common.ReplicatorConfig{
			Kind:         "PodDefault",
			ObjType:      &poddefaultv1.PodDefault{},
			AllowAll:     allowAll,
			ResyncPeriod: resyncPeriod,
			Client:       client,
			ListFunc: func(lo metav1.ListOptions) (runtime.Object, error) {
				return client.Resource(gvr).Namespace("").List(metav1.ListOptions{})
			},
			WatchFunc: func(lo metav1.ListOptions) (watch.Interface, error) {
				return client.Resource(gvr).Namespace("").Watch(lo)
			},
		}),
	}
	repl.UpdateFuncs = common.UpdateFuncs{
		ReplicateDataFrom:        repl.ReplicateDataFrom,
		ReplicateObjectTo:        repl.ReplicateObjectTo,
		PatchDeleteDependent:     repl.PatchDeleteDependent,
		DeleteReplicatedResource: repl.DeleteReplicatedResource,
	}

	return &repl
}

func (r *Replicator) ReplicateDataFrom(sourceObj interface{}, targetObj interface{}) error {
	source := sourceObj.(*poddefaultv1.PodDefault)
	target := targetObj.(*poddefaultv1.PodDefault)

	logger := log.
		WithField("kind", r.Kind).
		WithField("source", common.MustGetKey(source)).
		WithField("target", common.MustGetKey(target))

	// make sure replication is allowed
	if ok, err := r.IsReplicationPermitted(&target.ObjectMeta, &source.ObjectMeta); !ok {
		return errors.Wrapf(err, "replication of target %s is not permitted", common.MustGetKey(source))
	}

	targetVersion, ok := target.Annotations[common.ReplicatedFromVersionAnnotation]
	sourceVersion := source.ResourceVersion

	if ok && targetVersion == sourceVersion {
		logger.Debugf("target %s is already up-to-date", common.MustGetKey(target))
		return nil
	}

	targetCopy := target.DeepCopy()

	logger.Infof("updating target %s/%s", target.Namespace, target.Name)

	targetCopy.Annotations[common.ReplicatedAtAnnotation] = time.Now().Format(time.RFC3339)
	targetCopy.Annotations[common.ReplicatedFromVersionAnnotation] = source.ResourceVersion

	res, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(targetCopy)
	mdb := &unstructured.Unstructured{}
	mdb.SetUnstructuredContent(res)
	s, err := r.Client.(dynamic.Interface).Resource(gvr).Namespace(target.Namespace).Update(mdb, metav1.UpdateOptions{})
	if err != nil {
		err = errors.Wrapf(err, "Failed updating target %s/%s", target.Namespace, targetCopy.Name)
	} else if err = r.Store.Update(s); err != nil {
		err = errors.Wrapf(err, "Failed to update cache for %s/%s: %v", target.Namespace, targetCopy, err)
	}

	return err
}

// ReplicateObjectTo copies the whole object to target namespace
func (r *Replicator) ReplicateObjectTo(sourceObj interface{}, target *v1.Namespace) error {
	source := sourceObj.(*poddefaultv1.PodDefault)
	targetLocation := fmt.Sprintf("%s/%s", target.Name, source.Name)

	logger := log.
		WithField("kind", r.Kind).
		WithField("source", common.MustGetKey(source)).
		WithField("target", targetLocation)

	targetResource, exists, err := r.Store.GetByKey(targetLocation)
	if err != nil {
		return errors.Wrapf(err, "Could not get %s from cache!", targetLocation)
	}
	logger.Infof("Checking if %s exists? %v", targetLocation, exists)

	var targetCopy *poddefaultv1.PodDefault
	if exists {
		targetObject := targetResource.(*poddefaultv1.PodDefault)
		targetVersion, ok := targetObject.Annotations[common.ReplicatedFromVersionAnnotation]
		sourceVersion := source.ResourceVersion

		if ok && targetVersion == sourceVersion {
			logger.Debugf("PodDefault %s is already up-to-date", common.MustGetKey(targetObject))
			return nil
		}

		targetCopy = targetObject.DeepCopy()
	} else {
		targetCopy = new(poddefaultv1.PodDefault)
	}

	keepOwnerReferences, ok := source.Annotations[common.KeepOwnerReferences]
	if ok && keepOwnerReferences == "true" {
		targetCopy.OwnerReferences = source.OwnerReferences
	}

	// if targetCopy.Rules == nil {
	// 	targetCopy.Rules = make([]rbacv1.PolicyRule, 0)
	// }
	if targetCopy.Annotations == nil {
		targetCopy.Annotations = make(map[string]string)
	}

	labelsCopy := make(map[string]string)

	stripLabels, ok := source.Annotations[common.StripLabels]
	if !ok && stripLabels != "true" {
		if source.Labels != nil {
			for key, value := range source.Labels {
				labelsCopy[key] = value
			}
		}
	}

	targetCopy.Name = source.Name
	targetCopy.Labels = labelsCopy
	targetCopy.Annotations[common.ReplicatedAtAnnotation] = time.Now().Format(time.RFC3339)
	targetCopy.Annotations[common.ReplicatedFromVersionAnnotation] = source.ResourceVersion

	res, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(targetCopy)
	mdb := &unstructured.Unstructured{}
	mdb.SetUnstructuredContent(res)
	var obj interface{}
	if exists {
		logger.Debugf("Updating existing role %s/%s", target.Name, targetCopy.Name)
		obj, err = r.Client.(dynamic.Interface).Resource(gvr).Namespace(target.Name).Update(mdb, metav1.UpdateOptions{})
	} else {
		logger.Debugf("Creating a new role %s/%s", target.Name, targetCopy.Name)
		obj, err = r.Client.(dynamic.Interface).Resource(gvr).Namespace(target.Name).Create(mdb, metav1.CreateOptions{})
	}
	if err != nil {
		return errors.Wrapf(err, "Failed to update role %s/%s", target.Name, targetCopy.Name)
	}

	if err := r.Store.Update(obj); err != nil {
		return errors.Wrapf(err, "Failed to update cache for %s/%s", target.Name, targetCopy)
	}

	return nil
}

func (r *Replicator) PatchDeleteDependent(sourceKey string, target interface{}) (interface{}, error) {

	dependentKey := common.MustGetKey(target)
	logger := log.WithFields(log.Fields{
		"kind":   r.Kind,
		"source": sourceKey,
		"target": dependentKey,
	})

	targetObject, ok := target.(*poddefaultv1.PodDefault)
	if !ok {
		err := errors.Errorf("bad type returned from Store: %T", target)
		return nil, err
	}

	patch := []common.JSONPatchOperation{{Operation: "remove", Path: "/rules"}}
	patchBody, err := json.Marshal(&patch)

	if err != nil {
		return nil, errors.Wrapf(err, "error while building patch body for role %s: %v", dependentKey, err)
	}

	logger.Debugf("clearing dependent role %s", dependentKey)
	logger.Tracef("patch body: %s", string(patchBody))

	s, err := r.Client.(dynamic.Interface).Resource(gvr).Namespace(targetObject.Namespace).Patch(targetObject.Name, types.JSONPatchType, patchBody, metav1.UpdateOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "error while patching role %s: %v", dependentKey, err)
	}
	return s, nil
}

// DeleteReplicatedResource deletes a resource replicated by ReplicateTo annotation
func (r *Replicator) DeleteReplicatedResource(targetResource interface{}) error {
	targetLocation := common.MustGetKey(targetResource)
	logger := log.WithFields(log.Fields{
		"kind":   r.Kind,
		"target": targetLocation,
	})

	object := targetResource.(*poddefaultv1.PodDefault)
	logger.Debugf("Deleting %s", targetLocation)
	if err := r.Client.(dynamic.Interface).Resource(gvr).Namespace(object.Namespace).Delete(object.Name, &metav1.DeleteOptions{}); err != nil {
		return errors.Wrapf(err, "Failed deleting %s: %v", targetLocation, err)
	}
	return nil
}
