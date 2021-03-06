/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endpoints

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	vcclient "sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/client/clientset/versioned"
	vcinformers "sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/client/informers/externalversions/tenancy/v1alpha1"
	"sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/syncer/apis/config"
	"sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/syncer/constants"
	"sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/syncer/manager"
	pa "sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/syncer/patrol"
	uw "sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/syncer/uwcontroller"
	"sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/util/listener"
	mc "sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/util/mccontroller"
)

type controller struct {
	config *config.SyncerConfiguration
	// super master endpoints client
	endpointClient v1core.EndpointsGetter
	// super master endpoints informer lister/synced function
	endpointsLister listersv1.EndpointsLister
	endpointsSynced cache.InformerSynced
	// Connect to all tenant master endpoints informers
	multiClusterEndpointsController *mc.MultiClusterController
	// Periodic checker
	endPointsPatroller *pa.Patroller
}

func NewEndpointsController(config *config.SyncerConfiguration,
	client clientset.Interface,
	informer informers.SharedInformerFactory,
	vcClient vcclient.Interface,
	vcInformer vcinformers.VirtualClusterInformer,
	options manager.ResourceSyncerOptions) (manager.ResourceSyncer, *mc.MultiClusterController, *uw.UpwardController, error) {
	c := &controller{
		config:         config,
		endpointClient: client.CoreV1(),
	}

	multiClusterEndpointsController, err := mc.NewMCController(&v1.Endpoints{}, &v1.EndpointsList{}, c,
		mc.WithMaxConcurrentReconciles(constants.DwsControllerWorkerLow), mc.WithOptions(options.MCOptions))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create endpoints mc controller: %v", err)
	}
	c.multiClusterEndpointsController = multiClusterEndpointsController

	c.endpointsLister = informer.Core().V1().Endpoints().Lister()
	if options.IsFake {
		c.endpointsSynced = func() bool { return true }
	} else {
		c.endpointsSynced = informer.Core().V1().Endpoints().Informer().HasSynced
	}

	endPointsPatroller, err := pa.NewPatroller(&v1.Endpoints{}, c, pa.WithOptions(options.PatrolOptions))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create endpoints patroller: %v", err)
	}
	c.endPointsPatroller = endPointsPatroller

	return c, multiClusterEndpointsController, nil, nil
}

func (c *controller) StartUWS(stopCh <-chan struct{}) error {
	return nil
}

func (c *controller) BackPopulate(string) error {
	return nil
}

func (c *controller) GetListener() listener.ClusterChangeListener {
	return listener.NewMCControllerListener(c.multiClusterEndpointsController)
}
