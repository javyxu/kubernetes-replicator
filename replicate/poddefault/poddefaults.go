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
	"k8s.io/client-go/kubernetes"

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
func NewReplicator(client kubernetes.Interface, dynamicclient dynamic.Interface, resyncPeriod time.Duration, allowAll bool) common.Replicator {
	repl := Replicator{
		GenericReplicator: common.NewGenericReplicator(common.ReplicatorConfig{
			Kind:          "PodDefault",
			ObjType:       &poddefaultv1.PodDefault{},
			AllowAll:      allowAll,
			ResyncPeriod:  resyncPeriod,
			Client:        client,
			DynamicClient: dynamicclient,
			ListFunc: func(lo metav1.ListOptions) (runtime.Object, error) {
				// return dynamicclient.Resource(gvr).Namespace("").List(lo)
				res, err := dynamicclient.Resource(gvr).Namespace("").List(lo)
				var targetObject poddefaultv1.PodDefaultList
				runtime.DefaultUnstructuredConverter.FromUnstructured(res.UnstructuredContent(), &targetObject)
				return &targetObject, err
			},
			WatchFunc: func(lo metav1.ListOptions) (watch.Interface, error) {
				return dynamicclient.Resource(gvr).Namespace("").Watch(lo)
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
	// source := sourceObj.(*unstructured.Unstructured)
	// var srcpoddefault poddefaultv1.PodDefault
	// runtime.DefaultUnstructuredConverter.FromUnstructured(source.UnstructuredContent(), &srcpoddefault)
	srcpoddefault := sourceObj.(*poddefaultv1.PodDefault)
	target := targetObj.(*poddefaultv1.PodDefault)

	logger := log.
		WithField("kind", r.Kind).
		// WithField("source", common.MustGetKey(source)).
		WithField("source", common.MustGetKey(srcpoddefault)).
		WithField("target", common.MustGetKey(target))

	// make sure replication is allowed
	if ok, err := r.IsReplicationPermitted(&target.ObjectMeta, &srcpoddefault.ObjectMeta); !ok {
		// return errors.Wrapf(err, "replication of target %s is not permitted", common.MustGetKey(source))
		return errors.Wrapf(err, "replication of target %s is not permitted", common.MustGetKey(srcpoddefault))
	}

	targetVersion, ok := target.Annotations[common.ReplicatedFromVersionAnnotation]
	sourceVersion := srcpoddefault.ResourceVersion

	if ok && targetVersion == sourceVersion {
		logger.Debugf("target %s is already up-to-date", common.MustGetKey(target))
		return nil
	}

	targetCopy := target.DeepCopy()

	logger.Infof("updating target %s/%s", target.Namespace, target.Name)

	targetCopy.Annotations[common.ReplicatedAtAnnotation] = time.Now().Format(time.RFC3339)
	targetCopy.Annotations[common.ReplicatedFromVersionAnnotation] = srcpoddefault.ResourceVersion

	res, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(targetCopy)
	mdb := &unstructured.Unstructured{}
	mdb.SetUnstructuredContent(res)
	s, err := r.DynamicClient.Resource(gvr).Namespace(target.Namespace).Update(mdb, metav1.UpdateOptions{})
	if err != nil {
		err = errors.Wrapf(err, "Failed updating target %s/%s", target.Namespace, targetCopy.Name)
	} else if err = r.Store.Update(s); err != nil {
		err = errors.Wrapf(err, "Failed to update cache for %s/%s: %v", target.Namespace, targetCopy, err)
	}

	return err
}

// ReplicateObjectTo copies the whole object to target namespace
func (r *Replicator) ReplicateObjectTo(sourceObj interface{}, target *v1.Namespace) error {
	// source := sourceObj.(*unstructured.Unstructured)
	// var srcpoddefault poddefaultv1.PodDefault
	// runtime.DefaultUnstructuredConverter.FromUnstructured(source.UnstructuredContent(), &srcpoddefault)
	srcpoddefault := sourceObj.(*poddefaultv1.PodDefault)
	targetLocation := fmt.Sprintf("%s/%s", target.Name, srcpoddefault.Name)

	logger := log.
		WithField("kind", r.Kind).
		// WithField("source", common.MustGetKey(source)).
		WithField("source", common.MustGetKey(srcpoddefault)).
		WithField("target", targetLocation)

	targetResource, exists, err := r.Store.GetByKey(targetLocation)
	if err != nil {
		return errors.Wrapf(err, "Could not get %s from cache!", targetLocation)
	}
	logger.Infof("Checking if %s exists? %v", targetLocation, exists)

	var targetCopy *poddefaultv1.PodDefault
	if exists {
		// targetsource := targetResource.(*unstructured.Unstructured)
		// var targetObject poddefaultv1.PodDefault
		// runtime.DefaultUnstructuredConverter.FromUnstructured(targetsource.UnstructuredContent(), &targetObject)
		targetObject := targetResource.(*poddefaultv1.PodDefault)
		targetVersion, ok := targetObject.Annotations[common.ReplicatedFromVersionAnnotation]
		sourceVersion := srcpoddefault.ResourceVersion

		if ok && targetVersion == sourceVersion {
			// logger.Debugf("PodDefault %s is already up-to-date", common.MustGetKey(targetsource))
			logger.Debugf("PodDefault %s is already up-to-date", common.MustGetKey(targetObject))
			return nil
		}

		targetCopy = targetObject.DeepCopy()
	} else {
		targetCopy = new(poddefaultv1.PodDefault)
	}

	keepOwnerReferences, ok := srcpoddefault.Annotations[common.KeepOwnerReferences]
	if ok && keepOwnerReferences == "true" {
		targetCopy.OwnerReferences = srcpoddefault.OwnerReferences
	}

	if targetCopy.Annotations == nil {
		targetCopy.Annotations = make(map[string]string)
	}

	labelsCopy := make(map[string]string)

	stripLabels, ok := srcpoddefault.Annotations[common.StripLabels]
	if !ok && stripLabels != "true" {
		if srcpoddefault.Labels != nil {
			for key, value := range srcpoddefault.Labels {
				labelsCopy[key] = value
			}
		}
	}

	targetCopy.Kind = srcpoddefault.Kind
	targetCopy.Name = srcpoddefault.Name
	targetCopy.APIVersion = srcpoddefault.APIVersion
	targetCopy.Labels = labelsCopy
	targetCopy.Spec = srcpoddefault.Spec
	targetCopy.Annotations[common.ReplicatedAtAnnotation] = time.Now().Format(time.RFC3339)
	targetCopy.Annotations[common.ReplicatedFromVersionAnnotation] = srcpoddefault.ResourceVersion

	res, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(targetCopy)
	mdb := &unstructured.Unstructured{}
	mdb.SetUnstructuredContent(res)
	var obj interface{}
	if exists {
		logger.Debugf("Updating existing poddefault %s/%s", target.Name, targetCopy.Name)
		obj, err = r.DynamicClient.Resource(gvr).Namespace(target.Name).Update(mdb, metav1.UpdateOptions{})
	} else {
		logger.Debugf("Creating a new poddefault %s/%s", target.Name, targetCopy.Name)
		obj, err = r.DynamicClient.Resource(gvr).Namespace(target.Name).Create(mdb, metav1.CreateOptions{})
	}
	if err != nil {
		return errors.Wrapf(err, "Failed to update poddefault %s/%s", target.Name, targetCopy.Name)
	}

	if err := r.Store.Update(obj); err != nil {
		return errors.Wrapf(err, "Failed to update cache for %s/%s", target.Name, targetCopy)
	}

	return nil
}

func (r *Replicator) PatchDeleteDependent(sourceKey string, target interface{}) (interface{}, error) {

	// targetsource := target.(*unstructured.Unstructured)
	// var targetObject poddefaultv1.PodDefault
	// runtime.DefaultUnstructuredConverter.FromUnstructured(targetsource.UnstructuredContent(), &targetObject)

	targetObject := target.(*poddefaultv1.PodDefault)
	dependentKey := common.MustGetKey(targetObject)
	logger := log.WithFields(log.Fields{
		"kind":   r.Kind,
		"source": sourceKey,
		"target": dependentKey,
	})

	patch := []common.JSONPatchOperation{{Operation: "remove", Path: "/rules"}}
	patchBody, err := json.Marshal(&patch)

	if err != nil {
		return nil, errors.Wrapf(err, "error while building patch body for poddefault %s: %v", dependentKey, err)
	}

	logger.Debugf("clearing dependent poddefault %s", dependentKey)
	logger.Tracef("patch body: %s", string(patchBody))

	s, err := r.DynamicClient.Resource(gvr).Namespace(targetObject.Namespace).Patch(targetObject.Name, types.JSONPatchType, patchBody, metav1.UpdateOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "error while patching poddefault %s: %v", dependentKey, err)
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

	// targetsource := targetResource.(*unstructured.Unstructured)
	// var targetObject poddefaultv1.PodDefault
	// runtime.DefaultUnstructuredConverter.FromUnstructured(targetsource.UnstructuredContent(), &targetObject)
	targetObject := targetResource.(*poddefaultv1.PodDefault)
	logger.Debugf("Deleting %s", targetLocation)
	if err := r.DynamicClient.Resource(gvr).Namespace(targetObject.Namespace).Delete(targetObject.Name, &metav1.DeleteOptions{}); err != nil {
		return errors.Wrapf(err, "Failed deleting %s: %v", targetLocation, err)
	}
	return nil
}
