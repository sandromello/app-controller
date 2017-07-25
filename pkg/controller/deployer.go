package controller

import (
	"fmt"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

var (
	requiredAnnotations = []string{
		"app.manager/deploy-name",
		"app.manager/deploy-namespace",
		"app.manager/new-deploy-image",
	}
)

type DeployerController struct {
	clientset kubernetes.Interface

	// Controllers
	podController cache.Controller

	// podIndexer is secondary cache of pods which is used for object lookups
	podIndexer cache.Indexer

	// queue is where incoming work is placed to de-dup and to allow "easy"
	// rate limited requeues on errors
	queue workqueue.RateLimitingInterface
}

func NewDeployerController(client kubernetes.Interface) *DeployerController {
	c := &DeployerController{
		queue:     workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		clientset: client,
	}
	c.podIndexer, c.podController = cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
				opts.LabelSelector = "app.manager/type=builder"
				return client.Core().Pods(metav1.NamespaceAll).List(opts)
			},
			WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
				opts.LabelSelector = "app.manager/type=builder"
				return client.Core().Pods(metav1.NamespaceAll).Watch(opts)
			},
		},
		&v1.Pod{},
		30*time.Second,
		&cache.ResourceEventHandlerFuncs{
			// AddFunc: func(obj interface{}) {},
			UpdateFunc: func(o, n interface{}) {
				old := o.(*v1.Pod)
				new := n.(*v1.Pod)
				if old.ResourceVersion == new.ResourceVersion {
					glog.V(2).Infof("%s/%s - update, skip ...", new.Namespace, new.Name)
					return
				}
				glog.Infof("%s/%s - update, queuing ...", new.Namespace, new.Name)
				c.enqueue(new)
			},
			// DeleteFunc: func(obj interface{}) {}
		},
		cache.Indexers{},
	)
	return c
}

func (c *DeployerController) enqueue(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		glog.Infof("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	c.queue.Add(key)
}

func (c *DeployerController) Run(workers int, stopC <-chan struct{}) {
	// don't let panics crash the process
	defer utilruntime.HandleCrash()
	// make sure the work queue is shutdown which will trigger workers to end
	defer c.queue.ShutDown()

	glog.Infof("Starting Deployer Controller")

	go c.podController.Run(stopC)

	if !cache.WaitForCacheSync(stopC, c.podController.HasSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		// runWorker will loop until "something bad" happens.
		// The .Until will then rekick the worker after one second
		go wait.Until(c.runWorker, time.Second, stopC)
	}
	// run will loop until "something bad" happens.
	// It will rekick the worker after one second
	<-stopC

	glog.Infof("Shutting down Deployer controller")
}

func (c *DeployerController) runWorker() {
	for {
		// hot loop until we're told to stop. it will
		// automatically wait until there's work available, so we don't worry
		// about secondary waits
		c.processNextWorkItem()
	}
}

func (c *DeployerController) processNextWorkItem() {
	// pull the next work item from queue.  It should be a key we use to lookup
	// something in a cache
	key, quit := c.queue.Get()
	if quit {
		return
	}

	// you always have to indicate to the queue that you've completed a piece of
	// work
	defer c.queue.Done(key)
	glog.V(3).Infof("Syncing %v", key)
	if err := c.sync(key.(string)); err != nil {
		// there was a failure so be sure to report it.  This method allows for
		// pluggable error handling which can be used for things like
		// cluster-monitoring
		utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

		// since we failed, we should requeue the item to work on later.  This
		// method will add a backoff to avoid hotlooping on particular items
		// (they're probably still not going to work right away) and overall
		// controller protection (everything I've done is broken, this controller
		// needs to calm down or it can starve other useful work) cases.
		c.queue.AddRateLimited(key)
		return
	}
	// if you had no error, tell the queue to stop tracking history for your
	// key. This will reset things like failure counts for per-item rate
	// limiting
	c.queue.Forget(key)
}

func (c *DeployerController) sync(key string) error {
	obj, exists, err := c.podIndexer.GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		glog.Infof("%s - the pod doesn't exists", key)
		return nil
	}

	pod := obj.(*v1.Pod)
	if pod.Namespace != BuilderNamespace {
		glog.V(2).Infof("%s - it's not a builder pod", key)
		return nil
	}

	if pod.DeletionTimestamp != nil {
		glog.Infof("%s - marked for deletion, skip", key)
		return nil
	}

	glog.Infof(`%s - got build pod in phase %#s`, key, pod.Status.Phase)
	if pod.Status.Phase == v1.PodSucceeded {
		glog.Infof("%s - preparing for deploy", key)
		if err := isValidResourceForDeploy(pod); err != nil {
			glog.Warningf("%s - %s", key, err)
			return nil
		}

		_, err = c.clientset.Extensions().Deployments(pod.Annotations["app.manager/deploy-namespace"]).Patch(
			pod.Annotations["app.manager/deploy-name"],
			types.MergePatchType,
			getPatchData(pod),
		)
		if err != nil {
			return fmt.Errorf("failed deploying new release: %v", err)
		}
		glog.Infof(`%s - deployed new release "%s" with success`, key, pod.Annotations["app.manager/new-deploy-image"])
	}
	return nil
}

func getPatchData(pod *v1.Pod) []byte {
	// func getPatchData(deployName string, buildRev int, imageNewVersion string) []byte {
	buildRevision, _ := strconv.Atoi(pod.Annotations["app.manager/build-revision"])
	if buildRevision == 0 {
		buildRevision = 1
	}
	o := `{"metadata": {"annotations": {"%s": "%d"}}, "spec": {"paused": false, "template": {"spec": {"containers": [{"name": "%s", "image": "%s", "args": ["web"]}]}}}}`
	return []byte(fmt.Sprintf(o,
		"app.manager/build-revision",
		buildRevision,
		pod.Annotations["app.manager/deploy-name"],
		pod.Annotations["app.manager/new-deploy-image"],
	))
}

func isValidResourceForDeploy(pod *v1.Pod) error {
	if pod.Annotations == nil || pod.Labels == nil {
		return fmt.Errorf(`spec.annotations or spec.labels are null`)
	}
	for _, annotation := range requiredAnnotations {
		if _, ok := pod.Annotations[annotation]; !ok {
			return fmt.Errorf(`missing "%s" annotation`, annotation)
		}
	}
	return nil
}
