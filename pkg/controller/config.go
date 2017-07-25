package controller

import "k8s.io/client-go/rest"

type Config struct {
	Host        string
	TLSInsecure bool
	TLSConfig   rest.TLSClientConfig
	BuildImage  string
}
