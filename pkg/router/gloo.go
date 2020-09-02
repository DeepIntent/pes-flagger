package router

import (
	"context"
	"fmt"

	gloov1 "github.com/weaveworks/flagger/pkg/apis/gloo/v1"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	flaggerv1 "github.com/weaveworks/flagger/pkg/apis/flagger/v1beta1"
	clientset "github.com/weaveworks/flagger/pkg/client/clientset/versioned"
)

// GlooRouter is managing Istio virtual services
type GlooRouter struct {
	kubeClient          kubernetes.Interface
	glooClient          clientset.Interface
	flaggerClient       clientset.Interface
	logger              *zap.SugaredLogger
	upstreamDiscoveryNs string
}

// Reconcile creates or updates the Istio virtual service
func (gr *GlooRouter) Reconcile(canary *flaggerv1.Canary) error {
	apexName, _, _ := canary.GetServiceNames()
	canaryName := fmt.Sprintf("%s-canary-%v", apexName, canary.Spec.Service.Port)

	upstreamGroup, err := gr.glooClient.GlooV1().UpstreamGroups(canary.Namespace).Get(context.TODO(), apexName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return fmt.Errorf("UpstreamGroup %s.%s not found", apexName, canary.Namespace)
	} else if err != nil {
		return fmt.Errorf("UpstreamGroup %s.%s get query error: %w", apexName, canary.Namespace, err)
	}

	var newDestinations []gloov1.WeightedDestination
	var canaryDestination *gloov1.WeightedDestination

	for _, dst := range upstreamGroup.Spec.Destinations {
		if dst.Destination.Upstream.Name == canaryName {
			canaryDestination = &dst
			break
		}
	}

	if canaryDestination == nil {
		newDestinations = append(upstreamGroup.Spec.Destinations,
			gloov1.WeightedDestination{
				Destination: gloov1.Destination{
					Upstream: gloov1.ResourceRef{
						Name:      canaryName,
						Namespace: canary.Namespace,
					},
				},
				Weight: uint32(0),
			})
	} else {
		newDestinations = make([]gloov1.WeightedDestination, len(upstreamGroup.Spec.Destinations)+1)
		for i, dst := range upstreamGroup.Spec.Destinations {
			newDestinations = append(newDestinations, dst)
			if dst.Destination.Upstream.Name == canaryName {
				newDestinations[i].Weight = uint32(0)
			}
		}
	}

	clone := upstreamGroup.DeepCopy()
	clone.Spec.Destinations = newDestinations

	_, err = gr.glooClient.GlooV1().UpstreamGroups(canary.Namespace).Update(context.TODO(), clone, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("UpstreamGroup %s.%s update error: %w", apexName, canary.Namespace, err)
	}
	gr.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
		Infof("UpstreamGroup %s.%s updated", upstreamGroup.GetName(), canary.Namespace)

	return nil
}

// GetRoutes returns the destinations weight for primary and canary
func (gr *GlooRouter) GetRoutes(canary *flaggerv1.Canary) (
	primaryWeight int,
	canaryWeight int,
	mirrored bool,
	err error,
) {
	apexName := canary.Spec.TargetRef.Name
	primaryName := fmt.Sprintf("%s-%v", canary.Spec.TargetRef.Name, canary.Spec.Service.Port)

	upstreamGroup, err := gr.glooClient.GlooV1().UpstreamGroups(canary.Namespace).Get(context.TODO(), apexName, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("UpstreamGroup %s.%s get query error: %w", apexName, canary.Namespace, err)
		return
	}

	if len(upstreamGroup.Spec.Destinations) < 2 {
		err = fmt.Errorf("UpstreamGroup %s.%s destinations not found", apexName, canary.Namespace)
		return
	}

	for _, dst := range upstreamGroup.Spec.Destinations {
		if dst.Destination.Upstream.Name == primaryName {
			primaryWeight = int(dst.Weight) / 10 //Since we use 1000 as base value and flagger use 100
			canaryWeight = 100 - primaryWeight
			return
		}
	}

	return
}

// SetRoutes updates the destinations weight for primary and canary
func (gr *GlooRouter) SetRoutes(
	canary *flaggerv1.Canary,
	primaryWeight int,
	canaryWeight int,
	_ bool,
) error {
	apexName, _, _ := canary.GetServiceNames()
	canaryName := fmt.Sprintf("%s-canary-%v", apexName, canary.Spec.Service.Port)
	if primaryWeight == 0 && canaryWeight == 0 {
		return fmt.Errorf("RoutingRule %s.%s update failed: no valid weights", apexName, canary.Namespace)
	}

	upstreamGroup, err := gr.glooClient.GlooV1().UpstreamGroups(canary.Namespace).Get(context.TODO(), apexName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("UpstreamGroup %s.%s query error: %w", apexName, canary.Namespace, err)
	}

	for _, dst := range upstreamGroup.Spec.Destinations {
		if dst.Destination.Upstream.Name == canaryName {
			dst.Weight = uint32(canaryWeight * 10) //Since we use 1000 as base value and flagger use 100
			break
		}
	}

	_, err = gr.glooClient.GlooV1().UpstreamGroups(canary.Namespace).Update(context.TODO(), upstreamGroup, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("UpstreamGroup %s.%s update error: %w", apexName, canary.Namespace, err)
	}
	return nil
}

func (gr *GlooRouter) Finalize(canary *flaggerv1.Canary) error {
	apexName, _, _ := canary.GetServiceNames()
	canaryName := fmt.Sprintf("%s-canary-%v", apexName, canary.Spec.Service.Port)

	upstreamGroup, err := gr.glooClient.GlooV1().UpstreamGroups(canary.Namespace).Get(context.TODO(), apexName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("UpstreamGroup %s.%s query error: %w", apexName, canary.Namespace, err)
	}

	var idx int
	for i, dst := range upstreamGroup.Spec.Destinations {
		if dst.Destination.Upstream.Name == canaryName {
			idx = i
			break
		}
	}

	upstreamGroup.Spec.Destinations = remove(upstreamGroup.Spec.Destinations, idx)

	_, err = gr.glooClient.GlooV1().UpstreamGroups(canary.Namespace).Update(context.TODO(), upstreamGroup, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("UpstreamGroup %s.%s update error: %w", apexName, canary.Namespace, err)
	}
	return nil
}

func remove(s [] gloov1.WeightedDestination, i int) []gloov1.WeightedDestination {
	s[i] = s[len(s)-1]
	return s[:len(s)-1]
}
