/*
Copyright 2020 The Kubernetes Authors.

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

package provider

import (
	"context"

	clientset "k8s.io/client-go/kubernetes"
	prom "k8s.io/perf-tests/clusterloader2/pkg/prometheus/clients"
	sshutil "k8s.io/perf-tests/clusterloader2/pkg/util"
)

type VsphereProvider struct {
	features Features
}

func NewVsphereProvider(_ map[string]string) Provider {
	return &VsphereProvider{
		features: Features{
			SupportProbe:                        true,
			SupportSSHToMaster:                  true,
			SupportImagePreload:                 true,
			SupportEnablePrometheusServer:       true,
			SupportGrabMetricsFromKubelets:      true,
			SupportAccessAPIServerPprofEndpoint: true,
			SupportResourceUsageMetering:        true,
			ShouldScrapeKubeProxy:               true,
		},
	}
}

func (p *VsphereProvider) Name() string {
	return VsphereName
}

func (p *VsphereProvider) Features() *Features {
	return &p.features
}

func (p *VsphereProvider) GetComponentProtocolAndPort(componentName string) (string, int, error) {
	return getComponentProtocolAndPort(componentName)
}

func (p *VsphereProvider) GetConfig() Config {
	return Config{}
}

func (p *VsphereProvider) RunSSHCommand(cmd, host string) (string, string, int, error) {
	// skeleton provider takes ssh key from KUBE_SSH_KEY.
	// TODO(mborsz): Migrate to "vsphere" and LOCAL_SSH_KEY?
	r, err := sshutil.SSH(context.Background(), cmd, host, "skeleton")
	return r.Stdout, r.Stderr, r.Code, err
}

func (p *VsphereProvider) Metadata(_ clientset.Interface) (map[string]string, error) {
	return nil, nil
}

func (p *VsphereProvider) GetManagedPrometheusClient() (prom.Client, error) {
	return nil, ErrNoManagedPrometheus
}
