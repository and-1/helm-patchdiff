# helmdiff

Generates a JSON patch diff based on the parameters that would be passed along to `helm upgrade`.

## Usage

```console
$ go build
$ ./helmdiff RELEASE_NAME CHART_NAME
```

## Example

```console
$ helm create test
$ helm install test ./test
$ ./helmdiff test ./test/
$ ./helmdiff test ./test/ --set replicaCount=3
- action: patch
  namespace: default
  object: deployments/test
  patch:
    spec:
      replicas: 3

Changes in release test detected
```
