package controller

import (
	"fmt"
	"path"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	BuilderNamespace = "build-system"
)

type BuilderController struct {
	clientset kubernetes.Interface

	dpController cache.Controller

	// dpIndexer is secondary cache of deployments which is used for object lookups
	dpIndexer cache.Indexer

	// queue is where incoming work is placed to de-dup and to allow "easy"
	// rate limited requeues on errors
	queue workqueue.RateLimitingInterface

	config *Config
}

func NewBuilderController(client kubernetes.Interface, config *Config) *BuilderController {
	c := &BuilderController{
		queue:     workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		clientset: client,
		config:    config,
	}
	c.dpIndexer, c.dpController = cache.NewIndexerInformer(
		cache.NewListWatchFromClient(client.Extensions().RESTClient(), "deployments", metav1.NamespaceAll, fields.Everything()),
		&extensions.Deployment{},
		30*time.Second,
		&cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				d := obj.(*extensions.Deployment)
				glog.V(2).Infof("%s/%s - GOT add event", d.Namespace, d.Name)
				c.enqueue(obj)
			},
			UpdateFunc: func(o, n interface{}) {
				old := o.(*extensions.Deployment)
				new := n.(*extensions.Deployment)
				if old.ResourceVersion == new.ResourceVersion {
					glog.V(2).Infof("%s/%s - update, skip ...", new.Namespace, new.Name)
					return
				}
				glog.Infof("%s/%s - update, queuing ...", new.Namespace, new.Name)
				c.enqueue(new)
			},
			// DeleteFunc: func(obj interface{}) {},
		},
		cache.Indexers{},
	)
	return c
}

func (c *BuilderController) enqueue(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		glog.Infof("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	c.queue.Add(key)
}

func (c *BuilderController) Run(workers int, stopC <-chan struct{}) {
	// don't let panics crash the process
	defer utilruntime.HandleCrash()
	// make sure the work queue is shutdown which will trigger workers to end
	defer c.queue.ShutDown()

	glog.Infof("Starting Builder Controller")

	go c.dpController.Run(stopC)

	if !cache.WaitForCacheSync(stopC, c.dpController.HasSynced) {
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

	glog.Infof("Shutting down Builder controller")

}

func (c *BuilderController) runWorker() {
	for {
		// hot loop until we're told to stop. it will
		// automatically wait until there's work available, so we don't worry
		// about secondary waits
		c.processNextWorkItem()
	}
}

func (c *BuilderController) processNextWorkItem() {
	key, quit := c.queue.Get()
	if quit {
		glog.V(2).Infof("%s - GOT quit", key)
		return
	}

	// you always have to indicate to the queue that you've completed a piece of
	// work
	defer c.queue.Done(key)
	glog.V(2).Infof("Syncing %v", key)
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

func (c *BuilderController) sync(key string) error {
	obj, exists, err := c.dpIndexer.GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		glog.Infof("%s - the deployment doesn't exists", key)
		return nil
	}

	d := obj.(*extensions.Deployment)
	if err := isValidResourceForBuild(d); err != nil {
		glog.Infof("%s - %v", key, err)
		return nil
	}

	buildRevision, _ := strconv.Atoi(d.Annotations["app.manager/build-revision"])
	if buildRevision == 0 {
		buildRevision = 1
	}

	podBuilder := newBuilderPodResource(d, buildRevision, c.config.BuildImage)
	if _, err := c.clientset.Core().Pods(BuilderNamespace).Create(podBuilder); err != nil {
		return fmt.Errorf("failed starting builder pod: %v", err)
	}
	glog.Infof(`%s - pod builder "%s" started successfully`, key, podBuilder.Name)

	// Never mutate our cache, get a new copy of the resource
	dCopy, err := DeploymentDeepCopy(d)
	if err != nil {
		return err
	}

	// Increment the build revision
	dCopy.Annotations["app.manager/build-revision"] = strconv.Itoa(buildRevision + 1)
	// turn of the build, otherwise it will trigger unwanted builds
	dCopy.Annotations["app.manager/build"] = "false"
	_, err = c.clientset.Extensions().Deployments(d.Namespace).Update(dCopy)
	return err
}

func DeploymentDeepCopy(d *extensions.Deployment) (*extensions.Deployment, error) {
	objCopy, err := api.Scheme.DeepCopy(d)
	if err != nil {
		return nil, fmt.Errorf("failed deep copying deployment: %v", err)
	}
	copied, ok := objCopy.(*extensions.Deployment)
	if !ok {
		return nil, fmt.Errorf("expected Deployment, got %#v", objCopy)
	}
	return copied, nil
}

func newBuilderPodResource(d *extensions.Deployment, buildRevision int, buildImage string) *v1.Pod {
	podName := fmt.Sprintf("%s-%s", d.Name, string(uuid.NewUUID()))
	// <registry-url>/<org>/<app-name>:v<revision>
	newDeployImage := path.Join(
		d.Annotations["app.manager/registry-url"],
		d.Annotations["app.manager/registry-org"],
		d.Name,
	) + ":v" + strconv.Itoa(buildRevision)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: BuilderNamespace,
			Annotations: map[string]string{
				"app.manager/deploy-name":      d.Name,
				"app.manager/deploy-namespace": d.Namespace,
				"app.manager/new-deploy-image": newDeployImage,
			},
			Labels: map[string]string{
				// a way for identifying the type of the application that is running
				// will be used to watch only pods with this selector
				"app.manager/type": "builder",
				// the build revision to identify when the pod is running
				"app.manager/build-revision": strconv.Itoa(buildRevision),
			},
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{
				{
					Name:            podName,
					Image:           buildImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args: []string{
						fmt.Sprintf("--clone-url=%s", d.Annotations["app.manager/clone-url"]),
						fmt.Sprintf("--image-name=%s", d.Name),
						fmt.Sprintf("--image-tag=v%d", buildRevision),
						fmt.Sprintf("--registry-url=%s", d.Annotations["app.manager/registry-url"]),
						fmt.Sprintf("--registry-org=%s", d.Annotations["app.manager/registry-org"]),
						fmt.Sprintf("--clone-path=/tmp/%s", d.Name),
						"--overwrite",
						"--logtostderr",
						"--v=4",
					},
				},
			},
		},
	}

	// Defining the volumes
	pod.Spec.Volumes = []v1.Volume{
		{
			Name: "registry-secret",
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: "registry-secret",
				},
			},
		},
		// Will have access to the docker of the host machine
		{
			Name: "docker-socket",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/var/run/docker.sock",
				},
			},
		},
	}
	// Mounting volumes in containers
	pod.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{
		{
			Name:      "registry-secret",
			ReadOnly:  true,
			MountPath: "/var/run/fkt",
		},
		{
			Name:      "docker-socket",
			MountPath: "/var/run/docker.sock",
		},
	}
	return pod
}

func isValidResourceForBuild(d *extensions.Deployment) error {
	if d.Annotations != nil {
		if d.Annotations["app.manager/build"] != "true" {
			return fmt.Errorf(`annotation "app.manager/build" isn't set to "true"`)
		}
	}
	return nil
}
