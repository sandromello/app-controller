package main

import (
	"flag"

	"github.com/golang/glog"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sandromello/app-controller/pkg/controller"
)

var cfg controller.Config

func init() {
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.StringVar(&cfg.Host, "apiserver", "", "api server addr, e.g. 'http://127.0.0.1:8080'. Omit parameter to run in on-cluster mode and utilize the service account token.")
	pflag.StringVar(&cfg.TLSConfig.CertFile, "cert-file", "", "path to public TLS certificate file.")
	pflag.StringVar(&cfg.TLSConfig.KeyFile, "key-file", "", "path to private TLS certificate file.")
	pflag.StringVar(&cfg.TLSConfig.CAFile, "ca-file", "", "path to TLS CA file.")
	pflag.BoolVar(&cfg.TLSInsecure, "tls-insecure", false, "don't verify API server's CA certificate.")

	pflag.StringVar(&cfg.BuildImage, "build-image", "", "the image to build new applications")
	pflag.Parse()
}

func main() {
	if len(cfg.BuildImage) == 0 {
		glog.Fatalf(`missing "build-image" option`)
	}
	var config *rest.Config
	var err error
	if len(cfg.Host) == 0 {
		config, err = rest.InClusterConfig()
		if err != nil {
			glog.Fatalf("error creating client configuration: %v", err)
		}
	} else {
		config = &rest.Config{
			Host:            cfg.Host,
			TLSClientConfig: cfg.TLSConfig,
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("failed retrieving clientset from config: %v", err)
	}

	if _, err := clientset.Namespaces().Create(&v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: controller.BuilderNamespace},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		glog.Fatalf(`failed creating system namespace "%s": %v`, controller.BuilderNamespace, err)
	}

	stopC := wait.NeverStop
	go controller.NewBuilderController(clientset, &cfg).Run(1, stopC)
	go controller.NewDeployerController(clientset).Run(1, stopC)
	select {}
}
