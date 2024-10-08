//go:build integ
// +build integ

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/cluster"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/common/ports"
	"istio.io/istio/pkg/test/framework/components/echo/deployment"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/util/retry"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	clusterNames = []string{"east", "west"}

	appNs    namespace.Instance
	eastApps echo.Instances
	westApps echo.Instances

	_, file, _, _ = runtime.Caller(0)
	rootDir       = filepath.Join(filepath.Dir(file), "../..")
)

const (
	eastClusterName = "cluster-0"
	westClusterName = "cluster-1"
)

func TestMain(m *testing.M) {
	framework.
		NewSuite(m).
		Setup(createControlPlaneNamespace).
		Setup(createCACertsSecret).
		Setup(deployFederationControllers).
		Setup(deployControlPlanes).
		Setup(patchFederationControllers).
		Setup(namespace.Setup(&appNs, namespace.Config{Prefix: "app", Inject: true})).
		// a - client
		// b - service available in east and west clusters - covers importing with WorkloadEntry
		// c - service available only in west cluster - covers importing with ServiceEntry
		Setup(deployApps(&eastApps, eastClusterName, namespace.Future(&appNs), "a", "b")).
		Setup(deployApps(&westApps, westClusterName, namespace.Future(&appNs), "b", "c")).
		// c must be removed from the east cluster, because we want to test importing a service
		// that exists only in the remote cluster.
		Setup(removeServiceFromClusters("c", namespace.Future(&appNs), eastClusterName)).
		Run()
}

func createControlPlaneNamespace(ctx resource.Context) error {
	if len(ctx.Clusters()) > 2 {
		return fmt.Errorf("too many clusters - expected 2, got %d", len(ctx.Clusters()))
	}

	createNamespace := func(cluster cluster.Cluster) error {
		if _, err := cluster.Kube().CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
			ObjectMeta: v1.ObjectMeta{
				Name: "istio-system",
			},
		}, v1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create namespace: %v", err)
		}
		return nil
	}

	for _, c := range ctx.Clusters() {
		if err := retry.UntilSuccess(func() error {
			_, err := c.Kube().CoreV1().Namespaces().Get(context.TODO(), "istio-system", v1.GetOptions{})
			if err != nil {
				if !errors.IsNotFound(err) {
					return fmt.Errorf("failed to get namespace: %v", err)
				}
				return createNamespace(c)
			}
			if err := c.Kube().CoreV1().Namespaces().Delete(context.TODO(), "istio-system", v1.DeleteOptions{}); err != nil {
				return fmt.Errorf("failed to delete namespace: %v", err)
			}
			return createNamespace(c)
		}); err != nil {
			return err
		}
	}
	return nil
}

func createCACertsSecret(ctx resource.Context) error {
	for idx, c := range ctx.Clusters() {
		clusterName := clusterNames[idx]
		data := map[string][]byte{
			"root-cert.pem":  {},
			"cert-chain.pem": {},
			"ca-cert.pem":    {},
			"ca-key.pem":     {},
		}
		if err := setCacertKeys(fmt.Sprintf("%s/test/testdata/certs/%s", rootDir, clusterName), data); err != nil {
			return fmt.Errorf("failed to set keys in cacerts secret (cluster=%s): %v", clusterName, err)
		}
		cacerts := &corev1.Secret{
			ObjectMeta: v1.ObjectMeta{
				Name:      "cacerts",
				Namespace: "istio-system",
			},
			Data: data,
		}
		if _, err := c.Kube().CoreV1().Secrets("istio-system").Create(context.TODO(), cacerts, v1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create cacerts secret (cluster=%s): %v", clusterName, err)
		}
	}
	return nil
}

func setCacertKeys(dir string, data map[string][]byte) error {
	for key := range data {
		fileName := fmt.Sprintf("%s/%s", dir, key)
		fileData, err := os.ReadFile(fileName)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %v", fileName, err)
		}
		data[key] = fileData
	}
	return nil
}

// deployControlPlanes deploys Istio using the manifest generated from IstioOperator resource.
// We can't utilize standard Istio installation supported by the Istio framework,
// because it does not allow to apply different Istio settings to different primary clusters
// and always sets up direct access to the k8s api-server, while it's not desired in mesh federation.
func deployControlPlanes(ctx resource.Context) error {
	for idx, c := range ctx.Clusters() {
		clusterName := clusterNames[idx]
		if err := c.Config().ApplyYAMLFiles("", fmt.Sprintf("%s/test/testdata/out/istio-%s-manifests.yaml", rootDir, clusterName)); err != nil {
			return fmt.Errorf("failed to deploy istio control plane: %v", err)
		}
	}
	return nil
}

func deployFederationControllers(ctx resource.Context) error {
	for _, c := range ctx.Clusters() {
		if err := c.Config().ApplyYAMLFiles("istio-system", fmt.Sprintf("%s/test/testdata/federation-controller-manifests.yaml", rootDir)); err != nil {
			return fmt.Errorf("failed to deploy federation controller: %v", err)
		}
	}
	return nil
}

func patchFederationControllers(ctx resource.Context) error {
	for _, localCluster := range ctx.Clusters() {
		var dataPlaneIP string
		var discoveryIP string
		for _, remoteCluster := range ctx.Clusters() {
			if localCluster.Name() == remoteCluster.Name() {
				continue
			}
			var err error
			dataPlaneIP, err = findLoadBalancerIP(remoteCluster, "istio-eastwestgateway", "istio-system")
			discoveryIP, err = findLoadBalancerIP(remoteCluster, "federation-controller-lb", "istio-system")
			if err != nil {
				return fmt.Errorf("could not get IPs from remote federation-controller: %v", err)
			}
		}
		if err := localCluster.ApplyYAMLContents("istio-system", fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: federation-controller
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: federation-controller
  template:
    metadata:
      labels:
        app.kubernetes.io/name: federation-controller
    spec:
      serviceAccount: federation-controller
      containers:
      - name: server
        image: quay.io/jewertow/federation-controller:latest
        args:
        - --meshPeers
        - '{"remote":{"dataPlane":{"addresses":["%s"],"port":15443},"discovery":{"addresses":["%s"],"port":15020}}}'
        - --exportedServiceSet
        - '{"rules":[{"type":"LabelSelector","labelSelectors":[{"matchLabels":{"export-service":"true"}}]}]}'
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        ports:
        - name: grpc-mcp
          containerPort: 15010
        - name: grpc-fds
          containerPort: 15020
`, dataPlaneIP, discoveryIP)); err != nil {
			return fmt.Errorf("failed to patch federation-controller: %v", err)
		}
	}
	return nil
}

func findLoadBalancerIP(c cluster.Cluster, name, ns string) (string, error) {
	dataplaneGateway, err := c.Kube().CoreV1().Services(ns).Get(context.TODO(), name, v1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get %s/%s service from cluster %s: %v", name, ns, c.Name(), err)
	}
	for _, ip := range dataplaneGateway.Status.LoadBalancer.Ingress {
		if ip.IP != "" {
			return ip.IP, nil
		}
	}
	return "", fmt.Errorf("no load balancer IP found for service %s/%s in cluster %s", name, ns, c.Name())
}

func deployApps(apps *echo.Instances, targetClusterName string, ns namespace.Getter, names ...string) func(t resource.Context) error {
	return func(ctx resource.Context) error {
		targetCluster := ctx.Clusters().GetByName(targetClusterName)
		for _, name := range names {
			newApp, err := deployment.New(ctx).WithClusters(targetCluster).WithConfig(echo.Config{
				Service:   name,
				Namespace: ns.Get(),
				Ports:     ports.All(),
			}).Build()
			if err != nil {
				return fmt.Errorf("failed to create echo: %v", err)
			}
			*apps = apps.Append(newApp)
		}
		return nil
	}
}

func removeServiceFromClusters(name string, ns namespace.Getter, targetClusterNames ...string) func(t resource.Context) error {
	return func(ctx resource.Context) error {
		for _, targetClusterName := range targetClusterNames {
			targetCluster := ctx.Clusters().GetByName(targetClusterName)
			if err := targetCluster.Kube().CoreV1().Services(ns.Get().Name()).Delete(context.TODO(), name, v1.DeleteOptions{}); err != nil {
				return fmt.Errorf("failed to delete Service %s/%s from cluster %s: %v", name, ns.Get().Name(), targetCluster.Name(), err)
			}
		}
		return nil
	}
}
