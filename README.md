# App Manager

It's an implementation example of a custom Kubernetes controller.
App Manager is responsible for triggering builds and deploying new applications in a Kubernetes cluster

## Pre-Requisites

Minikube v1.6+

## How it works

1) An user creates a deployment paused with the following annotations:

- `app.manager.build` - A string boolean (true|false) indicating to trigger a new build
- `app.manager.clone-url` - Contain the URL of the git server where the app of the user resides
- `app.manager.registry-url` - The FQDN of the registry where the image will be pushed 
- `app.manager.registry-org` - The organization or the owner of the registry

2) The controller watches for deployments resources and identify candidates for builds based on the following annotation `app.manager.build=true`
3) Creates a POD resource to build the application, the builder is responsible for:
  - Clone the application in a specific directory using the `app.manager.clone-url` annotation as the URL
  - Detect the type of the application and trigger a docker build with a specific Dockerfile
  - Push the new image to the registry using the annotations `app.manager.registry-url` and `app.manager.registry-org`
4) Watch for pods resources with `app.manager/builder=true` labels with the `Completed` state in `status.phase`
5) Update the deployment with the new image triggering a new release
