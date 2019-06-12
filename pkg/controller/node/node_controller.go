package node

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/golang/glog"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	daemonconsts "github.com/openshift/machine-config-operator/pkg/daemon/constants"
	mcfgclientset "github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned"
	"github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned/scheme"
	mcfginformersv1 "github.com/openshift/machine-config-operator/pkg/generated/informers/externalversions/machineconfiguration.openshift.io/v1"
	mcfglistersv1 "github.com/openshift/machine-config-operator/pkg/generated/listers/machineconfiguration.openshift.io/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	coreclientsetv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	clientretry "k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
)

const (
	// maxRetries is the number of times a machineconfig pool will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a machineconfig pool is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	// updateDelay is a pause to deal with churn in MachineConfigs; see
	// https://github.com/openshift/machine-config-operator/issues/301
	updateDelay = 5 * time.Second
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = mcfgv1.SchemeGroupVersion.WithKind("MachineConfigPool")

var nodeUpdateBackoff = wait.Backoff{
	Steps:    5,
	Duration: 100 * time.Millisecond,
	Jitter:   1.0,
}

// Controller defines the node controller.
type Controller struct {
	client        mcfgclientset.Interface
	kubeClient    clientset.Interface
	eventRecorder record.EventRecorder

	syncHandler              func(mcp string) error
	enqueueMachineConfigPool func(*mcfgv1.MachineConfigPool)

	mcpLister  mcfglistersv1.MachineConfigPoolLister
	mcLister   mcfglistersv1.MachineConfigLister
	nodeLister corelisterv1.NodeLister

	mcpListerSynced  cache.InformerSynced
	mcListerSynced   cache.InformerSynced
	nodeListerSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface
}

// New returns a new node controller.
func New(
	mcpInformer mcfginformersv1.MachineConfigPoolInformer,
	mcInformer mcfginformersv1.MachineConfigInformer,
	nodeInformer coreinformersv1.NodeInformer,
	kubeClient clientset.Interface,
	mcfgClient mcfgclientset.Interface,
) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&coreclientsetv1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	ctrl := &Controller{
		client:        mcfgClient,
		kubeClient:    kubeClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "machineconfigcontroller-nodecontroller"}),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineconfigcontroller-nodecontroller"),
	}

	mcpInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addMachineConfigPool,
		UpdateFunc: ctrl.updateMachineConfigPool,
		DeleteFunc: ctrl.deleteMachineConfigPool,
	})
	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addNode,
		UpdateFunc: ctrl.updateNode,
		DeleteFunc: ctrl.deleteNode,
	})

	ctrl.syncHandler = ctrl.syncMachineConfigPool
	ctrl.enqueueMachineConfigPool = ctrl.enqueueDefault

	ctrl.mcpLister = mcpInformer.Lister()
	ctrl.mcLister = mcInformer.Lister()
	ctrl.nodeLister = nodeInformer.Lister()

	ctrl.mcpListerSynced = mcpInformer.Informer().HasSynced
	ctrl.mcListerSynced = mcInformer.Informer().HasSynced
	ctrl.nodeListerSynced = nodeInformer.Informer().HasSynced

	return ctrl
}

// Run executes the render controller.
func (ctrl *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ctrl.queue.ShutDown()

	if !cache.WaitForCacheSync(stopCh, ctrl.mcpListerSynced, ctrl.nodeListerSynced) {
		return
	}

	glog.Info("Starting MachineConfigController-NodeController")
	defer glog.Info("Shutting down MachineConfigController-NodeController")

	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (ctrl *Controller) addMachineConfigPool(obj interface{}) {
	pool := obj.(*mcfgv1.MachineConfigPool)
	glog.V(4).Infof("Adding MachineConfigPool %s", pool.Name)
	ctrl.enqueueMachineConfigPool(pool)

}

func (ctrl *Controller) updateMachineConfigPool(old, cur interface{}) {
	oldPool := old.(*mcfgv1.MachineConfigPool)
	curPool := cur.(*mcfgv1.MachineConfigPool)

	glog.V(4).Infof("Updating MachineConfigPool %s", oldPool.Name)
	ctrl.enqueueMachineConfigPool(curPool)
}

func (ctrl *Controller) deleteMachineConfigPool(obj interface{}) {
	pool, ok := obj.(*mcfgv1.MachineConfigPool)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
			return
		}
		pool, ok = tombstone.Obj.(*mcfgv1.MachineConfigPool)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a MachineConfigPool %#v", obj))
			return
		}
	}
	glog.V(4).Infof("Deleting MachineConfigPool %s", pool.Name)
	// TODO(abhinavdahiya): handle deletes.
}

func (ctrl *Controller) addNode(obj interface{}) {
	node := obj.(*corev1.Node)
	if node.DeletionTimestamp != nil {
		ctrl.deleteNode(node)
		return
	}

	pool, err := ctrl.getPoolForNode(node)
	if err != nil {
		glog.Errorf("error finding pools for node: %v", err)
		return
	}
	if pool == nil {
		return
	}
	glog.V(4).Infof("Node %s added", node.Name)
	ctrl.enqueueMachineConfigPool(pool)
}

func (ctrl *Controller) updateNode(old, cur interface{}) {
	oldNode := old.(*corev1.Node)
	curNode := cur.(*corev1.Node)

	if !isNodeManaged(curNode) {
		return
	}

	pool, err := ctrl.getPoolForNode(curNode)
	if err != nil {
		glog.Errorf("error finding pool for node: %v", err)
		return
	}
	if pool == nil {
		return
	}
	glog.V(4).Infof("Node %s updated", curNode.Name)

	var changed bool
	oldReadyErr := checkNodeReady(oldNode)
	newReadyErr := checkNodeReady(curNode)

	oldReady := getErrorString(oldReadyErr)
	newReady := getErrorString(newReadyErr)

	if oldReady != newReady {
		changed = true
		if newReadyErr != nil {
			glog.Infof("Pool %s: node %s is now reporting unready: %v", pool.Name, curNode.Name, newReadyErr)
		} else {
			glog.Infof("Pool %s: node %s is now reporting ready", pool.Name, curNode.Name)
		}
	}

	// Specifically log when a node has completed an update so the MCC logs are a useful central aggregate of state changes
	if oldNode.Annotations[daemonconsts.CurrentMachineConfigAnnotationKey] != oldNode.Annotations[daemonconsts.DesiredMachineConfigAnnotationKey] &&
		isNodeDone(curNode) {
		glog.Infof("Pool %s: node %s has completed update to %s", pool.Name, curNode.Name, curNode.Annotations[daemonconsts.DesiredMachineConfigAnnotationKey])
		changed = true
	} else {
		annos := []string{daemonconsts.CurrentMachineConfigAnnotationKey, daemonconsts.DesiredMachineConfigAnnotationKey, daemonconsts.MachineConfigDaemonStateAnnotationKey}
		for _, anno := range annos {
			if oldNode.Annotations[anno] != curNode.Annotations[anno] {
				glog.Infof("Pool %s: node %s changed %s = %s", pool.Name, curNode.Name, anno, curNode.Annotations[anno])
				changed = true
			}
		}
	}

	if !changed {
		return
	}

	ctrl.enqueueMachineConfigPool(pool)
}

func (ctrl *Controller) deleteNode(obj interface{}) {
	node, ok := obj.(*corev1.Node)

	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
			return
		}
		node, ok = tombstone.Obj.(*corev1.Node)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a Node %#v", obj))
			return
		}
	}

	pool, err := ctrl.getPoolForNode(node)
	if err != nil {
		glog.Errorf("error finding pools for node: %v", err)
		return
	}
	if pool == nil {
		return
	}
	glog.V(4).Infof("Node %s delete", node.Name)
	ctrl.enqueueMachineConfigPool(pool)
}

// getPoolForNode chooses the MachineConfigPool that should be used for a given node.
// It disambiguates in the case where e.g. a node has both master/worker roles applied,
// and where a custom role may be used.
func (ctrl *Controller) getPoolForNode(node *corev1.Node) (*mcfgv1.MachineConfigPool, error) {
	pl, err := ctrl.mcpLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var pools []*mcfgv1.MachineConfigPool
	for _, p := range pl {
		selector, err := metav1.LabelSelectorAsSelector(p.Spec.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %v", err)
		}

		// If a pool with a nil or empty selector creeps in, it should match nothing, not everything.
		if selector.Empty() || !selector.Matches(labels.Set(node.Labels)) {
			continue
		}

		pools = append(pools, p)
	}

	if len(pools) == 0 {
		// This is not an error, as there might be nodes in cluster that are not managed by machineconfigpool.
		return nil, nil
	}

	var master, worker *mcfgv1.MachineConfigPool
	var custom []*mcfgv1.MachineConfigPool
	for _, pool := range pools {
		if pool.Name == "master" {
			master = pool
		} else if pool.Name == "worker" {
			worker = pool
		} else {
			custom = append(custom, pool)
		}
	}

	if len(custom) > 1 {
		return nil, fmt.Errorf("node %s belongs to %d custom roles, cannot proceed with this Node", node.Name, len(custom))
	} else if len(custom) == 1 {
		// We don't support making custom pools for masters
		if master != nil {
			return nil, fmt.Errorf("node %s has both master role and custom role %s", node.Name, custom[0].Name)
		}
		// One custom role, let's use its pool
		return custom[0], nil
	} else if master != nil {
		// In the case where a node is both master/worker, have it live under
		// the master pool. This occurs in CodeReadyContainers and general
		// "single node" deployments, which one may want to do for testing bare
		// metal, etc.
		return master, nil
	}
	// Otherwise, it's a worker with no custom roles.
	return worker, nil
}

func (ctrl *Controller) enqueue(pool *mcfgv1.MachineConfigPool) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pool)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", pool, err))
		return
	}

	ctrl.queue.Add(key)
}

func (ctrl *Controller) enqueueRateLimited(pool *mcfgv1.MachineConfigPool) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pool)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", pool, err))
		return
	}

	ctrl.queue.AddRateLimited(key)
}

// enqueueAfter will enqueue a pool after the provided amount of time.
func (ctrl *Controller) enqueueAfter(pool *mcfgv1.MachineConfigPool, after time.Duration) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pool)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", pool, err))
		return
	}

	ctrl.queue.AddAfter(key, after)
}

// enqueueDefault calls a default enqueue function
func (ctrl *Controller) enqueueDefault(pool *mcfgv1.MachineConfigPool) {
	ctrl.enqueueAfter(pool, updateDelay)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (ctrl *Controller) worker() {
	for ctrl.processNextWorkItem() {
	}
}

func (ctrl *Controller) processNextWorkItem() bool {
	key, quit := ctrl.queue.Get()
	if quit {
		return false
	}
	defer ctrl.queue.Done(key)

	err := ctrl.syncHandler(key.(string))
	ctrl.handleErr(err, key)

	return true
}

func (ctrl *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		ctrl.queue.Forget(key)
		return
	}

	if ctrl.queue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing machineconfigpool %v: %v", key, err)
		ctrl.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping machineconfigpool %q out of the queue: %v", key, err)
	ctrl.queue.Forget(key)
	ctrl.queue.AddAfter(key, 1*time.Minute)
}

// syncMachineConfigPool will sync the machineconfig pool with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (ctrl *Controller) syncMachineConfigPool(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing machineconfigpool %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing machineconfigpool %q (%v)", key, time.Since(startTime))
	}()

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	machineconfigpool, err := ctrl.mcpLister.Get(name)
	if errors.IsNotFound(err) {
		glog.V(2).Infof("MachineConfigPool %v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	if machineconfigpool.Spec.Configuration.Name == "" {
		// Previously we spammed the logs about empty pools.
		// Let's just pause for a bit here to let the renderer
		// initialize them.
		glog.Infof("Pool %s is unconfigured, pausing %v for renderer to initialize", name, updateDelay)
		time.Sleep(updateDelay)
		return nil
	}

	// Deep-copy otherwise we are mutating our cache.
	// TODO: Deep-copy only when needed.
	pool := machineconfigpool.DeepCopy()
	everything := metav1.LabelSelector{}

	if reflect.DeepEqual(pool.Spec.NodeSelector, &everything) {
		ctrl.eventRecorder.Eventf(pool, corev1.EventTypeWarning, "SelectingAll", "This machineconfigpool is selecting all nodes. A non-empty selector is required.")
		return nil
	}

	if pool.DeletionTimestamp != nil {
		return ctrl.syncStatusOnly(pool)
	}

	if pool.Spec.Paused {
		return ctrl.syncStatusOnly(pool)
	}

	nodes, err := ctrl.getNodesForPool(pool)
	if err != nil {
		return err
	}

	maxunavail, err := maxUnavailable(pool, nodes)
	if err != nil {
		return err
	}

	candidates := getCandidateMachines(pool, nodes, maxunavail)
	for _, node := range candidates {
		if err := ctrl.syncLabelsAndTaints(node, pool.Spec.Configuration.Name); err != nil {
			return err
		}
		if err := ctrl.setDesiredMachineConfigAnnotation(node.Name, pool.Spec.Configuration.Name); err != nil {
			return err
		}
	}
	return ctrl.syncStatusOnly(pool)
}

func (ctrl *Controller) getNodesForPool(pool *mcfgv1.MachineConfigPool) ([]*corev1.Node, error) {
	selector, err := metav1.LabelSelectorAsSelector(pool.Spec.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid label selector: %v", err)
	}

	initialNodes, err := ctrl.nodeLister.List(selector)
	if err != nil {
		return nil, err
	}

	nodes := []*corev1.Node{}
	for _, n := range initialNodes {
		p, err := ctrl.getPoolForNode(n)
		if err != nil {
			glog.Warningf("can't get pool for node %q: %v", n.Name, err)
			continue
		}
		if p.Name != pool.Name {
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func updateLabels(node *corev1.Node, mcls []mcfgv1.MachineConfigLabels) *corev1.Node {
	for _, mcl := range mcls {
		for label, value := range mcl.Labels {
			if mcl.Exist {
				node.ObjectMeta.Labels[label] = value
			} else {
				delete(node.ObjectMeta.Labels, label)
			}
		}
	}
	return node
}

func findTaint(node *corev1.Node, taint corev1.Taint) (bool, int) {
	for i, t := range node.Spec.Taints {
		if t.Key == taint.Key {
			return true, i
		}
	}
	return false, 0
}

func updateTaints(node *corev1.Node, mcts []mcfgv1.MachineConfigTaint) *corev1.Node {
	for i := range mcts {
		mct := &mcts[i]
		found, index := findTaint(node, mct.Taint)
		if mct.Exist {
			if !found {
				// Supposed to be there but wasn't found, add it.
				node.Spec.Taints = append(node.Spec.Taints, mct.Taint)
				continue
			}
			// Supposed to be there and was, just overwrite it, instead of wasting time
			// checking if it has the right values.
			node.Spec.Taints[index] = mct.Taint
		} else {
			if !found {
				// Not supposed to be there and not there
				continue
			}
			// Not supposed to be there, but is, so delete it.
			node.Spec.Taints = append(node.Spec.Taints[:index], node.Spec.Taints[index+1:]...)
		}
	}
	return node
}

func (ctrl *Controller) syncLabelsAndTaints(oldNode *corev1.Node, currentConfigName string) error {
	mc, err := ctrl.mcLister.Get(currentConfigName)
	if err != nil {
		return err
	}

	return clientretry.RetryOnConflict(nodeUpdateBackoff, func() error {
		oldData, err := json.Marshal(oldNode)
		if err != nil {
			return err
		}

		newNode := oldNode.DeepCopy()

		newNode = updateLabels(newNode, mc.Spec.Labels)
		newNode = updateTaints(newNode, mc.Spec.Taints)

		if equality.Semantic.DeepEqual(oldNode, newNode) {
			return nil
		}

		newData, err := json.Marshal(newNode)
		if err != nil {
			return err
		}

		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, corev1.Node{})
		if err != nil {
			return fmt.Errorf("failed to create patch for node %q: %v", oldNode.Name, err)
		}
		_, err = ctrl.kubeClient.CoreV1().Nodes().Patch(oldNode.Name, types.StrategicMergePatchType, patchBytes)
		return err
	})
}

func (ctrl *Controller) setDesiredMachineConfigAnnotation(nodeName, currentConfig string) error {
	glog.Infof("Setting node %s to desired config %s", nodeName, currentConfig)
	return clientretry.RetryOnConflict(nodeUpdateBackoff, func() error {
		oldNode, err := ctrl.kubeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		oldData, err := json.Marshal(oldNode)
		if err != nil {
			return err
		}

		newNode := oldNode.DeepCopy()
		if newNode.Annotations == nil {
			newNode.Annotations = map[string]string{}
		}
		if newNode.Annotations[daemonconsts.DesiredMachineConfigAnnotationKey] == currentConfig {
			return nil
		}
		newNode.Annotations[daemonconsts.DesiredMachineConfigAnnotationKey] = currentConfig
		newData, err := json.Marshal(newNode)
		if err != nil {
			return err
		}

		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, corev1.Node{})
		if err != nil {
			return fmt.Errorf("failed to create patch for node %q: %v", nodeName, err)
		}
		_, err = ctrl.kubeClient.CoreV1().Nodes().Patch(nodeName, types.StrategicMergePatchType, patchBytes)
		return err
	})
}

func getCandidateMachines(pool *mcfgv1.MachineConfigPool, nodesInPool []*corev1.Node, maxUnavailable int) []*corev1.Node {
	targetConfig := pool.Spec.Configuration.Name

	unavail := getUnavailableMachines(nodesInPool)
	// If we're at capacity, there's nothing to do.
	if len(unavail) >= maxUnavailable {
		return nil
	}
	capacity := maxUnavailable - len(unavail)
	failingThisConfig := 0
	// We only look at nodes which aren't already targeting our desired config
	var nodes []*corev1.Node
	for _, node := range nodesInPool {
		if node.Annotations[daemonconsts.DesiredMachineConfigAnnotationKey] == targetConfig {
			if isNodeMCDFailing(node) {
				failingThisConfig++
			}
			continue
		}

		nodes = append(nodes, node)
	}

	// Nodes which are failing to target this config also count against
	// availability - it might be a transient issue, and if the issue
	// clears we don't want multiple to update at once.
	if failingThisConfig >= capacity {
		return nil
	}
	capacity -= failingThisConfig

	if len(nodes) < capacity {
		return nodes
	}
	return nodes[:capacity]
}

func maxUnavailable(pool *mcfgv1.MachineConfigPool, nodes []*corev1.Node) (int, error) {
	intOrPercent := intstrutil.FromInt(1)
	if pool.Spec.MaxUnavailable != nil {
		intOrPercent = *pool.Spec.MaxUnavailable
	}
	maxunavail, err := intstrutil.GetValueFromIntOrPercent(&intOrPercent, len(nodes), false)
	if err != nil {
		return 0, err
	}
	if maxunavail == 0 {
		maxunavail = 1
	}
	if pool.Name == "master" {
		// calculate the fault tolerance dynamically for the master pool
		// to avoid risking losing etcd quorum.
		tolerance := len(nodes) - ((len(nodes) / 2) + 1)
		if maxunavail > tolerance {
			glog.Warningf("Refusing to honor master pool maxUnavailable %d to prevent losing etcd quorum, using %d instead", maxunavail, tolerance)
			return tolerance, nil
		}
	}
	return maxunavail, nil
}

// getErrorString returns error string if not nil and empty string if error is nil
func getErrorString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
