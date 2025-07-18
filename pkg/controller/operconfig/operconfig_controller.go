package operconfig

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	"github.com/openshift/cluster-network-operator/pkg/hypershift"
	"github.com/pkg/errors"

	configv1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	operv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-network-operator/pkg/apply"
	cnoclient "github.com/openshift/cluster-network-operator/pkg/client"
	"github.com/openshift/cluster-network-operator/pkg/controller/statusmanager"
	"github.com/openshift/cluster-network-operator/pkg/names"
	"github.com/openshift/cluster-network-operator/pkg/network"
	"github.com/openshift/cluster-network-operator/pkg/platform"
	"github.com/openshift/cluster-network-operator/pkg/util"
	ipsecMetrics "github.com/openshift/cluster-network-operator/pkg/util/ipsec"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	v1coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// The periodic resync interval.
// We will re-run the reconciliation logic, even if the network configuration
// hasn't changed.
var ResyncPeriod = 3 * time.Minute

// ManifestPaths is the path to the manifest templates
// bad, but there's no way to pass configuration to the reconciler right now
var ManifestPath = "./bindata"

// Add creates a new OperConfig Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, status *statusmanager.StatusManager, c cnoclient.Client, featureGates featuregates.FeatureGate) error {
	rc, err := newReconciler(mgr, status, c, featureGates)
	if err != nil {
		return err
	}
	return add(mgr, rc)
}

const ControllerName = "operconfig"

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, status *statusmanager.StatusManager, c cnoclient.Client, featureGates featuregates.FeatureGate) (*ReconcileOperConfig, error) {
	return &ReconcileOperConfig{
		client:       c,
		status:       status,
		mapper:       mgr.GetRESTMapper(),
		featureGates: featureGates,
	}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r *ReconcileOperConfig) error {
	// Create a new controller
	c, err := controller.New("operconfig-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to networkDiagnostics in network.config
	err = c.Watch(source.Kind[crclient.Object](mgr.GetCache(), &configv1.Network{}, &handler.EnqueueRequestForObject{}, predicate.Funcs{
		UpdateFunc: func(evt event.UpdateEvent) bool {
			old, ok := evt.ObjectOld.(*configv1.Network)
			if !ok {
				return true
			}
			new, ok := evt.ObjectNew.(*configv1.Network)
			if !ok {
				return true
			}
			if reflect.DeepEqual(old.Spec.NetworkDiagnostics, new.Spec.NetworkDiagnostics) {
				return false
			}
			return true
		},
	}))
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Network (as long as the spec changes)
	err = c.Watch(source.Kind[crclient.Object](mgr.GetCache(), &operv1.Network{}, &handler.EnqueueRequestForObject{}, predicate.Funcs{
		UpdateFunc: func(evt event.UpdateEvent) bool {
			old, ok := evt.ObjectOld.(*operv1.Network)
			if !ok {
				return true
			}
			new, ok := evt.ObjectNew.(*operv1.Network)
			if !ok {
				return true
			}
			if reflect.DeepEqual(old.Spec, new.Spec) {
				log.Printf("Skipping reconcile of Network.operator.openshift.io: spec unchanged")
				return false
			}
			return true
		},
	}))
	if err != nil {
		return err
	}

	// watch for changes in all configmaps in our namespace
	// Currently, this would catch the mtu-prober reporting or the ovs flows config map.
	// Need to do this with a custom namespaced informer.
	cmInformer := v1coreinformers.NewConfigMapInformer(
		r.client.Default().Kubernetes(),
		names.APPLIED_NAMESPACE,
		0, // don't resync
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	r.client.Default().AddCustomInformer(cmInformer) // Tell the ClusterClient about this informer

	if err := c.Watch(&source.Informer{
		Informer: cmInformer,
		Handler:  handler.EnqueueRequestsFromMapFunc(reconcileOperConfig),
		Predicates: []predicate.TypedPredicate[crclient.Object]{
			predicate.ResourceVersionChangedPredicate{},
			predicate.NewPredicateFuncs(func(object crclient.Object) bool {
				// Ignore ConfigMaps we manage as part of this loop
				return !(object.GetName() == "network-operator-lock" ||
					object.GetName() == "applied-cluster")
			}),
		},
	}); err != nil {
		return err
	}

	// Watch when nodes are created and updated.
	// We need to watch when nodes are updated since we are interested in the labels
	// of nodes for hardware offloading.
	nodePredicate := predicate.Funcs{
		CreateFunc: func(_ event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(ev event.UpdateEvent) bool {
			// Node conditions change *a lot* and we don't care. We only care
			// about updates when the labels change.
			return !reflect.DeepEqual(
				ev.ObjectOld.GetLabels(),
				ev.ObjectNew.GetLabels(),
			)
		},
		DeleteFunc: func(_ event.DeleteEvent) bool {
			return true
		},
	}
	if err := c.Watch(source.Kind[crclient.Object](mgr.GetCache(), &corev1.Node{}, handler.EnqueueRequestsFromMapFunc(reconcileOperConfig), nodePredicate)); err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileOperConfig{}

// ReconcileOperConfig reconciles a Network.operator.openshift.io object
type ReconcileOperConfig struct {
	client cnoclient.Client
	status *statusmanager.StatusManager
	mapper meta.RESTMapper

	// If we can skip cleaning up the MTU prober job.
	mtuProberCleanedUp bool
	// maintain the copy of feature gates in the cluster
	featureGates featuregates.FeatureGate
}

// Reconcile updates the state of the cluster to match that which is desired
// in the operator configuration (Network.operator.openshift.io)
func (r *ReconcileOperConfig) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	defer utilruntime.HandleCrash(r.status.SetDegradedOnPanicAndCrash)

	log.Printf("Reconciling Network.operator.openshift.io %s\n", request.Name)

	// We won't create more than one network
	if request.Name != names.OPERATOR_CONFIG {
		log.Printf("Ignoring Network.operator.openshift.io without default name")
		return reconcile.Result{}, nil
	}

	// Fetch the Network.operator.openshift.io instance
	operConfig := &operv1.Network{TypeMeta: metav1.TypeMeta{APIVersion: operv1.GroupVersion.String(), Kind: "Network"}}
	err := r.client.Default().CRClient().Get(ctx, request.NamespacedName, operConfig)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.status.SetDegraded(statusmanager.OperatorConfig, "NoOperatorConfig",
				fmt.Sprintf("Operator configuration %s was deleted", request.NamespacedName.String()))
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected, since we set
			// the ownerReference (see https://kubernetes.io/docs/concepts/workloads/controllers/garbage-collection/).
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Printf("Unable to retrieve Network.operator.openshift.io object: %v", err)
		// FIXME: operator status?
		return reconcile.Result{}, err
	}

	if operConfig.Spec.ManagementState == operv1.Unmanaged {
		log.Printf("Operator configuration state is %s - skipping operconfig reconciliation", operConfig.Spec.ManagementState)
		return reconcile.Result{}, nil
	}

	// Fetch the Network.config.openshift.io instance
	clusterConfig := &configv1.Network{}
	err = r.client.Default().CRClient().Get(ctx, types.NamespacedName{Name: names.CLUSTER_CONFIG}, clusterConfig)
	if err != nil {
		log.Printf("Unable to retrieve network.config.openshift.io object: %v", err)
		return reconcile.Result{}, err
	}
	// Merge in the cluster configuration, in case the administrator has updated some "downstream" fields
	// This will also commit the change back to the apiserver.
	if err := r.MergeClusterConfig(ctx, operConfig, clusterConfig); err != nil {
		log.Printf("Failed to merge the cluster configuration: %v", err)
		// not set degraded if the err is a version conflict, but return a reconcile err for retry.
		if !apierrors.IsConflict(err) {
			r.status.SetDegraded(statusmanager.OperatorConfig, "MergeClusterConfig",
				fmt.Sprintf("Internal error while merging cluster configuration and operator configuration: %v", err))
		}
		return reconcile.Result{}, err
	}

	// Convert certain fields to canonicalized form for backward compatibility
	network.DeprecatedCanonicalize(&operConfig.Spec)

	// Validate the configuration
	if err := network.Validate(&operConfig.Spec); err != nil {
		log.Printf("Failed to validate Network.operator.openshift.io.Spec: %v", err)
		r.status.SetDegraded(statusmanager.OperatorConfig, "InvalidOperatorConfig",
			fmt.Sprintf("The operator configuration is invalid (%v). Use 'oc edit network.operator.openshift.io cluster' to fix.", err))
		return reconcile.Result{}, err
	}

	// Retrieve the previously applied operator configuration
	prev, err := GetAppliedConfiguration(ctx, r.client.Default().CRClient(), operConfig.ObjectMeta.Name)
	if err != nil {
		log.Printf("Failed to retrieve previously applied configuration: %v", err)
		// FIXME: operator status?
		return reconcile.Result{}, err
	}

	// Gather the Infra status, we'll need it a few places
	infraStatus, err := platform.InfraStatus(r.client)
	if err != nil {
		log.Printf("Failed to retrieve infrastructure status: %v", err)
		return reconcile.Result{}, err
	}

	// If we need to, probe the host's MTU via a Job.
	// Note that running clusters have no need of this but we want the configmap
	// mtu to be created for consistancy with other non-hypershift clusters.
	// A hypershift cluster may not have any worker nodes for running the mtu prober.
	mtu := 0
	err = r.client.Default().CRClient().Get(ctx, types.NamespacedName{Namespace: util.MTU_CM_NAMESPACE, Name: util.MTU_CM_NAME}, &corev1.ConfigMap{})
	if network.NeedMTUProbe(prev, &operConfig.Spec) || (apierrors.IsNotFound(err) && infraStatus.HostedControlPlane == nil) {
		mtu, err = r.probeMTU(ctx, operConfig, infraStatus)
		if err != nil {
			log.Printf("Failed to probe MTU: %v", err)
			r.status.SetDegraded(statusmanager.OperatorConfig, "MTUProbeFailed",
				fmt.Sprintf("Failed to probe MTU: %v", err))
			return reconcile.Result{}, fmt.Errorf("could not probe MTU -- maybe no available nodes: %w", err)
		}
		log.Printf("Using detected MTU %d", mtu)
	}

	// up-convert Prev by filling defaults
	if prev != nil {
		network.FillDefaults(prev, prev, mtu)
	}
	// Reserve operConfig for the DeepEqual check before UpdateOperConfig
	newOperConfig := operConfig.DeepCopy()
	// Fill all defaults explicitly
	network.FillDefaults(&newOperConfig.Spec, prev, mtu)

	// Compare against previous applied configuration to see if this change
	// is safe.
	if prev != nil {
		// We may need to fill defaults here -- sort of as a poor-man's
		// upconversion scheme -- if we add additional fields to the config.
		err = network.IsChangeSafe(prev, &newOperConfig.Spec, infraStatus)
		if err != nil {
			log.Printf("Not applying unsafe change: %v", err)
			r.status.SetDegraded(statusmanager.OperatorConfig, "InvalidOperatorConfig",
				fmt.Sprintf("Not applying unsafe configuration change: %v. Use 'oc edit network.operator.openshift.io cluster' to undo the change.", err))
			return reconcile.Result{}, err
		}
	}

	// Bootstrap any resources
	bootstrapResult, err := network.Bootstrap(newOperConfig, r.client)
	if err != nil {
		log.Printf("Failed to reconcile platform networking resources: %v", err)
		r.status.SetDegraded(statusmanager.OperatorConfig, "BootstrapError",
			fmt.Sprintf("Internal error while reconciling platform networking resources: %v", err))
		return reconcile.Result{}, err
	}

	if !reflect.DeepEqual(operConfig, newOperConfig) {
		if err := r.UpdateOperConfig(ctx, newOperConfig); err != nil {
			log.Printf("Failed to update the operator configuration: %v", err)
			// not set degraded if the err is a version conflict, but return a reconcile err for retry.
			if !apierrors.IsConflict(err) {
				r.status.SetDegraded(statusmanager.OperatorConfig, "UpdateOperatorConfig",
					fmt.Sprintf("Internal error while updating operator configuration: %v", err))
			}
			return reconcile.Result{}, err
		}
	}

	updateIPsecMetric(&newOperConfig.Spec)
	// once updated, use the new config
	operConfig = newOperConfig

	// Generate the objects.
	// Note that Render might have side effects in the passed in operConfig that
	// will be reflected later on in the updated status.
	objs, progressing, err := network.Render(&operConfig.Spec, &clusterConfig.Spec, ManifestPath, r.client, r.featureGates, bootstrapResult)
	if err != nil {
		log.Printf("Failed to render: %v", err)
		r.status.SetDegraded(statusmanager.OperatorConfig, "RenderError",
			fmt.Sprintf("Internal error while rendering operator configuration: %v", err))
		return reconcile.Result{}, err
	}

	if progressing {
		r.status.SetProgressing(statusmanager.OperatorRender, "RenderProgressing",
			"Waiting to render manifests")
	} else {
		r.status.UnsetProgressing(statusmanager.OperatorRender)
	}

	// The first object we create should be the record of our applied configuration. The last object we create is config.openshift.io/v1/Network.Status
	app, err := AppliedConfiguration(operConfig)
	if err != nil {
		log.Printf("Failed to render applied: %v", err)
		r.status.SetDegraded(statusmanager.OperatorConfig, "RenderError",
			fmt.Sprintf("Internal error while recording new operator configuration: %v", err))
		return reconcile.Result{}, err
	}
	objs = append([]*uns.Unstructured{app}, objs...)

	relatedObjects := []configv1.ObjectReference{}
	relatedClusterObjects := []hypershift.RelatedObject{}
	renderedMachineConfigs := []mcfgv1.MachineConfig{}
	hcpCfg := hypershift.NewHyperShiftConfig()
	for _, obj := range objs {
		// Label all DaemonSets, Deployments, and StatefulSets with the label that generates Status.
		if obj.GetAPIVersion() == "apps/v1" && (obj.GetKind() == "DaemonSet" || obj.GetKind() == "Deployment" || obj.GetKind() == "StatefulSet") {
			l := obj.GetLabels()
			if l == nil {
				l = map[string]string{}
			}

			// Resources with GenerateStatusLabel set to "" are not meant to generate status
			if v, exists := l[names.GenerateStatusLabel]; !exists || v != "" {
				// In HyperShift use the infrastructure name to differentiate between resources deployed by the management cluster CNO and CNO deployed in the hosted clusters control plane namespace
				// Without that the CNO running against the management cluster would pick the resources rendered by the hosted cluster CNO
				if hcpCfg.Enabled {
					l[names.GenerateStatusLabel] = bootstrapResult.Infra.InfraName
				} else {
					l[names.GenerateStatusLabel] = names.StandAloneClusterName
				}
				obj.SetLabels(l)
			}
		}
		restMapping, err := r.mapper.RESTMapping(obj.GroupVersionKind().GroupKind())
		if err != nil {
			log.Printf("Failed to get REST mapping for storing related object: %v", err)
			continue
		}
		if apply.GetClusterName(obj) != "" {
			relatedClusterObjects = append(relatedClusterObjects, hypershift.RelatedObject{
				ObjectReference: configv1.ObjectReference{
					Group:     obj.GetObjectKind().GroupVersionKind().Group,
					Resource:  restMapping.Resource.Resource,
					Name:      obj.GetName(),
					Namespace: obj.GetNamespace(),
				},
				ClusterName: apply.GetClusterName(obj),
			})
			// Don't add management cluster objects in relatedObjects
			continue
		}
		relatedObjects = append(relatedObjects, configv1.ObjectReference{
			Group:     obj.GetObjectKind().GroupVersionKind().Group,
			Resource:  restMapping.Resource.Resource,
			Name:      obj.GetName(),
			Namespace: obj.GetNamespace(),
		})

		if obj.GetAPIVersion() == "machineconfiguration.openshift.io/v1" && obj.GetKind() == "MachineConfig" {
			mc := mcfgv1.MachineConfig{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &mc)
			if err != nil {
				log.Printf("Unable to retrieve MachineConfig for rendered object: %v", err)
				continue
			}
			renderedMachineConfigs = append(renderedMachineConfigs, mc)
		}
	}

	relatedObjects = append(relatedObjects, configv1.ObjectReference{
		Resource: "namespaces",
		Name:     names.APPLIED_NAMESPACE,
	})

	// Add operator.openshift.io/v1/network to relatedObjects for must-gather
	relatedObjects = append(relatedObjects, configv1.ObjectReference{
		Group:    "operator.openshift.io",
		Resource: "networks",
		Name:     "cluster",
	})

	// This Namespace is rendered by the CVO, but it's really our operand.
	relatedObjects = append(relatedObjects, configv1.ObjectReference{
		Resource: "namespaces",
		Name:     "openshift-cloud-network-config-controller",
	})

	r.status.SetRelatedObjects(relatedObjects)
	r.status.SetRelatedClusterObjects(relatedClusterObjects)
	err = r.status.SetMachineConfigs(ctx, renderedMachineConfigs)
	if err != nil {
		log.Printf("Failed to process machine configs: %v", err)
		r.status.SetDegraded(statusmanager.OperatorConfig, "MachineConfigError",
			fmt.Sprintf("Internal error while processing rendered Machine Configs: %v", err))
		return reconcile.Result{}, err
	}

	// Apply the objects to the cluster
	setDegraded := false
	var degradedErr error
	for _, obj := range objs {
		// TODO: OwnerRef for non default clusters. For HyperShift this should probably be HostedControlPlane CR
		if apply.GetClusterName(obj) == "" {
			// Mark the object to be GC'd if the owner is deleted.
			if err := controllerutil.SetControllerReference(operConfig, obj, r.client.ClientFor(apply.GetClusterName(obj)).Scheme()); err != nil {
				err = errors.Wrapf(err, "could not set reference for (%s) %s/%s", obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())
				log.Println(err)
				r.status.SetDegraded(statusmanager.OperatorConfig, "InternalError",
					fmt.Sprintf("Internal error while updating operator configuration: %v", err))
				return reconcile.Result{}, err
			}
		}

		// Open question: should an error here indicate we will never retry?
		if err := apply.ApplyObject(ctx, r.client, obj, ControllerName); err != nil {
			err = errors.Wrapf(err, "could not apply (%s) %s/%s", obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())

			// If error comes from nonexistent namespace print out a help message.
			if obj.GroupVersionKind().Kind == "NetworkAttachmentDefinition" && strings.Contains(err.Error(), "namespaces") {
				err = errors.Wrapf(err, "could not apply (%s) %s/%s; Namespace error for networkattachment definition, consider possible solutions: (1) Edit config files to include existing namespace (2) Create non-existent namespace (3) Delete erroneous network-attachment-definition", obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())
			}

			log.Println(err)

			// Ignore errors if we've asked to do so.
			anno := obj.GetAnnotations()
			if anno != nil {
				if _, ok := anno[names.IgnoreObjectErrorAnnotation]; ok {
					log.Println("Object has ignore-errors annotation set, continuing")
					continue
				}
			}
			setDegraded = true
			degradedErr = err
		}
	}

	if setDegraded {
		r.status.SetDegraded(statusmanager.OperatorConfig, "ApplyOperatorConfig",
			fmt.Sprintf("Error while updating operator configuration: %v", degradedErr))
		return reconcile.Result{}, degradedErr
	}

	if operConfig.Spec.Migration != nil && operConfig.Spec.Migration.NetworkType != "" {
		if !(operConfig.Spec.Migration.NetworkType == string(operv1.NetworkTypeOpenShiftSDN) || operConfig.Spec.Migration.NetworkType == string(operv1.NetworkTypeOVNKubernetes)) {
			err = fmt.Errorf("Error: operConfig.Spec.Migration.NetworkType: %s is not equal to either \"OpenshiftSDN\" or \"OVNKubernetes\"", operConfig.Spec.Migration.NetworkType)
			return reconcile.Result{}, err
		}

		migration := operConfig.Spec.Migration
		if migration.Features == nil || migration.Features.EgressFirewall {
			err = migrateEgressFirewallCRs(ctx, operConfig, r.client)
			if err != nil {
				log.Printf("Could not migrate EgressFirewall CRs: %v", err)
				return reconcile.Result{}, err
			}
		}
		if migration.Features == nil || migration.Features.Multicast {
			err = migrateMulticastEnablement(ctx, operConfig, r.client)
			if err != nil {
				log.Printf("Could not migrate Multicast settings: %v", err)
				return reconcile.Result{}, err
			}
		}
		if migration.Features == nil || migration.Features.EgressIP {
			err = migrateEgressIpCRs(ctx, operConfig, r.client)
			if err != nil {
				log.Printf("Could not migrate EgressIP CRs: %v", err)
				return reconcile.Result{}, err
			}
		}
	}

	// Update Network.config.openshift.io.Status
	status, err := r.ClusterNetworkStatus(ctx, operConfig, bootstrapResult)
	if err != nil {
		log.Printf("Could not generate network status: %v", err)
		r.status.SetDegraded(statusmanager.OperatorConfig, "StatusError",
			fmt.Sprintf("Could not update cluster configuration status: %v", err))
		return reconcile.Result{}, err
	}
	if status != nil {
		// Don't set the owner reference in this case -- we're updating
		// the status of our owner.
		if err := apply.ApplyObject(ctx, r.client, status, ControllerName); err != nil {
			err = errors.Wrapf(err, "could not apply (%s) %s/%s", status.GroupVersionKind(), status.GetNamespace(), status.GetName())
			log.Println(err)
			r.status.SetDegraded(statusmanager.OperatorConfig, "StatusError",
				fmt.Sprintf("Could not update cluster configuration status: %v", err))
			return reconcile.Result{}, err
		}
	}

	r.status.SetNotDegraded(statusmanager.OperatorConfig)

	// All was successful. Request that this be re-triggered after ResyncPeriod,
	// so we can reconcile state again.
	log.Printf("Operconfig Controller complete")
	return reconcile.Result{RequeueAfter: ResyncPeriod}, nil
}

func updateIPsecMetric(newOperConfigSpec *operv1.NetworkSpec) {
	if newOperConfigSpec == nil {
		// spec is not initilized yet
		klog.V(5).Infof("IPsec: << updateIPsecTelemetry, new spec is nil, skipping")
	} else if newOperConfigSpec.DefaultNetwork.OVNKubernetesConfig == nil {
		// non ovn-k network, ipsec is not supported
		ipsecMetrics.UpdateIPsecMetricNA()
	} else {
		// ovn-k network, ipsec is supported, update the ipsec state metric
		newOVNKubeConfig := newOperConfigSpec.DefaultNetwork.OVNKubernetesConfig
		mode := string(network.GetIPsecMode(newOVNKubeConfig))
		legacyAPI := network.IsIPsecLegacyAPI(newOVNKubeConfig)

		ipsecMetrics.UpdateIPsecMetric(mode, legacyAPI)
	}
}

func reconcileOperConfig(ctx context.Context, obj crclient.Object) []reconcile.Request {
	log.Printf("%s %s/%s changed, triggering operconf reconciliation", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName())
	// Update reconcile.Request object to align with unnamespaced default network,
	// to ensure we don't have multiple requeueing reconcilers running
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Name: names.OPERATOR_CONFIG,
	}}}
}
