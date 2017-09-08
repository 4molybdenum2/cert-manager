package issuers

import (
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/jetstack-experimental/cert-manager/pkg/apis/certmanager"
	"github.com/jetstack-experimental/cert-manager/pkg/client"
	controllerpkg "github.com/jetstack-experimental/cert-manager/pkg/controller"
	cminformers "github.com/jetstack-experimental/cert-manager/pkg/informers/certmanager/v1alpha1"
	"github.com/jetstack-experimental/cert-manager/pkg/issuer"
	cmlisters "github.com/jetstack-experimental/cert-manager/pkg/listers/certmanager/v1alpha1"
)

type Controller struct {
	client        kubernetes.Interface
	cmClient      client.Interface
	issuerFactory issuer.Factory

	// To allow injection for testing.
	syncHandler func(key string) error

	issuerInformerSynced cache.InformerSynced
	issuerLister         cmlisters.IssuerLister

	secretInformerSynced cache.InformerSynced
	secretLister         corelisters.SecretLister

	queue workqueue.RateLimitingInterface
}

func New(
	issuersInformer cache.SharedIndexInformer,
	secretsInformer cache.SharedIndexInformer,
	cl kubernetes.Interface,
	cmClient client.Interface,
	issuerFactory issuer.Factory,
) *Controller {
	ctrl := &Controller{client: cl, cmClient: cmClient, issuerFactory: issuerFactory}
	ctrl.syncHandler = ctrl.processNextWorkItem
	ctrl.queue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "issuers")

	issuersInformer.AddEventHandler(&controllerpkg.QueuingEventHandler{Queue: ctrl.queue})
	ctrl.issuerInformerSynced = issuersInformer.HasSynced
	ctrl.issuerLister = cmlisters.NewIssuerLister(issuersInformer.GetIndexer())

	secretsInformer.AddEventHandler(&controllerpkg.BlockingEventHandler{WorkFunc: ctrl.secretDeleted})
	ctrl.secretInformerSynced = secretsInformer.HasSynced
	ctrl.secretLister = corelisters.NewSecretLister(secretsInformer.GetIndexer())

	return ctrl
}

// TODO: replace with generic handleObjet function (like Navigator)
func (c *Controller) secretDeleted(obj interface{}) {
	var secret *corev1.Secret
	var ok bool
	secret, ok = obj.(*corev1.Secret)
	if !ok {
		runtime.HandleError(fmt.Errorf("Object was not a Secret object %#v", obj))
		return
	}
	issuers, err := c.issuersForSecret(secret)
	if err != nil {
		runtime.HandleError(fmt.Errorf("Error looking up issuers observing Secret: %s/%s", secret.Namespace, secret.Name))
		return
	}
	for _, iss := range issuers {
		key, err := keyFunc(iss)
		if err != nil {
			runtime.HandleError(err)
			continue
		}
		c.queue.Add(key)
	}
}

func (c *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer c.queue.ShutDown()

	log.Printf("Starting control loop")
	// wait for all the informer caches we depend on are synced
	if !cache.WaitForCacheSync(stopCh, c.issuerInformerSynced, c.secretInformerSynced) {
		// TODO: replace with Errorf call to glog
		log.Printf("error waiting for informer caches to sync")
		return
	}

	for i := 0; i < workers; i++ {
		// TODO (@munnerz): make time.Second duration configurable
		go wait.Until(c.worker, time.Second, stopCh)
	}

	<-stopCh
	log.Printf("shutting down queue as workqueue signalled shutdown")
}

func (c *Controller) worker() {
	log.Printf("starting worker")
	for {
		obj, shutdown := c.queue.Get()
		if shutdown {
			break
		}

		err := func(obj interface{}) error {
			defer c.queue.Done(obj)
			var key string
			var ok bool
			if key, ok = obj.(string); !ok {
				runtime.HandleError(fmt.Errorf("expected string in workqueue but got %T", obj))
				return nil
			}
			if err := c.syncHandler(key); err != nil {
				return err
			}
			c.queue.Forget(obj)
			return nil
		}(obj)

		if err != nil {
			log.Printf("requeuing item due to error processing: %s", err.Error())
			c.queue.AddRateLimited(obj)
			continue
		}

		log.Printf("finished processing work item")
	}
	log.Printf("exiting worker loop")
}

func (c *Controller) processNextWorkItem(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	issuer, err := c.issuerLister.Issuers(namespace).Get(name)

	if err != nil {
		if k8sErrors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("issuer '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	return c.Sync(issuer)
}

var keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc

const (
	ControllerName = "issuers"
)

func init() {
	controllerpkg.Register(ControllerName, func(ctx *controllerpkg.Context, stopCh <-chan struct{}) (bool, error) {
		go New(
			ctx.SharedInformerFactory.InformerFor(
				ctx.Namespace,
				metav1.GroupVersionKind{Group: certmanager.GroupName, Version: "v1alpha1", Kind: "Issuer"},
				cminformers.NewIssuerInformer(
					ctx.CMClient,
					ctx.Namespace,
					time.Second*30,
					cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
				),
			),
			ctx.SharedInformerFactory.InformerFor(
				ctx.Namespace,
				metav1.GroupVersionKind{Version: "v1", Kind: "Secret"},
				coreinformers.NewSecretInformer(
					ctx.Client,
					ctx.Namespace,
					time.Second*30,
					cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
				),
			),
			ctx.Client,
			ctx.CMClient,
			ctx.IssuerFactory,
		).Run(2, stopCh)

		return true, nil
	})
}
